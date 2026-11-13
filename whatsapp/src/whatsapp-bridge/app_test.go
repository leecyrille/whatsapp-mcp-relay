package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	testDM    = "15550000001@s.whatsapp.net"
	testGroup = "120363000000000001@g.us"
	testLID   = "99999@lid"
)

// fakeWA records what would have gone out. Nothing in this file touches WhatsApp.
type fakeWA struct {
	mu         sync.Mutex
	sent       []*waE2E.Message
	sentTo     []types.JID
	read       [][]string
	readSender []types.JID
	readQuiet  []bool
	sendErr    error
	download   func(d whatsmeow.DownloadableMessage, f whatsmeow.File) error
	retried    []string
	history    int
	nextID     int

	contacts    map[types.JID]types.ContactInfo
	createCalls []whatsmeow.ReqCreateGroup
	createErr   error
	createReply func(whatsmeow.ReqCreateGroup) *types.GroupInfo
	renames     []string
	leaves      []string
	adminErr    error

	groupInfo      *types.GroupInfo
	groupInfoErr   error
	groupInfoCalls int
	photoCalls     [][]byte
	photoErr       error

	picInfo      *types.ProfilePictureInfo
	picErr       error
	picCalls     []string
	lastExisting string
	uploads      int
}

func (f *fakeWA) ProfilePicture(ctx context.Context, jid types.JID, existingID string) (*types.ProfilePictureInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.picCalls = append(f.picCalls, jid.String())
	f.lastExisting = existingID
	return f.picInfo, f.picErr
}

func (f *fakeWA) GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groupInfoCalls++
	return f.groupInfo, f.groupInfoErr
}

func (f *fakeWA) SetGroupPhoto(ctx context.Context, jid types.JID, photo []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.photoCalls = append(f.photoCalls, photo)
	if f.photoErr != nil {
		return "", f.photoErr
	}
	return "photo-id", nil
}

func (f *fakeWA) CreateGroup(ctx context.Context, req whatsmeow.ReqCreateGroup) (*types.GroupInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createCalls = append(f.createCalls, req)
	if f.createErr != nil {
		return nil, f.createErr
	}
	if f.createReply != nil {
		return f.createReply(req), nil
	}
	info := &types.GroupInfo{JID: types.NewJID("1203630000000"+strconv.Itoa(10000+len(f.createCalls)), types.GroupServer)}
	for _, m := range req.Participants {
		info.Participants = append(info.Participants, types.GroupParticipant{JID: m})
	}
	return info, nil
}

func (f *fakeWA) SetGroupName(ctx context.Context, jid types.JID, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.renames = append(f.renames, name)
	return f.adminErr
}

func (f *fakeWA) LeaveGroup(ctx context.Context, jid types.JID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.leaves = append(f.leaves, jid.String())
	return f.adminErr
}

func (f *fakeWA) Connected() bool       { return true }
func (f *fakeWA) LoggedIn() bool        { return true }
func (f *fakeWA) Own() (string, string) { return "15559990000@s.whatsapp.net", "11111@lid" }
func (f *fakeWA) OwnSender(chat types.JID) types.JID {
	return types.NewJID("15559990000", types.DefaultUserServer)
}
func (f *fakeWA) Send(ctx context.Context, to types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return whatsmeow.SendResponse{}, f.sendErr
	}
	f.sent = append(f.sent, msg)
	f.sentTo = append(f.sentTo, to)
	f.nextID++
	return whatsmeow.SendResponse{ID: "SENT" + string(rune('A'+f.nextID)), Timestamp: time.Now()}, nil
}
func (f *fakeWA) MarkRead(ctx context.Context, ids []string, chat, sender types.JID, quiet bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.read = append(f.read, ids)
	f.readQuiet = append(f.readQuiet, quiet)
	f.readSender = append(f.readSender, sender)
	return nil
}
func (f *fakeWA) Contacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	f.mu.Lock()
	if f.contacts != nil {
		defer f.mu.Unlock()
		return f.contacts, nil
	}
	f.mu.Unlock()
	return map[types.JID]types.ContactInfo{
		types.NewJID("15550000001", types.DefaultUserServer): {FullName: "Sample Person"},
	}, nil
}
func (f *fakeWA) Aliases(ctx context.Context) ([][2]string, error) {
	return [][2]string{{testLID, testDM}}, nil
}
func (f *fakeWA) Upload(ctx context.Context, r io.Reader, t whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	data, _ := io.ReadAll(r)
	f.mu.Lock()
	f.uploads++
	f.mu.Unlock()
	return whatsmeow.UploadResponse{
		URL: "https://mmg.example/x", DirectPath: "/x", MediaKey: []byte("k"), FileEncSHA256: []byte("e"),
		FileSHA256: []byte("s"), FileLength: uint64(len(data)),
	}, nil
}
func (f *fakeWA) DownloadToFile(ctx context.Context, d whatsmeow.DownloadableMessage, file whatsmeow.File) error {
	return f.download(d, file)
}
func (f *fakeWA) Canon(jid string) string {
	if jid == testLID {
		return testDM
	}
	return jid
}
func (f *fakeWA) RetryMedia(ctx context.Context, info *types.MessageInfo, key []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.retried = append(f.retried, info.ID)
	return nil
}
func (f *fakeWA) RequestHistory(ctx context.Context, info *types.MessageInfo, count int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.history++
	return nil
}
func (f *fakeWA) BuildReaction(chat, sender types.JID, id, emoji string) *waE2E.Message {
	return &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key:  &waCommon.MessageKey{ID: proto.String(id), RemoteJID: proto.String(chat.String())},
		Text: proto.String(emoji),
	}}
}
func (f *fakeWA) ChatName(jid types.JID, sender string) string { return "Named " + jid.User }
func (f *fakeWA) BuildEdit(chat types.JID, id string, content *waE2E.Message) *waE2E.Message {
	return &waE2E.Message{EditedMessage: &waE2E.FutureProofMessage{Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:  &waCommon.MessageKey{FromMe: proto.Bool(true), ID: proto.String(id), RemoteJID: proto.String(chat.String())},
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), EditedMessage: content,
	}}}}
}
func (f *fakeWA) BuildRevoke(chat, sender types.JID, id string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Key:  &waCommon.MessageKey{FromMe: proto.Bool(true), ID: proto.String(id), RemoteJID: proto.String(chat.String())},
		Type: waE2E.ProtocolMessage_REVOKE.Enum(),
	}}
}

