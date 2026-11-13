package main

// Groups with no name go by their members' names. Everyone here is made up.

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const unnamedChat = "120363000000000077@g.us"

func TestGroupLabelJoinsNamesLikeWhatsApp(t *testing.T) {
	for want, names := range map[string][]string{
		"":                            nil,
		"Ann":                         {"Ann"},
		"Ann & Bo":                    {"Bo", "Ann"},
		"Ann, Bo & Cy":                {"Cy", "Ann", "Bo"},
		"Ann, Bo & +15550000003":      {"+15550000003", "Bo", "Ann"},
		"A, B, C, D, E, F & 1 other":  {"A", "B", "C", "D", "E", "F", "G"},
		"A, B, C, D, E, F & 3 others": {"A", "B", "C", "D", "E", "F", "G", "H", "I"},
	} {
		if got := groupLabel(names); got != want {
			t.Errorf("%q: got %q", want, got)
		}
	}
}

func unnamedMembers() *types.GroupInfo {
	return &types.GroupInfo{Participants: []types.GroupParticipant{
		{JID: types.NewJID("15559990000", types.DefaultUserServer)},                                                             // The user
		{JID: types.NewJID("99999", types.HiddenUserServer)},                                                                    // hidden ID with a known number and a saved name
		{JID: types.NewJID("33333", types.HiddenUserServer), PhoneNumber: types.NewJID("15550000002", types.DefaultUserServer)}, // profile name only
		{JID: types.NewJID("15550000003", types.DefaultUserServer)},                                                             // number only
		{JID: types.NewJID("88888", types.HiddenUserServer)},                                                                    // nothing known: left out
	}}
}

func newUnnamedHarness(t *testing.T, stored string) *harness {
	t.Helper()
	h := newHarness(t)
	h.wa.contacts = map[types.JID]types.ContactInfo{
		types.NewJID("15550000001", types.DefaultUserServer): {FullName: "Sample Person"},
		types.NewJID("15550000002", types.DefaultUserServer): {PushName: "Made-up Profile"},
	}
	msg := incoming(t, unnamedChat, "15550000003@s.whatsapp.net", "U1", time.Now().Add(-time.Minute), textMsg("A made-up message"))
	if err := h.store.Ingest(msg, stored, ""); err != nil {
		t.Fatal(err)
	}
	return h
}

