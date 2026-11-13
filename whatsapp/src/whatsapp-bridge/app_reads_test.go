package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	waCommon "go.mau.fi/whatsmeow/proto/waCommon"
	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	waSyncAction "go.mau.fi/whatsmeow/proto/waSyncAction"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

func readMs(t *testing.T, h *harness, chat string) int64 {
	t.Helper()
	c, err := h.store.GetChat(chat)
	if err != nil {
		t.Fatal(err)
	}
	return c.LastReadMs
}

// phoneRead is the receipt the user's phone sends their other devices when they read a chat.
func phoneRead(t *testing.T, chat string, rt types.ReceiptType, at time.Time, ids ...string) *events.Receipt {
	return &events.Receipt{
		MessageSource: types.MessageSource{Chat: jidOf(t, chat), Sender: jidOf(t, "15559990000@s.whatsapp.net"), IsFromMe: true,
			IsGroup: strings.HasSuffix(chat, "@g.us")},
		MessageIDs: ids, Type: rt, Timestamp: at,
	}
}

func TestPhoneReadWithReceiptsOnMovesMarker(t *testing.T) {
	// The bug: with read receipts on, the phone reports the user's reads as plain reads from their own account,
	// and only read-self was handled, so nothing he read on the phone ever cleared here.
	h := newHarness(t)
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	for i, id := range []string{"a", "b", "c"} {
		h.store.Ingest(incoming(t, testDM, testDM, id, base.Add(time.Duration(i)*time.Minute), textMsg(id)), "P", "")
	}
	h.app.handleEvent(phoneRead(t, testDM, types.ReceiptTypeRead, time.Now(), "a", "b"))
	if got, want := readMs(t, h, testDM), base.Add(time.Minute).UnixMilli(); got != want {
		t.Fatalf("marker should be at the newest message read (%d), got %d", want, got)
	}
	if line := h.app.reads.line(); !strings.Contains(line, "phone read moved 1") {
		t.Fatalf("not counted: %q", line)
	}
}

func TestPhoneReadInGroupAndReadSelf(t *testing.T) {
	h := newHarness(t)
	now := time.Now().Truncate(time.Second)
	h.store.Ingest(incoming(t, testGroup, testDM, "g1", now.Add(-time.Minute), textMsg("x")), "G", "")
	h.app.handleEvent(phoneRead(t, testGroup, types.ReceiptTypeRead, now, "g1"))
	if readMs(t, h, testGroup) != now.Add(-time.Minute).UnixMilli() {
		t.Fatal("group read from the phone didn't move the marker")
	}
	h.store.Ingest(incoming(t, testDM, testDM, "d1", now, textMsg("x")), "P", "")
	h.app.handleEvent(phoneRead(t, testDM, types.ReceiptTypeReadSelf, now.Add(time.Second), "d1"))
	if readMs(t, h, testDM) != now.UnixMilli() {
		t.Fatal("read-self (receipts off) must still work")
	}
}

func TestOthersReadingMyMessageLeavesMarker(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testDM, testDM, "in", now.Add(-time.Minute), textMsg("x")), "P", "")
	h.app.recordSent(jidOf(t, testDM), textMsg("mine"), whatsmeow.SendResponse{ID: "S1", Timestamp: now.Add(-30 * time.Second)})
	before := readMs(t, h, testDM)
	h.app.handleEvent(&events.Receipt{MessageSource: types.MessageSource{Chat: jidOf(t, testDM), Sender: jidOf(t, testDM)},
		MessageIDs: []string{"S1"}, Type: types.ReceiptTypeRead, Timestamp: now})
	if readMs(t, h, testDM) != before {
		t.Fatal("the other person reading my message must not mark their messages read")
	}
	m, _ := h.store.GetMessage("S1", testDM)
	if m.Status != "read" {
		t.Fatalf("blue ticks on my message lost: %q", m.Status)
	}
}