type harness struct {
	t     *testing.T
	db    *sql.DB
	store *AppStore
	app   *App
	wa    *fakeWA
	hub   *Hub
	srv   *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(dir, "m.db"))+"?_foreign_keys=on&_journal_mode=WAL&_busy_timeout=10000")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	createLegacyTables(t, db)
	return startHarness(t, db, dir)
}

func createLegacyTables(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS chats (jid TEXT PRIMARY KEY, name TEXT, last_message_time TIMESTAMP);
		CREATE TABLE IF NOT EXISTS messages (id TEXT, chat_jid TEXT, sender TEXT, content TEXT, timestamp TIMESTAMP,
			is_from_me BOOLEAN, media_type TEXT, filename TEXT, url TEXT, media_key BLOB, file_sha256 BLOB,
			file_enc_sha256 BLOB, file_length INTEGER, PRIMARY KEY (id, chat_jid), FOREIGN KEY (chat_jid) REFERENCES chats(jid));`); err != nil {
		t.Fatal(err)
	}
}

func startHarness(t *testing.T, db *sql.DB, dir string) *harness {
	t.Helper()
	hub := NewHub()
	store, err := NewAppStore(db, hub)
	if err != nil {
		t.Fatal(err)
	}
	wa := &fakeWA{}
	app, err := NewApp(store, wa, hub, dir)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	app.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, db: db, store: store, app: app, wa: wa, hub: hub, srv: srv}
}

func jidOf(t *testing.T, s string) types.JID {
	t.Helper()
	j, err := types.ParseJID(s)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func incoming(t *testing.T, chat, sender, id string, ts time.Time, msg *waE2E.Message) *events.Message {
	t.Helper()
	return &events.Message{Message: msg, Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jidOf(t, chat), Sender: jidOf(t, sender), IsGroup: strings.HasSuffix(chat, "@g.us")},
		ID:            id, Timestamp: ts, PushName: "Pusher",
	}}
}

func textMsg(s string) *waE2E.Message { return &waE2E.Message{Conversation: proto.String(s)} }

func (h *harness) getJSON(path string, into any) int {
	h.t.Helper()
	resp, err := http.Get(h.srv.URL + path)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func (h *harness) post(path string, body any, into any) int {
	h.t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(h.srv.URL+path, "application/json", bytes.NewReader(b))
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func TestHistoryRowsWrittenBeforeUpgradeGetSequenceNumbers(t *testing.T) {
	dir := t.TempDir()
	db, _ := sql.Open("sqlite3", "file:"+filepath.ToSlash(filepath.Join(dir, "m.db"))+"?_foreign_keys=on")
	defer db.Close()
	createLegacyTables(t, db)
	old := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	db.Exec(`INSERT INTO chats VALUES (?, 'Old', ?)`, testDM, old)
	db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES ('m1', ?, '1', 'hi', ?, 0, '', '')`, testDM, old)
	h := startHarness(t, db, dir)
	page, err := h.store.Sync(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 1 || page.Messages[0].Text != "hi" || len(page.Chats) != 1 {
		t.Fatalf("got %+v", page)
	}
	if page.Chats[0].LastReadMs == 0 {
		t.Fatal("existing chats must start as read, or years of history show as unread")
	}
	// A second start must add nothing.
	before := h.store.Head()
	if err := h.store.backfill(); err != nil || h.store.Head() != before {
		t.Fatalf("backfill not idempotent: %v %d %d", err, before, h.store.Head())
	}
}

func TestCaptionsMediaAndQuotesAreKept(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Caption: proto.String("look"), Mimetype: proto.String("image/png"), URL: proto.String("https://mmg.example/a?x=1"),
		DirectPath: proto.String("/a?x=1"), MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"),
		FileLength: proto.Uint64(10), JPEGThumbnail: []byte("thumb"), Width: proto.Uint32(4), Height: proto.Uint32(3),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("orig"), Participant: proto.String(testDM),
			QuotedMessage: textMsg("earlier")},
	}}
	if err := h.store.Ingest(incoming(t, testGroup, testDM, "img1", now, msg), "Group", ""); err != nil {
		t.Fatal(err)
	}
	m, err := h.store.GetMessage("img1", testGroup)
	if err != nil {
		t.Fatal(err)
	}
	if m.Text != "look" || m.Media != "image" || m.Mime != "image/png" || string(m.Thumb) != "thumb" || m.Width != 4 {
		t.Fatalf("media fields lost: %+v", m)
	}
	if m.QuotedID != "orig" || m.QuotedText != "earlier" || m.Filename != "image_img1.png" {
		t.Fatalf("quote or filename wrong: %+v", m)
	}
	if m.Sender != "15550000001" || m.SenderJID != testDM {
		t.Fatalf("group sender must be the person, not the group: %+v", m)
	}
}

