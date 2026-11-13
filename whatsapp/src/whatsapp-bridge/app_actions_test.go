package main

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// All made up. Nothing here touches WhatsApp.

func (h *pictureHarness) ownMessage(t *testing.T, chat, id, text string, at time.Time) {
	t.Helper()
	if err := h.app.recordSent(jidOf(t, chat), textMsg(text), whatsmeow.SendResponse{ID: id, Timestamp: at}); err != nil {
		t.Fatal(err)
	}
}

func (h *pictureHarness) sentCount() int {
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	return len(h.wa.sent)
}

func TestEditingOwnTextSendsAnEditAndStoresIt(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	h.ownMessage(t, photoGroup, "m1", "first try", time.Now().Add(-time.Minute))
	before, _ := h.store.GetMessage("m1", photoGroup)
	var out map[string]any
	if code := h.post("/api/app/edit", map[string]any{"chat": photoGroup, "id": "m1", "text": "Hi @777777777", "mentions": []string{dana}}, &out); code != 200 {
		t.Fatalf("edit: %d %v", code, out)
	}
	proto := h.sentMessages()[0].GetEditedMessage().GetMessage().GetProtocolMessage()
	if proto.GetType() != waE2E.ProtocolMessage_MESSAGE_EDIT || proto.GetKey().GetID() != "m1" {
		t.Fatalf("not an edit of m1: %v", proto)
	}
	edited := proto.GetEditedMessage().GetExtendedTextMessage()
	if edited.GetText() != "Hi @777777777" || len(edited.GetContextInfo().GetMentionedJID()) != 1 {
		t.Fatalf("mentions must survive an edit: %v", edited)
	}
	after, _ := h.store.GetMessage("m1", photoGroup)
	if after.Text != "Hi @777777777" || after.EditedMs == 0 || after.Seq <= before.Seq {
		t.Fatalf("edit not stored: %+v", after)
	}
}

func TestEditsThatWhatsAppWouldRefuseSendNothing(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.ownMessage(t, photoGroup, "old", "too late", time.Now().Add(-16*time.Minute))
	h.ownMessage(t, photoGroup, "fresh", "fine", time.Now())
	h.store.Ingest(incoming(t, photoGroup, dana, "theirs", time.Now(), textMsg("not yours")), "G", "")
	h.store.Ingest(&events.Message{Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("pic"), Mimetype: proto.String("image/jpeg")}},
		Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: jidOf(t, photoGroup), Sender: h.wa.OwnSender(jidOf(t, photoGroup)), IsFromMe: true}, ID: "media", Timestamp: time.Now()}}, "G", "sent")
	cases := []struct {
		id, text string
		code     int
	}{
		{"old", "x", 409}, {"theirs", "x", 409}, {"media", "x", 409}, {"fresh", "   ", 400}, {"missing", "x", 404},
	}
	for _, c := range cases {
		if code := h.post("/api/app/edit", map[string]any{"chat": photoGroup, "id": c.id, "text": c.text}, nil); code != c.code {
			t.Errorf("%s: got %d, want %d", c.id, code, c.code)
		}
	}
	if n := h.sentCount(); n != 0 {
		t.Fatalf("%d sent", n)
	}
}

func TestDeleteForEveryoneOnlyForOwnRecentMessages(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.ownMessage(t, photoGroup, "mine", "oops", time.Now().Add(-time.Hour))
	h.ownMessage(t, photoGroup, "ancient", "old", time.Now().Add(-61*time.Hour))
	h.store.Ingest(incoming(t, photoGroup, dana, "theirs", time.Now(), textMsg("not yours")), "G", "")
	if code := h.post("/api/app/revoke", map[string]any{"chat": photoGroup, "id": "theirs"}, nil); code != 409 {
		t.Fatalf("someone else's: %d", code)
	}
	if code := h.post("/api/app/revoke", map[string]any{"chat": photoGroup, "id": "ancient"}, nil); code != 409 {
		t.Fatalf("too old: %d", code)
	}
	if code := h.post("/api/app/revoke", map[string]any{"chat": photoGroup, "id": "mine"}, nil); code != 200 {
		t.Fatalf("own: %d", code)
	}
	p := h.sentMessages()[0].GetProtocolMessage()
	if p.GetType() != waE2E.ProtocolMessage_REVOKE || p.GetKey().GetID() != "mine" {
		t.Fatalf("not a revoke: %v", p)
	}
	m, _ := h.store.GetMessage("mine", photoGroup)
	if !m.Revoked || m.Text != "" {
		t.Fatalf("not stored as deleted: %+v", m)
	}
	if code := h.post("/api/app/revoke", map[string]any{"chat": photoGroup, "id": "mine"}, nil); code != 200 || h.sentCount() != 1 {
		t.Fatalf("a second delete must not send again: %d, %d sent", code, h.sentCount())
	}
}