func TestPhoneReadFindsTheOtherAddress(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	// Stored under the number, read named by LID.
	h := newHarness(t)
	h.store.Ingest(incoming(t, testDM, testDM, "p1", now, textMsg("x")), "P", "")
	h.app.handleEvent(phoneRead(t, testLID, types.ReceiptTypeRead, now.Add(time.Second), "p1"))
	if readMs(t, h, testDM) != now.UnixMilli() {
		t.Fatal("a read naming the LID must clear the chat stored by number")
	}
	// Stored under the LID, read named by number.
	h2 := newHarness(t)
	h2.store.Ingest(incoming(t, testLID, testLID, "l1", now, textMsg("x")), "P", "")
	h2.app.handleEvent(phoneRead(t, testDM, types.ReceiptTypeRead, now.Add(time.Second), "l1"))
	if readMs(t, h2, testLID) != now.UnixMilli() {
		t.Fatal("a read naming the number must clear the chat stored by LID")
	}
}

func TestPhoneReadOfUnstoredMessageUsesReadTime(t *testing.T) {
	h := newHarness(t)
	now := time.Now().Truncate(time.Second)
	h.store.Ingest(incoming(t, testDM, testDM, "old", now.Add(-time.Hour), textMsg("x")), "P", "")
	h.app.handleEvent(phoneRead(t, testDM, types.ReceiptTypeRead, now, "not-stored"))
	if readMs(t, h, testDM) != now.UnixMilli() {
		t.Fatal("should fall back to when it was read")
	}
	// And never backwards.
	h.app.handleEvent(phoneRead(t, testDM, types.ReceiptTypeRead, now.Add(-time.Minute), "old"))
	if readMs(t, h, testDM) != now.UnixMilli() {
		t.Fatal("marker went backwards")
	}
}

func TestPhoneReadOfUnknownChatCreatesNothing(t *testing.T) {
	h := newHarness(t)
	h.app.handleEvent(phoneRead(t, "15550000077@s.whatsapp.net", types.ReceiptTypeRead, time.Now(), "x"))
	if _, err := h.store.GetChat("15550000077@s.whatsapp.net"); err == nil {
		t.Fatal("a read must not create a chat")
	}
	if line := h.app.reads.line(); !strings.Contains(line, "phone read no chat 1") {
		t.Fatalf("not counted: %q", line)
	}
}

func TestMarkedReadOnPhoneByLID(t *testing.T) {
	h := newHarness(t)
	now := time.Now().Truncate(time.Second)
	h.store.Ingest(incoming(t, testDM, testDM, "p1", now, textMsg("x")), "P", "")
	h.app.handleEvent(&events.MarkChatAsRead{JID: jidOf(t, testLID), Timestamp: now.Add(time.Second),
		Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(true)}})
	if readMs(t, h, testDM) != now.Add(time.Second).UnixMilli() {
		t.Fatal("mark as read on the phone, named by LID, didn't clear")
	}
	h.app.handleEvent(&events.MarkChatAsRead{JID: jidOf(t, testDM), Timestamp: now.Add(2 * time.Second),
		Action: &waSyncAction.MarkChatAsReadAction{Read: proto.Bool(false)}})
	if line := h.app.reads.line(); !strings.Contains(line, "marked read moved 1") || !strings.Contains(line, "marked unread 1") {
		t.Fatalf("not counted: %q", line)
	}
}