func TestMCPReadQueryStillWorksAfterSend(t *testing.T) {
	// The exact join the MCP server's whatsapp.py runs. Sent messages must appear in it.
	h := newHarness(t)
	var resp struct {
		OK bool `json:"ok"`
	}
	if code := h.post("/api/app/send", map[string]any{"chat": testDM, "text": "from the app"}, &resp); code != 200 || !resp.OK {
		t.Fatalf("send: %d", code)
	}
	var content string
	var fromMe bool
	err := h.db.QueryRow(`SELECT messages.content, messages.is_from_me FROM messages
		JOIN chats ON messages.chat_jid = chats.jid WHERE chats.jid = ? ORDER BY messages.timestamp DESC LIMIT 1`, testDM).Scan(&content, &fromMe)
	if err != nil || content != "from the app" || !fromMe {
		t.Fatalf("sent message missing on read-back: %v %q %v", err, content, fromMe)
	}
}

func TestRecordSentCoversTheMCPSendPath(t *testing.T) {
	h := newHarness(t)
	err := h.app.recordSent(jidOf(t, testDM), textMsg("via mcp"), whatsmeow.SendResponse{ID: "MCP1", Timestamp: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	m, err := h.store.GetMessage("MCP1", testDM)
	if err != nil || m.Text != "via mcp" || !m.FromMe || m.Status != "sent" {
		t.Fatalf("got %+v, %v", m, err)
	}
	c, err := h.store.GetChat(testDM)
	if err != nil || c.Name == "" {
		t.Fatalf("chat not created: %+v, %v", c, err)
	}
}

func TestSendDedupesRetries(t *testing.T) {
	h := newHarness(t)
	body := map[string]any{"chat": testDM, "text": "once", "client_id": "c1"}
	h.post("/api/app/send", body, nil)
	h.post("/api/app/send", body, nil)
	if len(h.wa.sent) != 1 {
		t.Fatalf("a retried request sent %d messages", len(h.wa.sent))
	}
}

func TestFailedSendCanBeRetried(t *testing.T) {
	h := newHarness(t)
	h.wa.sendErr = errors.New("offline")
	body := map[string]any{"chat": testDM, "text": "x", "client_id": "c2"}
	if code := h.post("/api/app/send", body, nil); code != 502 {
		t.Fatalf("got %d", code)
	}
	h.wa.sendErr = nil
	if code := h.post("/api/app/send", body, nil); code != 200 || len(h.wa.sent) != 1 {
		t.Fatalf("retry after failure: %d, sent %d", code, len(h.wa.sent))
	}
}

func TestSendRefusals(t *testing.T) {
	h := newHarness(t)
	for _, chat := range []string{"", "status@broadcast", "1234@broadcast", "garbage", "1@newsletter"} {
		if code := h.post("/api/app/send", map[string]any{"chat": chat, "text": "x"}, nil); code != 400 {
			t.Errorf("chat %q: got %d, want 400", chat, code)
		}
	}
	if code := h.post("/api/app/send", map[string]any{"chat": testDM, "text": "   "}, nil); code != 400 {
		t.Errorf("blank text: got %d", code)
	}
	if len(h.wa.sent) != 0 {
		t.Fatal("something was sent")
	}
}

func TestReactionsEditsAndRevokes(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testDM, testDM, "m1", now, textMsg("hello")), "P", "")

	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("m1")}, Text: proto.String("👍")}}
	h.store.Ingest(incoming(t, testDM, testDM, "r1", now, react), "P", "")
	m, _ := h.store.GetMessage("m1", testDM)
	if !strings.Contains(string(m.Reactions), "👍") {
		t.Fatalf("reaction missing: %s", m.Reactions)
	}
	if _, err := h.store.GetMessage("r1", testDM); err == nil {
		t.Fatal("a reaction must not become its own message")
	}
	h.store.Ingest(incoming(t, testDM, testDM, "r2", now, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("m1")}, Text: proto.String("")}}), "P", "")
	m, _ = h.store.GetMessage("m1", testDM)
	if len(m.Reactions) != 0 {
		t.Fatalf("reaction not removed: %s", m.Reactions)
	}

	edit := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String("m1")}, EditedMessage: textMsg("hello edited"),
	}}
	h.store.Ingest(incoming(t, testDM, testDM, "e1", now, edit), "P", "")
	m, _ = h.store.GetMessage("m1", testDM)
	if m.Text != "hello edited" || m.EditedMs == 0 {
		t.Fatalf("edit not applied: %+v", m)
	}

	revoke := &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String("m1")},
	}}
	h.store.Ingest(incoming(t, testDM, testDM, "d1", now, revoke), "P", "")
	m, _ = h.store.GetMessage("m1", testDM)
	if !m.Revoked || m.Text != "" {
		t.Fatalf("revoke must clear the text: %+v", m)
	}
}

func TestReactionFindsMessageStoredUnderOtherAddress(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testDM, testDM, "m1", now, textMsg("hello")), "P", "")
	react := &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("m1")}, Text: proto.String("❤")}}
	h.store.Ingest(incoming(t, testLID, testLID, "r1", now, react), "P", "")
	m, _ := h.store.GetMessage("m1", testDM)
	if !strings.Contains(string(m.Reactions), "❤") {
		t.Fatalf("got %s", m.Reactions)
	}
}