func TestDeleteForMeHidesEverywhereAndSendsNothing(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "m1", time.Now(), textMsg("hide me")), "G", "")
	before, _ := h.store.GetMessage("m1", photoGroup)
	if code := h.post("/api/app/hide", map[string]any{"chat": photoGroup, "id": "m1"}, nil); code != 200 {
		t.Fatalf("hide: %d", code)
	}
	after, _ := h.store.GetMessage("m1", photoGroup)
	if !after.Hidden || after.Seq <= before.Seq || after.Text != "hide me" {
		t.Fatalf("not hidden with a new sequence number, or text lost: %+v", after)
	}
	page, _ := h.store.Sync(before.Seq, 100)
	if len(page.Messages) != 1 || !page.Messages[0].Hidden {
		t.Fatalf("PCs must get the change: %+v", page.Messages)
	}
	if code := h.post("/api/app/hide", map[string]any{"chat": photoGroup, "id": "m1"}, nil); code != 200 {
		t.Fatalf("again: %d", code)
	}
	again, _ := h.store.GetMessage("m1", photoGroup)
	if again.Seq != after.Seq || h.sentCount() != 0 {
		t.Fatalf("hiding twice must change nothing and nothing is sent: %d %d", again.Seq, h.sentCount())
	}
	var content string
	h.db.QueryRow(`SELECT content FROM messages WHERE id = 'm1'`).Scan(&content)
	if content != "hide me" {
		t.Fatal("the MCP server's view must be unchanged")
	}
}

func TestADeleteForMeFromThePhoneHidesItToo(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "m1", time.Now(), textMsg("phone hid me")), "G", "")
	h.app.handleEvent(&events.DeleteForMe{ChatJID: jidOf(t, photoGroup), SenderJID: jidOf(t, dana), MessageID: "m1", Timestamp: time.Now()})
	h.app.handleEvent(&events.DeleteForMe{ChatJID: jidOf(t, photoGroup), MessageID: "unknown", Timestamp: time.Now()})
	m, _ := h.store.GetMessage("m1", photoGroup)
	if !m.Hidden {
		t.Fatal("not hidden")
	}
}