func storedName(t *testing.T, h *harness, chat string) string {
	t.Helper()
	var name string
	if err := h.db.QueryRow(`SELECT COALESCE(name, '') FROM chats WHERE jid = ?`, chat).Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

func shownName(t *testing.T, h *harness, chat string) string {
	t.Helper()
	c, err := h.store.GetChat(chat)
	if err != nil {
		t.Fatal(err)
	}
	return c.Name
}

func TestUnnamedGroupGoesByItsMembers(t *testing.T) {
	h := newUnnamedHarness(t, "Group 120363000000000077")
	h.wa.groupInfo = unnamedMembers()
	before := h.store.Head()
	h.app.sweepUnnamedGroups(context.Background(), true)

	want := "Made-up Profile, Sample Person & +15550000003"
	if got := shownName(t, h, unnamedChat); got != want {
		t.Fatalf("shown as %q, want %q", got, want)
	}
	if got := storedName(t, h, unnamedChat); got != "" {
		t.Fatalf("the placeholder or the label was stored as its name: %q", got)
	}
	page, err := h.store.Sync(before, 100)
	if err != nil || len(page.Chats) != 1 || page.Chats[0].Name != want {
		t.Fatalf("apps aren't sent the new name: %+v %v", page.Chats, err)
	}
	members, _ := h.store.GroupMembers(unnamedChat)
	if len(members) != 5 || h.wa.groupInfoCalls != 1 {
		t.Fatalf("members %v, asked WhatsApp %d times", members, h.wa.groupInfoCalls)
	}

	// Once members are known, another sweep doesn't ask WhatsApp, and nothing changes.
	head := h.store.Head()
	h.app.sweepUnnamedGroups(context.Background(), true)
	if h.wa.groupInfoCalls != 1 || h.store.Head() != head {
		t.Fatalf("asked again (%d) or resent (%d to %d)", h.wa.groupInfoCalls, head, h.store.Head())
	}
}

func TestLeesOwnNameForAMemberWins(t *testing.T) {
	h := newUnnamedHarness(t, "")
	h.wa.groupInfo = unnamedMembers()
	h.app.sweepUnnamedGroups(context.Background(), true)
	if err := h.app.names.Set("15550000003@s.whatsapp.net", "Made-up Neighbour"); err != nil {
		t.Fatal(err)
	}
	head := h.store.Head()
	h.app.sweepUnnamedGroups(context.Background(), false)
	if got := shownName(t, h, unnamedChat); got != "Made-up Neighbour, Made-up Profile & Sample Person" {
		t.Fatalf("got %q", got)
	}
	if h.store.Head() == head {
		t.Fatal("the renamed label wasn't sent to apps")
	}
}

func TestANameGivenLaterReplacesTheLabel(t *testing.T) {
	h := newUnnamedHarness(t, "")
	h.wa.groupInfo = unnamedMembers()
	h.app.sweepUnnamedGroups(context.Background(), true)
	h.app.handleEvent(&events.GroupInfo{JID: jidOf(t, unnamedChat), Name: &types.GroupName{Name: "Made-up Trip"}})
	if got := shownName(t, h, unnamedChat); got != "Made-up Trip" {
		t.Fatalf("got %q", got)
	}
	// Asking WhatsApp again for a group that has a name now leaves the name alone.
	h.app.groupChanged(jidOf(t, unnamedChat))
	if err := h.app.applyGroupInfo(context.Background(), &types.GroupInfo{JID: jidOf(t, unnamedChat)}); err != nil {
		t.Fatal(err)
	}
	if got := shownName(t, h, unnamedChat); got != "Made-up Trip" {
		t.Fatalf("a member list replaced the name: %q", got)
	}
	// Taking the name away goes back to the members' names.
	h.app.handleEvent(&events.GroupInfo{JID: jidOf(t, unnamedChat), Name: &types.GroupName{Name: ""}})
	if got := shownName(t, h, unnamedChat); got != "Made-up Profile, Sample Person & +15550000003" {
		t.Fatalf("got %q", got)
	}
}

func TestAGroupWhoseNameWhatsAppKnowsGetsIt(t *testing.T) {
	h := newUnnamedHarness(t, "Group 120363000000000077")
	info := unnamedMembers()
	info.Name = "Made-up Club"
	h.wa.groupInfo = info
	h.app.sweepUnnamedGroups(context.Background(), true)
	if got := storedName(t, h, unnamedChat); got != "Made-up Club" {
		t.Fatalf("got %q", got)
	}
}

func TestUnnamedWithNoMembersKnown(t *testing.T) {
	h := newUnnamedHarness(t, "")
	h.wa.groupInfoErr = context.DeadlineExceeded
	h.app.sweepUnnamedGroups(context.Background(), true)
	if got := shownName(t, h, unnamedChat); got != unnamedGroup {
		t.Fatalf("got %q", got)
	}
}

func TestAMessageInAnUnnamedGroupLearnsItsMembers(t *testing.T) {
	h := newUnnamedHarness(t, "")
	h.wa.groupInfo = unnamedMembers()
	h.app.noticeUnnamed(jidOf(t, unnamedChat))
	deadline := time.Now().Add(3 * time.Second)
	for shownName(t, h, unnamedChat) == unnamedGroup && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := shownName(t, h, unnamedChat); got != "Made-up Profile, Sample Person & +15550000003" {
		t.Fatalf("got %q", got)
	}
}

func TestPlaceholderOnlyClearedForItsOwnGroup(t *testing.T) {
	h := newUnnamedHarness(t, "Group 120363000000000099") // someone's real name that happens to look like one
	if n, err := h.store.ClearPlaceholderNames(); err != nil || n != 0 {
		t.Fatalf("cleared %d, %v", n, err)
	}
	if got := storedName(t, h, unnamedChat); got != "Group 120363000000000099" {
		t.Fatalf("got %q", got)
	}
}