func TestReceiptsOnlyMoveForward(t *testing.T) {
	h := newHarness(t)
	h.app.recordSent(jidOf(t, testDM), textMsg("x"), whatsmeow.SendResponse{ID: "S1", Timestamp: time.Now()})
	apply := func(rt types.ReceiptType) string {
		err := h.store.ApplyReceipt(&events.Receipt{
			MessageSource: types.MessageSource{Chat: jidOf(t, testDM)}, MessageIDs: []string{"S1"}, Type: rt, Timestamp: time.Now(),
		})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := h.store.GetMessage("S1", testDM)
		return m.Status
	}
	if s := apply(types.ReceiptTypeRead); s != "read" {
		t.Fatalf("got %s", s)
	}
	if s := apply(types.ReceiptTypeDelivered); s != "read" {
		t.Fatalf("delivered after read must not go backwards: %s", s)
	}
}

func TestReadReceiptsGroupBySender(t *testing.T) {
	h := newHarness(t)
	code := h.post("/api/app/read", map[string]any{
		"chat": testGroup, "up_to_ms": 5000, "receipts": true,
		"messages": []map[string]string{
			{"id": "a", "sender_jid": "1@s.whatsapp.net"}, {"id": "b", "sender_jid": "1@s.whatsapp.net"},
			{"id": "c", "sender_jid": "2@s.whatsapp.net"}, {"id": "d", "sender_jid": ""},
		},
	}, nil)
	if code != 200 {
		t.Fatalf("got %d", code)
	}
	if len(h.wa.read) != 2 {
		t.Fatalf("expected one call per sender, and none for a group message with no sender: %v", h.wa.read)
	}
}

func TestReadWithoutReceiptsSendsOnlyReadSelf(t *testing.T) {
	h := newHarness(t)
	h.store.Ingest(incoming(t, testDM, testDM, "m1", time.Now(), textMsg("x")), "P", "")
	h.post("/api/app/read", map[string]any{"chat": testDM, "up_to_ms": time.Now().UnixMilli() + 1000, "receipts": false,
		"messages": []map[string]string{{"id": "m1"}}}, nil)
	// No blue ticks were asked for, but the phone still has to hear it was read.
	if len(h.wa.read) != 1 || !h.wa.readQuiet[0] {
		t.Fatalf("expected one read-self receipt: %v %v", h.wa.read, h.wa.readQuiet)
	}
	c, _ := h.store.GetChat(testDM)
	if c.LastReadMs == 0 {
		t.Fatal("read marker didn't move")
	}
}