func TestAnOldStoreGetsTheHiddenColumn(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(dir, "m.db"))+"?_foreign_keys=on")
	defer db.Close()
	createLegacyTables(t, db)
	old := strings.Replace(appSchema, "reactions TEXT NOT NULL DEFAULT '',", "reactions TEXT NOT NULL DEFAULT '',", 1)
	if _, err := db.Exec(old); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAppStore(db, NewHub()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewAppStore(db, NewHub()); err != nil {
		t.Fatalf("a second start must not fail: %v", err)
	}
	var n int
	db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('app_msg') WHERE name = 'hidden'`).Scan(&n)
	if n != 1 {
		t.Fatal("hidden column missing")
	}
}

func TestAGroupReplyTakesItsSenderFromTheStore(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "q1", time.Now(), textMsg("theirs")), "G", "")
	h.ownMessage(t, photoGroup, "q2", "mine", time.Now())
	// The app says someone else wrote it. The store knows better.
	if code, _ := h.send(t, "c1", photoGroup, "reply", nil, map[string]any{"id": "q1", "sender": robin, "text": "x"}); code != 200 {
		t.Fatalf("send: %d", code)
	}
	if c := h.sentMessages()[0].GetExtendedTextMessage().GetContextInfo(); c.GetParticipant() != dana || c.GetQuotedMessage().GetConversation() != "theirs" {
		t.Fatalf("quote %v", c)
	}
	if code, _ := h.send(t, "c2", photoGroup, "reply to me", nil, map[string]any{"id": "q2", "sender": "", "text": "mine"}); code != 200 {
		t.Fatalf("send: %d", code)
	}
	if c := h.sentMessages()[1].GetExtendedTextMessage().GetContextInfo(); c.GetParticipant() != "15559990000@s.whatsapp.net" {
		t.Fatalf("own quote must name the user: %v", c)
	}
}

func TestAGroupReplyWithNoKnownSenderIsRefused(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "grey", time.Now(), textMsg("old")), "G", "")
	h.db.Exec(`UPDATE app_msg SET sender_jid = '' WHERE id = 'grey'`)
	for _, quote := range []map[string]any{{"id": "grey", "sender": "", "text": "old"}, {"id": "never-stored", "sender": dana, "text": "x"}} {
		code, out := h.send(t, "c-"+quote["id"].(string), photoGroup, "reply", nil, quote)
		if code != 409 || !strings.Contains(out["error"].(string), "without the reply") {
			t.Fatalf("%v: %d %v", quote["id"], code, out)
		}
	}
	if h.sentCount() != 0 {
		t.Fatal("nothing may be sent with a quote that names no one")
	}
	// One-to-one chats need no sender for a quote.
	if code, _ := h.send(t, "c3", testDM, "reply", nil, map[string]any{"id": "nowhere", "sender": "", "text": "x"}); code != 200 {
		t.Fatalf("one-to-one: %d", code)
	}
}

func TestAReactionUsesTheStoredSender(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "r1", time.Now(), textMsg("x")), "G", "")
	h.store.Ingest(incoming(t, photoGroup, dana, "grey", time.Now(), textMsg("y")), "G", "")
	h.db.Exec(`UPDATE app_msg SET sender_jid = '' WHERE id = 'grey'`)
	if code := h.post("/api/app/react", map[string]any{"chat": photoGroup, "id": "grey", "sender_jid": "", "emoji": "👍"}, nil); code != 409 {
		t.Fatalf("no sender: %d", code)
	}
	if h.sentCount() != 0 {
		t.Fatal("sent anyway")
	}
	if code := h.post("/api/app/react", map[string]any{"chat": photoGroup, "id": "r1", "sender_jid": robin, "emoji": "👍"}, nil); code != 200 {
		t.Fatalf("react: %d", code)
	}
}

func waitForward(t *testing.T, sub chan []byte) map[string]any {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case data := <-sub:
			var evt map[string]any
			if json.Unmarshal(data, &evt) == nil && evt["type"] == "forward_result" {
				return evt
			}
		case <-deadline:
			t.Fatal("no forward result")
		}
	}
}

func TestForwardingTextToSeveralChats(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "f1", time.Now(), &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
		Text: proto.String("Pass this on @777777777"), ContextInfo: &waE2E.ContextInfo{MentionedJID: []string{dana}}}}), "G", "")
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	targets := []string{testDM, "15550000002@s.whatsapp.net", testDM}
	if code := h.post("/api/app/forward", map[string]any{"chat": photoGroup, "id": "f1", "to": targets, "client_id": "fw1"}, nil); code != 202 {
		t.Fatalf("forward: %d", code)
	}
	res := waitForward(t, sub)
	if sent := res["sent"].([]any); len(sent) != 2 || res["ok"] != true {
		t.Fatalf("result %v", res)
	}
	for _, m := range h.sentMessages() {
		ext := m.GetExtendedTextMessage()
		if ext.GetText() != "Pass this on @777777777" || !ext.GetContextInfo().GetIsForwarded() || len(ext.GetContextInfo().GetMentionedJID()) != 0 {
			t.Fatalf("forwarded message %v", ext)
		}
	}
	if h.sentCount() != 2 {
		t.Fatalf("a chat named twice must get one copy: %d", h.sentCount())
	}
	if code := h.post("/api/app/forward", map[string]any{"chat": photoGroup, "id": "f1", "to": targets, "client_id": "fw1"}, nil); code != 200 || h.sentCount() != 2 {
		t.Fatalf("a repeated request must not forward again: %d %d", code, h.sentCount())
	}
}

func TestForwardRefusals(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "ok", time.Now(), textMsg("x")), "G", "")
	h.store.Ingest(incoming(t, photoGroup, dana, "gone", time.Now(), textMsg("y")), "G", "")
	h.db.Exec(`UPDATE app_msg SET revoked = 1 WHERE id = 'gone'`)
	six := []string{}
	for i := 0; i < 6; i++ {
		six = append(six, "1555000010"+string(rune('0'+i))+"@s.whatsapp.net")
	}
	cases := []struct {
		id   string
		to   []string
		code int
	}{
		{"ok", six, 400}, {"ok", nil, 400}, {"ok", []string{"not a chat"}, 400}, {"gone", []string{testDM}, 409}, {"missing", []string{testDM}, 404},
	}
	for _, c := range cases {
		if code := h.post("/api/app/forward", map[string]any{"chat": photoGroup, "id": c.id, "to": c.to}, nil); code != c.code {
			t.Errorf("%s %v: got %d, want %d", c.id, c.to, code, c.code)
		}
	}
	time.Sleep(100 * time.Millisecond)
	if h.sentCount() != 0 {
		t.Fatal("sent anyway")
	}
}

func TestForwardLimitNamesTheChatsItStopped(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	h.store.Ingest(incoming(t, photoGroup, dana, "f1", time.Now(), textMsg("x")), "G", "")
	tries := []int64{}
	for i := 0; i < forwardPerHour-1; i++ {
		tries = append(tries, time.Now().Add(-time.Minute).Unix())
	}
	raw, _ := json.Marshal(tries)
	os.WriteFile(filepath.Join(h.app.pictures.dir, "forward-tries.json"), raw, 0o600)
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	to := []string{testDM, "15550000002@s.whatsapp.net", "15550000003@s.whatsapp.net"}
	var out map[string]any
	if code := h.post("/api/app/forward", map[string]any{"chat": photoGroup, "id": "f1", "to": to}, &out); code != 202 {
		t.Fatalf("forward: %d", code)
	}
	res := waitForward(t, sub)
	if len(res["sent"].([]any)) != 1 || len(res["limited"].([]any)) != 2 || res["ok"] != false {
		t.Fatalf("result %v", res)
	}
	if res["limited"].([]any)[0] != "15550000002@s.whatsapp.net" {
		t.Fatalf("the limited chats must be named: %v", res["limited"])
	}
}

func TestForwardingAFileUploadsItOnce(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false), nil)
	img := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("made-up photo"), Mimetype: proto.String("image/png"), URL: proto.String("https://mmg.example/a"),
		DirectPath: proto.String("/a"), MediaKey: []byte("k"), FileEncSHA256: []byte("e"), FileSHA256: []byte("s"), FileLength: proto.Uint64(8),
	}}
	h.store.Ingest(incoming(t, photoGroup, dana, "pic", time.Now(), img), "G", "")
	path := filepath.Join(h.app.mediaDir, mediaFileName(photoGroup, "pic"))
	os.WriteFile(path, []byte{137, 80, 78, 71, 13, 10, 26, 10}, 0o600)
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	if code := h.post("/api/app/forward", map[string]any{"chat": photoGroup, "id": "pic", "to": []string{testDM, "15550000002@s.whatsapp.net"}}, nil); code != 202 {
		t.Fatalf("forward: %d", code)
	}
	res := waitForward(t, sub)
	if len(res["sent"].([]any)) != 2 {
		t.Fatalf("result %v", res)
	}
	if h.wa.uploads != 1 {
		t.Fatalf("uploaded %d times", h.wa.uploads)
	}
	for _, m := range h.sentMessages() {
		im := m.GetImageMessage()
		if im.GetCaption() != "made-up photo" || !im.GetContextInfo().GetIsForwarded() {
			t.Fatalf("forwarded file %v", im)
		}
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("the cached file must stay")
	}
}
