package main

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

const (
	dana    = "777777777@lid"
	danaPN  = "15550000007@s.whatsapp.net"
	robin   = "888888888@lid"
	robinPN = "15550000008@s.whatsapp.net"
)

func lidMember(t *testing.T, lid, pn string) types.GroupParticipant {
	return types.GroupParticipant{JID: jidOf(t, lid), LID: jidOf(t, lid), PhoneNumber: jidOf(t, pn)}
}

func phoneMember(t *testing.T, lid, pn string) types.GroupParticipant {
	return types.GroupParticipant{JID: jidOf(t, pn), LID: jidOf(t, lid), PhoneNumber: jidOf(t, pn)}
}

func (h *pictureHarness) send(t *testing.T, id, chat, text string, mentions []string, quote map[string]any) (int, map[string]any) {
	t.Helper()
	var out map[string]any
	code := h.post("/api/app/send", map[string]any{"chat": chat, "text": text, "client_id": id, "mentions": mentions, "quote": quote}, &out)
	return code, out
}

func (h *pictureHarness) sentMessages() []*waE2E.Message {
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	return append([]*waE2E.Message{}, h.wa.sent...)
}

func (h *pictureHarness) waitSent(t *testing.T) *waE2E.Message {
	t.Helper()
	for i := 0; i < 300; i++ {
		if sent := h.sentMessages(); len(sent) > 0 {
			return sent[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("nothing was sent")
	return nil
}

func TestAMentionGoesOutWithTheGroupsOwnAddress(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN), lidMember(t, robin, robinPN)), nil)
	if code, _ := h.send(t, "c1", photoGroup, "Hi @777777777 and @888888888", []string{dana, robin}, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	ext := h.sentMessages()[0].GetExtendedTextMessage()
	if ext.GetText() != "Hi @777777777 and @888888888" {
		t.Fatalf("text %q", ext.GetText())
	}
	if !reflect.DeepEqual(ext.GetContextInfo().GetMentionedJID(), []string{dana, robin}) {
		t.Fatalf("mentions %v", ext.GetContextInfo().GetMentionedJID())
	}
}

func TestAPhoneAddressIsSwappedInAGroupUsingHiddenIDs(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	if code, _ := h.send(t, "c1", photoGroup, "Hi @15550000007, ok @15550000007?", []string{danaPN}, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	ext := h.sentMessages()[0].GetExtendedTextMessage()
	if ext.GetText() != "Hi @777777777, ok @777777777?" {
		t.Fatalf("the text and the mention must agree: %q", ext.GetText())
	}
	if !reflect.DeepEqual(ext.GetContextInfo().GetMentionedJID(), []string{dana}) {
		t.Fatalf("mentions %v", ext.GetContextInfo().GetMentionedJID())
	}
}

func TestAGroupUsingPhoneNumbersKeepsThem(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, phoneMember(t, dana, danaPN)), nil)
	if code, _ := h.send(t, "c1", photoGroup, "Hi @777777777", []string{dana}, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	ext := h.sentMessages()[0].GetExtendedTextMessage()
	if ext.GetText() != "Hi @15550000007" || !reflect.DeepEqual(ext.GetContextInfo().GetMentionedJID(), []string{danaPN}) {
		t.Fatalf("%q %v", ext.GetText(), ext.GetContextInfo().GetMentionedJID())
	}
}

func TestAMentionKeepsAReplysQuote(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	h.store.Ingest(incoming(t, photoGroup, robin, "Q1", time.Now(), textMsg("Made up")), "G", "")
	quote := map[string]any{"id": "Q1", "sender": robin, "text": "Made up"}
	if code, _ := h.send(t, "c1", photoGroup, "Hi @777777777", []string{dana}, quote); code != 200 {
		t.Fatalf("send: %d", code)
	}
	info := h.sentMessages()[0].GetExtendedTextMessage().GetContextInfo()
	if info.GetStanzaID() != "Q1" || info.GetParticipant() != robin || len(info.GetMentionedJID()) != 1 {
		t.Fatalf("%+v", info)
	}
}

func TestMentionsThatCantBeTrustedSendNothing(t *testing.T) {
	cases := map[string]struct {
		text     string
		mentions []string
		code     int
	}{
		"not in the text":          {"No mention here", []string{dana}, 400},
		"only inside an email":     {"Mail a@777777777 now", []string{dana}, 400},
		"only inside a number":     {"Call @7777777771", []string{dana}, 400},
		"someone not in the group": {"Hi @999999999", []string{"999999999@lid"}, 409},
		"a group, not a person":    {"Hi @120363000000000002", []string{otherGroup}, 400},
		"nonsense":                 {"Hi @x", []string{"not an address"}, 400},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
			code, out := h.send(t, "c1", photoGroup, c.text, c.mentions, nil)
			if code != c.code {
				t.Fatalf("code %d: %v", code, out)
			}
			if code == 401 || code == 403 {
				t.Fatal("these read as a lapsed login in the app")
			}
			if n := len(h.sentMessages()); n != 0 {
				t.Fatalf("%d messages went out", n)
			}
		})
	}
}