func TestSyncPagesWithoutLosingAnything(t *testing.T) {
	h := newHarness(t)
	base := time.Now()
	for i := 0; i < 25; i++ {
		id := "m" + string(rune('a'+i))
		h.store.Ingest(incoming(t, testDM, testDM, id, base.Add(time.Duration(i)*time.Second), textMsg(id)), "P", "")
	}
	seen := map[string]bool{}
	chats := 0
	after := int64(0)
	for pages := 0; pages < 20; pages++ {
		page, err := h.store.Sync(after, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.Messages {
			seen[m.ID] = true
		}
		chats += len(page.Chats)
		after = page.Next
		if !page.More {
			break
		}
	}
	if len(seen) != 25 {
		t.Fatalf("saw %d of 25", len(seen))
	}
	if chats == 0 {
		t.Fatal("chat row never delivered")
	}
	last, _ := h.store.Sync(after, 10)
	if len(last.Messages) != 0 || last.Next != after {
		t.Fatalf("caught-up client got %+v", last)
	}
}

func TestLiveDeltaLinesUpWithSync(t *testing.T) {
	h := newHarness(t)
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	before := h.store.Head()
	h.store.Ingest(incoming(t, testDM, testDM, "m1", time.Now(), textMsg("x")), "P", "")
	var d struct {
		Type     string   `json:"type"`
		Prev     int64    `json:"prev"`
		Seq      int64    `json:"seq"`
		Messages []AppMsg `json:"messages"`
	}
	select {
	case raw := <-sub:
		json.Unmarshal(raw, &d)
	case <-time.After(time.Second):
		t.Fatal("no event")
	}
	if d.Type != "delta" || d.Prev != before || d.Seq != h.store.Head() || len(d.Messages) != 1 || d.Messages[0].ID != "m1" {
		t.Fatalf("got %+v", d)
	}
}

func TestSlowSubscriberIsDroppedNotWaitedFor(t *testing.T) {
	hub := NewHub()
	sub := hub.Subscribe()
	done := make(chan struct{})
	go func() {
		for i := 0; i < 2000; i++ {
			hub.Publish(map[string]int{"i": i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a slow subscriber blocked publishing")
	}
	count := 0
	for range sub {
		count++
	}
	if count != 512 {
		t.Fatalf("buffered %d", count)
	}
}

func TestHistoryUnreadCountSetsMarker(t *testing.T) {
	h := newHarness(t)
	base := time.Now().Add(-time.Hour)
	var msgs []*events.Message
	for i := 0; i < 5; i++ {
		id := "h" + string(rune('a'+i))
		msgs = append(msgs, incoming(t, testDM, testDM, id, base.Add(time.Duration(i)*time.Minute), textMsg(id)))
	}
	if _, err := h.store.IngestHistory(testDM, HistoryChat{Name: "P", Unread: 2, Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	c, _ := h.store.GetChat(testDM)
	want := base.Add(3 * time.Minute).UnixMilli()
	if c.LastReadMs >= want || c.LastReadMs < base.Add(2*time.Minute).UnixMilli() {
		t.Fatalf("marker %d should sit just before the second newest (%d)", c.LastReadMs, want)
	}
}

func TestHistoryDoesNotMoveChatBackwardsOrUndoStatus(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.app.recordSent(jidOf(t, testDM), textMsg("new"), whatsmeow.SendResponse{ID: "S1", Timestamp: now})
	h.store.ApplyReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: jidOf(t, testDM)}, MessageIDs: []string{"S1"}, Type: types.ReceiptTypeRead})
	older := incoming(t, testDM, testDM, "old", now.Add(-24*time.Hour), textMsg("old"))
	again := &events.Message{Message: textMsg("new"), Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jidOf(t, testDM), Sender: jidOf(t, testDM), IsFromMe: true}, ID: "S1", Timestamp: now,
	}}
	if _, err := h.store.IngestHistory(testDM, HistoryChat{Name: "P", Messages: []*events.Message{older, again}}); err != nil {
		t.Fatal(err)
	}
	c, _ := h.store.GetChat(testDM)
	if c.LastMs < now.UnixMilli()-1000 {
		t.Fatalf("history moved the chat's time back: %d", c.LastMs)
	}
	m, _ := h.store.GetMessage("S1", testDM)
	if m.Status != "read" {
		t.Fatalf("redelivery undid the status: %s", m.Status)
	}
}

func TestMediaDownloadThenServeWithRange(t *testing.T) {
	h := newHarness(t)
	ready := make(chan struct{})
	h.wa.download = func(d whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		<-ready
		_, err := f.Write([]byte("0123456789"))
		return err
	}
	msg := &waE2E.Message{VideoMessage: &waE2E.VideoMessage{Mimetype: proto.String("video/mp4"), URL: proto.String("https://mmg.example/v"),
		DirectPath: proto.String("/v"), MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(10)}}
	h.store.Ingest(incoming(t, testDM, testDM, "v1", time.Now(), msg), "P", "")
	path := "/api/app/media?chat=" + testDM + "&id=v1"
	if code := h.getJSON(path, nil); code != 202 {
		t.Fatalf("first request should start a download: %d", code)
	}
	if code := h.getJSON(path, nil); code != 202 {
		t.Fatalf("second request should wait: %d", code)
	}
	close(ready)
	deadline := time.Now().Add(3 * time.Second)
	for h.getJSON(path, nil) != 200 {
		if time.Now().After(deadline) {
			t.Fatal("never became ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	req, _ := http.NewRequest("GET", h.srv.URL+path, nil)
	req.Header.Set("Range", "bytes=2-4")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 206 || string(body) != "234" || resp.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("range: %d %q %s", resp.StatusCode, body, resp.Header.Get("Content-Type"))
	}
}

func TestMediaFailureIsReportedThenRetriable(t *testing.T) {
	h := newHarness(t)
	h.wa.download = func(d whatsmeow.DownloadableMessage, f whatsmeow.File) error { return errors.New("network down") }
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), URL: proto.String("https://mmg.example/i"),
		MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(1)}}
	h.store.Ingest(incoming(t, testDM, testDM, "i1", time.Now(), msg), "P", "")
	path := "/api/app/media?chat=" + testDM + "&id=i1"
	h.getJSON(path, nil)
	deadline := time.Now().Add(3 * time.Second)
	for h.getJSON(path, nil) != 410 {
		if time.Now().After(deadline) {
			t.Fatal("failure never reported")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestExpiredMediaAsksPhoneToReupload(t *testing.T) {
	h := newHarness(t)
	h.wa.download = func(d whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		return whatsmeow.ErrMediaDownloadFailedWith410
	}
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), URL: proto.String("https://mmg.example/i"),
		MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(1)}}
	h.store.Ingest(incoming(t, testDM, testDM, "i1", time.Now(), msg), "P", "")
	h.getJSON("/api/app/media?chat="+testDM+"&id=i1", nil)
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.wa.mu.Lock()
		n := len(h.wa.retried)
		h.wa.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the phone was never asked")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestMediaLookupRefusals(t *testing.T) {
	h := newHarness(t)
	h.store.Ingest(incoming(t, testDM, testDM, "t1", time.Now(), textMsg("x")), "P", "")
	if code := h.getJSON("/api/app/media?chat="+testDM+"&id=t1", nil); code != 404 {
		t.Fatalf("text message: %d", code)
	}
	if code := h.getJSON("/api/app/media?chat="+testDM+"&id=nope", nil); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	if code := h.getJSON("/api/app/media?chat=&id=", nil); code != 400 {
		t.Fatalf("empty: %d", code)
	}
}

func putChunk(t *testing.T, h *harness, uid string, offset int, data []byte) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("PUT", h.srv.URL+"/api/app/upload/"+uid+"?offset="+itoa(offset), bytes.NewReader(data))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestUploadChunksResumeAndSend(t *testing.T) {
	h := newHarness(t)
	uid := "upload-0001"
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{1}, 100)...)
	if code, _ := putChunk(t, h, uid, 0, png[:50]); code != 200 {
		t.Fatalf("chunk: %d", code)
	}
	if code, out := putChunk(t, h, uid, 10, png[50:]); code != 409 || out["size"].(float64) != 50 {
		t.Fatalf("wrong offset must be refused and report the real size: %d %v", code, out)
	}
	if code, _ := putChunk(t, h, uid, 50, png[50:]); code != 200 {
		t.Fatalf("resume: %d", code)
	}
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	code := h.post("/api/app/upload/"+uid+"/finish", map[string]any{
		"chat": testDM, "kind": "image", "filename": "a.png", "caption": "cap", "client_id": "cu1",
	}, nil)
	if code != 202 {
		t.Fatalf("finish: %d", code)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case raw := <-sub:
			var e struct {
				Type string `json:"type"`
				OK   bool   `json:"ok"`
			}
			json.Unmarshal(raw, &e)
			if e.Type != "send_result" {
				continue
			}
			if !e.OK {
				t.Fatalf("send failed: %s", raw)
			}
			h.wa.mu.Lock()
			img := h.wa.sent[0].GetImageMessage()
			h.wa.mu.Unlock()
			if img == nil || img.GetCaption() != "cap" || img.GetMimetype() != "image/png" {
				t.Fatalf("wrong message: %+v", img)
			}
			if _, err := os.Stat(filepath.Join(h.app.uploadDir, uid+".part")); !os.IsNotExist(err) {
				t.Fatal("spool file left behind")
			}
			return
		case <-deadline:
			t.Fatal("no send_result")
		}
	}
}