func TestWritingOnThePhoneMarksRead(t *testing.T) {
	h := newHarness(t)
	now := time.Now().Truncate(time.Second)
	in := incoming(t, testDM, testDM, "in1", now.Add(-time.Minute), textMsg("question"))
	h.store.Ingest(in, "P", "")
	h.app.afterLiveMessage(in)
	if readMs(t, h, testDM) >= now.Add(-time.Minute).UnixMilli() {
		t.Fatal("an incoming message must not mark read")
	}
	// A reaction from the phone isn't writing.
	react := mine(t, testDM, "r1", now.Add(-30*time.Second), &waE2E.Message{ReactionMessage: &waE2E.ReactionMessage{
		Key: &waCommon.MessageKey{ID: proto.String("in1")}, Text: proto.String("👍")}})
	h.store.Ingest(react, "P", "")
	h.app.afterLiveMessage(react)
	if readMs(t, h, testDM) >= now.Add(-time.Minute).UnixMilli() {
		t.Fatal("a reaction must not mark read")
	}
	reply := mine(t, testDM, "out1", now, textMsg("answer"))
	h.store.Ingest(reply, "P", "")
	h.app.afterLiveMessage(reply)
	if readMs(t, h, testDM) != now.UnixMilli() {
		t.Fatal("replying on the phone must mark the chat read up to the reply")
	}
	later := incoming(t, testDM, testDM, "in2", now.Add(time.Minute), textMsg("next"))
	h.store.Ingest(later, "P", "")
	if c, _ := h.store.GetChat(testDM); c.LastReadMs >= now.Add(time.Minute).UnixMilli() {
		t.Fatal("a message after the reply must stay unread")
	}
}

func TestReadCountsHoldNoAddresses(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.store.Ingest(incoming(t, testDM, testDM, "a", now, textMsg("x")), "P", "")
	h.app.handleEvent(phoneRead(t, testDM, types.ReceiptTypeRead, now, "a"))
	h.post("/api/app/read", map[string]any{"chat": testDM, "up_to_ms": now.UnixMilli() + 5, "receipts": true,
		"messages": []map[string]string{{"id": "a"}}}, nil)
	line := h.app.reads.line()
	for _, bad := range []string{"15550000001", "lid", "@", "Pusher"} {
		if strings.Contains(line, bad) {
			t.Fatalf("log line holds %q: %s", bad, line)
		}
	}
	if !strings.Contains(line, "app read moved 1") || !strings.Contains(line, "receipts sent 1") {
		t.Fatalf("counts missing: %s", line)
	}
	if h.app.reads.line() != "" {
		t.Fatal("counts should start again after each line")
	}
}

func mine(t *testing.T, chat, id string, ts time.Time, msg *waE2E.Message) *events.Message {
	evt := incoming(t, chat, "15559990000@s.whatsapp.net", id, ts, msg)
	evt.Info.IsFromMe = true
	return evt
}

func TestCatchUpOwnRepliesRunsOnce(t *testing.T) {
	h := newHarness(t)
	now := time.Now().Truncate(time.Second)
	h.store.Ingest(incoming(t, testDM, testDM, "q", now.Add(-time.Hour), textMsg("question")), "P", "")
	h.store.Ingest(mine(t, testDM, "a", now.Add(-50*time.Minute), textMsg("answer")), "P", "")
	h.store.Ingest(incoming(t, testDM, testDM, "q2", now.Add(-40*time.Minute), textMsg("later")), "P", "")
	h.store.Ingest(incoming(t, testGroup, testDM, "g", now.Add(-time.Hour), textMsg("no reply here")), "G", "")
	record := filepath.Join(t.TempDir(), "repair.json")
	n, err := h.store.CatchUpOwnReplies(record)
	if err != nil || n != 1 {
		t.Fatalf("expected one chat caught up: %d %v", n, err)
	}
	if readMs(t, h, testDM) != now.Add(-50*time.Minute).UnixMilli() {
		t.Fatal("marker should sit at the reply, leaving the later message unread")
	}
	if readMs(t, h, testGroup) != 0 {
		t.Fatal("a chat without a reply must not move")
	}
	data, _ := os.ReadFile(record)
	if !strings.Contains(string(data), `"before_ms": 0`) {
		t.Fatalf("record lacks the before value: %s", data)
	}
	// A later send, such as Claude's through the connector, isn't a read, and the catch-up doesn't run again.
	h.app.recordSent(jidOf(t, testDM), textMsg("via mcp"), whatsmeow.SendResponse{ID: "M1", Timestamp: now})
	if n, _ := h.store.CatchUpOwnReplies(record); n != 0 || readMs(t, h, testDM) != now.Add(-50*time.Minute).UnixMilli() {
		t.Fatal("the catch-up ran twice")
	}
}