func TestAMentionInAOneToOneChatIsRefused(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	if code, _ := h.send(t, "c1", pictureChat, "Hi @777777777", []string{dana}, nil); code != 400 || len(h.sentMessages()) != 0 {
		t.Fatalf("code %d, sent %d", code, len(h.sentMessages()))
	}
}

func TestIfWhatsAppCantConfirmMembersNothingGoesAndARetryWorks(t *testing.T) {
	h := newPhotoHarness(t, nil, errors.New("no answer"))
	if code, _ := h.send(t, "c1", photoGroup, "Hi @777777777", []string{dana}, nil); code != 503 || len(h.sentMessages()) != 0 {
		t.Fatalf("code %d, sent %d", code, len(h.sentMessages()))
	}
	h.wa.mu.Lock()
	h.wa.groupInfo, h.wa.groupInfoErr = groupInfo(false, lidMember(t, dana, danaPN)), nil
	h.wa.mu.Unlock()
	h.p.mu.Lock()
	delete(h.p.groupInfos, photoGroup) // A failure is kept for a minute, and this test won't wait.
	h.p.mu.Unlock()
	if code, out := h.send(t, "c1", photoGroup, "Hi @777777777", []string{dana}, nil); code != 200 {
		t.Fatalf("the same message couldn't be retried: %d %v", code, out)
	}
}

func TestSomeoneWhoJustJoinedIsFoundByAskingAgain(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, robin, robinPN)), nil)
	if code, _ := h.send(t, "c1", photoGroup, "Hi @888888888", []string{robin}, nil); code != 200 {
		t.Fatalf("first: %d", code)
	}
	h.wa.mu.Lock()
	h.wa.groupInfo = groupInfo(false, lidMember(t, robin, robinPN), lidMember(t, dana, danaPN))
	h.wa.mu.Unlock()
	if code, out := h.send(t, "c2", photoGroup, "Hi @777777777", []string{dana}, nil); code != 200 {
		t.Fatalf("second: %d %v", code, out)
	}
}

func TestAPlainMessageNeverAsksWhatsAppAboutMembers(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	if code, _ := h.send(t, "c1", photoGroup, "Mail a@777777777 or @777777777 as typed", nil, nil); code != 200 {
		t.Fatalf("send: %d", code)
	}
	if h.sentMessages()[0].GetConversation() != "Mail a@777777777 or @777777777 as typed" {
		t.Fatalf("%+v", h.sentMessages()[0])
	}
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	if h.wa.groupInfoCalls != 0 {
		t.Fatalf("%d calls", h.wa.groupInfoCalls)
	}
}

func TestSwapMentionOnlyChangesWholeMentions(t *testing.T) {
	got, n := swapMention("@1 a@1 @12 (@1) @1é é@1 @1", "1", "9")
	if got != "@9 a@1 @12 (@9) @1é é@1 @9" || n != 3 {
		t.Fatalf("%q %d", got, n)
	}
	if !strings.Contains(got, "a@1") {
		t.Fatal("an email changed")
	}
}

func TestACaptionCanMentionSomeoneToo(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	uid := strings.Repeat("a", 32)
	putChunk(t, h.harness, uid, 0, []byte("made-up file"))
	var out map[string]any
	code := h.post("/api/app/upload/"+uid+"/finish", map[string]any{
		"chat": photoGroup, "kind": "document", "filename": "made-up.txt", "caption": "For @15550000007", "client_id": "c1", "mentions": []string{danaPN},
	}, &out)
	if code != 202 {
		t.Fatalf("finish: %d %v", code, out)
	}
	sent := h.waitSent(t)
	doc := sent.GetDocumentMessage()
	if doc.GetCaption() != "For @777777777" || !reflect.DeepEqual(doc.GetContextInfo().GetMentionedJID(), []string{dana}) {
		t.Fatalf("%q %v", doc.GetCaption(), doc.GetContextInfo().GetMentionedJID())
	}
}

func TestACaptionMentionThatFailsSendsNoFile(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, lidMember(t, dana, danaPN)), nil)
	uid := strings.Repeat("b", 32)
	putChunk(t, h.harness, uid, 0, []byte("made-up file"))
	var out map[string]any
	code := h.post("/api/app/upload/"+uid+"/finish", map[string]any{
		"chat": photoGroup, "kind": "document", "filename": "made-up.txt", "caption": "For @999999999", "client_id": "c1", "mentions": []string{"999999999@lid"},
	}, &out)
	if code != 409 || len(h.sentMessages()) != 0 {
		t.Fatalf("code %d, sent %d", code, len(h.sentMessages()))
	}
}