func TestUploadIDCannotEscapeSpoolFolder(t *testing.T) {
	h := newHarness(t)
	for _, uid := range []string{"..%2f..%2fevil", "a", "has space aaaaaaaaa", "..", "%2e%2e%5cevil1234"} {
		req, _ := http.NewRequest("PUT", h.srv.URL+"/api/app/upload/"+uid+"?offset=0", strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			continue
		}
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("uid %q accepted", uid)
		}
	}
	entries, _ := os.ReadDir(h.app.uploadDir)
	if len(entries) != 0 {
		t.Fatalf("files created: %v", entries)
	}
}

func TestDetectKind(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n0000")
	exe := []byte("MZ\x90\x00")
	cases := []struct {
		req, name string
		head      []byte
		kind      string
	}{
		{"image", "a.png", png, "image"},
		{"image", "fake.png", exe, "document"},
		{"video", "a.mov", []byte("not an mp4"), "document"},
		{"audio", "a.mp3", []byte("ID3"), "audio"},
		{"audio", "a.wav", []byte("RIFF"), "document"},
		{"document", "a.png", png, "document"},
	}
	for _, c := range cases {
		if kind, _ := detectKind(c.req, c.name, c.head); kind != c.kind {
			t.Errorf("%s %s: got %s, want %s", c.req, c.name, kind, c.kind)
		}
	}
}

func TestSafeFilename(t *testing.T) {
	cases := map[string]string{
		`..\..\Windows\evil.exe`: "evil.exe",
		"../../etc/passwd":       "passwd",
		"normal report.pdf":      "normal report.pdf",
		`a:b*c?.txt`:             "a_b_c_.txt",
		"":                       "",
		"name\u202egpj.exe":      "name_gpj.exe",
	}
	for in, want := range cases {
		if got := safeFilename(in); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
}

func TestGuardRefusesBrowsersAndOtherHosts(t *testing.T) {
	called := 0
	h := appGuard(18700, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called++ }))
	cases := []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:18700", "", 200},
		{"localhost:18700", "", 200},
		{"127.0.0.1:18700", "https://evil.example", 403},
		{"127.0.0.1:18700", "null", 403},
		{"evil.example", "", 403},
		{"evil.example:18700", "", 403},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "/api/send", strings.NewReader("{}"))
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("host %q origin %q: got %d, want %d", c.host, c.origin, w.Code, c.want)
		}
	}
	if called != 2 {
		t.Fatalf("handler ran %d times", called)
	}
}

func TestContactsIncludeAliases(t *testing.T) {
	h := newHarness(t)
	var out struct {
		Contacts []struct {
			JID  string `json:"jid"`
			Full string `json:"full_name"`
		} `json:"contacts"`
		Aliases [][2]string `json:"aliases"`
	}
	if code := h.getJSON("/api/app/contacts", &out); code != 200 {
		t.Fatal(code)
	}
	if len(out.Contacts) != 1 || out.Contacts[0].Full != "Sample Person" || len(out.Aliases) != 1 || out.Aliases[0][0] != testLID {
		t.Fatalf("%+v", out)
	}
}

func TestParsePlaceholdersForNonTextMessages(t *testing.T) {
	m := parseMessage("x", &waE2E.Message{LocationMessage: &waE2E.LocationMessage{DegreesLatitude: proto.Float64(1.5), DegreesLongitude: proto.Float64(2.5)}})
	if !strings.HasPrefix(m.text, "[Location") {
		t.Fatalf("got %q", m.text)
	}
	if !parseMessage("x", &waE2E.Message{SenderKeyDistributionMessage: &waE2E.SenderKeyDistributionMessage{}}).empty() {
		t.Fatal("a protocol-only message must store nothing")
	}
	if !parseMessage("x", nil).empty() {
		t.Fatal("nil must be empty")
	}
}

func editOf(id, text string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id)}, EditedMessage: textMsg(text),
	}}
}

func revokeOf(id string) *waE2E.Message {
	return &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{
		Type: waE2E.ProtocolMessage_REVOKE.Enum(), Key: &waCommon.MessageKey{ID: proto.String(id)},
	}}
}

const (
	alice = "111@s.whatsapp.net"
	bob   = "222@s.whatsapp.net"
)

func TestGroupMemberCantRewriteOrEraseSomeoneElse(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testGroup, alice, "m1", now, textMsg("alice wrote this")), "G", "")
	h.store.Ingest(incoming(t, testGroup, bob, "e1", now, editOf("m1", "FORGED")), "G", "")
	m, _ := h.store.GetMessage("m1", testGroup)
	if m.Text != "alice wrote this" || m.EditedMs != 0 {
		t.Fatalf("a stranger edited Alice's message: %+v", m)
	}
	h.store.Ingest(incoming(t, testGroup, bob, "d1", now, revokeOf("m1")), "G", "")
	m, _ = h.store.GetMessage("m1", testGroup)
	if m.Revoked || m.Text == "" {
		t.Fatalf("a stranger deleted Alice's message: %+v", m)
	}
	h.store.Ingest(incoming(t, testGroup, alice, "e2", now, editOf("m1", "alice fixed it")), "G", "")
	m, _ = h.store.GetMessage("m1", testGroup)
	if m.Text != "alice fixed it" {
		t.Fatalf("the author couldn't edit: %+v", m)
	}
	h.store.Ingest(incoming(t, testGroup, alice, "d2", now, revokeOf("m1")), "G", "")
	m, _ = h.store.GetMessage("m1", testGroup)
	if !m.Revoked {
		t.Fatalf("the author couldn't delete: %+v", m)
	}
}

func TestNobodyCanRewriteOrEraseLeesMessages(t *testing.T) {
	h := newHarness(t)
	for _, chat := range []string{testDM, testGroup} {
		id := "mine-" + chat
		h.app.recordSent(jidOf(t, chat), textMsg("The user wrote this"), whatsmeow.SendResponse{ID: id, Timestamp: time.Now()})
		h.store.Ingest(incoming(t, chat, testDM, "e-"+chat, time.Now(), editOf(id, "FORGED")), "G", "")
		h.store.Ingest(incoming(t, chat, testDM, "d-"+chat, time.Now(), revokeOf(id)), "G", "")
		m, _ := h.store.GetMessage(id, chat)
		if m.Text != "The user wrote this" || m.Revoked {
			t.Fatalf("%s: the user's message was changed: %+v", chat, m)
		}
	}
}

func TestLeeCanEditHisOwnAndDeleteInGroups(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.app.recordSent(jidOf(t, testGroup), textMsg("mine"), whatsmeow.SendResponse{ID: "s1", Timestamp: now})
	own := func(id string, msg *waE2E.Message) *events.Message {
		return &events.Message{Message: msg, Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: jidOf(t, testGroup), Sender: jidOf(t, "15559990000@s.whatsapp.net"), IsFromMe: true, IsGroup: true},
			ID:            id, Timestamp: now,
		}}
	}
	h.store.Ingest(own("e1", editOf("s1", "mine, edited")), "G", "")
	m, _ := h.store.GetMessage("s1", testGroup)
	if m.Text != "mine, edited" {
		t.Fatalf("%+v", m)
	}
	h.store.Ingest(incoming(t, testGroup, alice, "m1", now, textMsg("from alice")), "G", "")
	h.store.Ingest(own("e2", editOf("m1", "FORGED")), "G", "")
	m, _ = h.store.GetMessage("m1", testGroup)
	if m.Text != "from alice" {
		t.Fatal("editing someone else's message must not apply, even from the user's own devices")
	}
	h.store.Ingest(own("d1", revokeOf("m1")), "G", "")
	m, _ = h.store.GetMessage("m1", testGroup)
	if !m.Revoked {
		t.Fatal("as a group admin, the user can delete others' messages from their phone")
	}
}

func TestSenderMatchesAcrossPhoneAndLID(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testGroup, testDM, "m1", now, textMsg("by number")), "G", "")
	h.store.Ingest(incoming(t, testGroup, testLID, "e1", now, editOf("m1", "fixed by same person")), "G", "")
	m, _ := h.store.GetMessage("m1", testGroup)
	if m.Text != "fixed by same person" {
		t.Fatalf("the same person under their other address was refused: %+v", m)
	}
}

func TestOldGroupRowsCantBeRewrittenByAnyone(t *testing.T) {
	// Rows from before the fix have no recorded sender, so nobody can prove they wrote them.
	h := newHarness(t)
	now := time.Now()
	h.db.Exec(`INSERT INTO chats VALUES (?, 'G', ?)`, testGroup, now)
	h.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES ('old', ?, 'x', 'old text', ?, 0, '', '')`, testGroup, now)
	h.store.backfill()
	h.store.Ingest(incoming(t, testGroup, bob, "e1", now, editOf("old", "FORGED")), "G", "")
	h.store.Ingest(incoming(t, testGroup, bob, "old", now, textMsg("FORGED BY REUSING THE ID")), "G", "")
	m, _ := h.store.GetMessage("old", testGroup)
	if m.Text != "old text" {
		t.Fatalf("%+v", m)
	}
}

func TestReusingAnIDCantOverwriteAMessage(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testGroup, alice, "m1", now, textMsg("original")), "G", "")
	h.store.Ingest(incoming(t, testGroup, bob, "m1", now, textMsg("FORGED")), "G", "")
	m, _ := h.store.GetMessage("m1", testGroup)
	if m.Text != "original" || m.SenderJID != alice {
		t.Fatalf("%+v", m)
	}
	h.store.Ingest(incoming(t, testGroup, alice, "m1", now, textMsg("original")), "G", "")
	m, _ = h.store.GetMessage("m1", testGroup)
	if m.Text != "original" {
		t.Fatal("a genuine redelivery must still work")
	}
	h.app.recordSent(jidOf(t, testGroup), textMsg("mine"), whatsmeow.SendResponse{ID: "s1", Timestamp: now})
	h.store.Ingest(incoming(t, testGroup, alice, "s1", now, textMsg("FORGED")), "G", "")
	m, _ = h.store.GetMessage("s1", testGroup)
	if m.Text != "mine" || !m.FromMe {
		t.Fatalf("%+v", m)
	}
}

func TestHistoryFromThePhoneCanCorrectOldRows(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.db.Exec(`INSERT INTO chats VALUES (?, 'G', ?)`, testGroup, now)
	h.db.Exec(`INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename) VALUES ('old', ?, 'x', '', ?, 0, 'image', '')`, testGroup, now)
	h.store.backfill()
	evt := incoming(t, testGroup, alice, "old", now, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("recovered caption"), Mimetype: proto.String("image/jpeg")}})
	if _, err := h.store.IngestHistory(testGroup, HistoryChat{Name: "G", Messages: []*events.Message{evt}}); err != nil {
		t.Fatal(err)
	}
	m, _ := h.store.GetMessage("old", testGroup)
	if m.Text != "recovered caption" || m.SenderJID != alice {
		t.Fatalf("a re-link must be able to repair old rows: %+v", m)
	}
}

func TestIDFromAnotherChatCantReachAMessage(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	other := "333@s.whatsapp.net"
	h.store.Ingest(incoming(t, testDM, testDM, "m1", now, textMsg("private")), "P", "")
	h.store.Ingest(incoming(t, other, other, "e1", now, editOf("m1", "FORGED")), "O", "")
	h.store.Ingest(incoming(t, other, other, "d1", now, revokeOf("m1")), "O", "")
	h.store.Ingest(incoming(t, other, other, "r1", now, &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{Key: &waCommon.MessageKey{ID: proto.String("m1")}, Text: proto.String("X")}}), "O", "")
	m, _ := h.store.GetMessage("m1", testDM)
	if m.Text != "private" || m.Revoked || len(m.Reactions) != 0 {
		t.Fatalf("%+v", m)
	}
	h.app.recordSent(jidOf(t, testDM), textMsg("sent"), whatsmeow.SendResponse{ID: "s1", Timestamp: now})
	h.store.ApplyReceipt(&events.Receipt{MessageSource: types.MessageSource{Chat: jidOf(t, other)}, MessageIDs: []string{"s1"}, Type: types.ReceiptTypeRead})
	if s, _ := h.store.GetMessage("s1", testDM); s.Status == "read" {
		t.Fatal("a receipt from another chat changed status")
	}
}

func TestDownloadBiggerThanClaimedIsStopped(t *testing.T) {
	h := newHarness(t)
	h.wa.download = func(d whatsmeow.DownloadableMessage, f whatsmeow.File) error {
		// Both ways whatsmeow might write, and a big ReadFrom-style copy.
		if _, err := io.Copy(f, bytes.NewReader(make([]byte, 1<<20))); err != nil {
			return err
		}
		return nil
	}
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), URL: proto.String("https://mmg.example/i"),
		MediaKey: []byte("k"), FileSHA256: []byte("s"), FileEncSHA256: []byte("e"), FileLength: proto.Uint64(10)}}
	h.store.Ingest(incoming(t, testDM, testDM, "big", time.Now(), msg), "P", "")
	path := "/api/app/media?chat=" + testDM + "&id=big"
	h.getJSON(path, nil)
	deadline := time.Now().Add(3 * time.Second)
	var out map[string]any
	for h.getJSON(path, &out) != 410 {
		if time.Now().After(deadline) {
			t.Fatal("never refused")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(out["error"].(string), "bigger") {
		t.Fatalf("%v", out)
	}
	entries, _ := os.ReadDir(h.app.mediaDir)
	if len(entries) != 0 {
		t.Fatalf("left on disk: %v", entries)
	}
}

func TestStrangersOversizedFieldsAreCappedAtIngest(t *testing.T) {
	h := newHarness(t)
	msg := &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
		Mimetype: proto.String("image/jpeg"), JPEGThumbnail: make([]byte, 5<<20), Caption: proto.String(strings.Repeat("é", 1<<20)),
		ContextInfo: &waE2E.ContextInfo{StanzaID: proto.String("q"), QuotedMessage: textMsg(strings.Repeat("x", 1<<20))},
	}}
	h.store.Ingest(incoming(t, testDM, testDM, "huge", time.Now(), msg), "P", "")
	m, err := h.store.GetMessage("huge", testDM)
	if err != nil {
		t.Fatal(err)
	}
	if m.Thumb != nil || len(m.Text) > maxTextBytes || len(m.QuotedText) > maxQuoteBytes {
		t.Fatalf("thumb %d text %d quote %d", len(m.Thumb), len(m.Text), len(m.QuotedText))
	}
	if !strings.HasPrefix(m.Text, "é") || strings.ContainsRune(m.Text, '\uFFFD') {
		t.Fatal("clipping broke a character")
	}
}

func TestMediaCacheStaysUnderItsCap(t *testing.T) {
	h := newHarness(t)
	h.app.mediaCap = 150
	old := filepath.Join(h.app.mediaDir, "old.bin")
	recent := filepath.Join(h.app.mediaDir, "new.bin")
	os.WriteFile(old, make([]byte, 100), 0o600)
	os.WriteFile(recent, make([]byte, 100), 0o600)
	os.Chtimes(old, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour))
	h.app.PruneMedia()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the oldest file should go first")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("the recent file should stay")
	}
}

func TestFilenamesFromMessageIDsAreClean(t *testing.T) {
	p := parseMessage(`..\..\evil`, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}})
	if strings.ContainsAny(p.filename, `\/`) || strings.Contains(p.filename, "..") {
		t.Fatalf("%q", p.filename)
	}
	for _, bad := range []string{"..", ".", "a/..", `..\..`, "   ", "..."} {
		if got := safeFilename(bad); got != "" {
			t.Errorf("%q gave %q", bad, got)
		}
	}
	if got := safeFilename("run.exe. . "); got != "run.exe" {
		t.Errorf("trailing dots: %q", got)
	}
}

func TestStatusBroadcastIsNotStored(t *testing.T) {
	h := newHarness(t)
	h.app.handleLiveMessage(nil, nil, incoming(t, "status@broadcast", testDM, "s1", time.Now(), textMsg("my status")), nil)
	var n int
	h.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n)
	if n != 0 {
		t.Fatal("status update stored as a chat")
	}
}
