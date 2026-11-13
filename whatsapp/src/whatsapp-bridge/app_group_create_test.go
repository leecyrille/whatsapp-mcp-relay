package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// All made up. The fake never reaches WhatsApp.
const (
	friendA = "15550000011@s.whatsapp.net"
	friendB = "15550000012@s.whatsapp.net"
)

func newCreateHarness(t *testing.T) *pictureHarness {
	t.Helper()
	h := newPictureHarness(t)
	h.wa.mu.Lock()
	h.wa.contacts = map[types.JID]types.ContactInfo{
		types.NewJID("15550000011", types.DefaultUserServer): {FullName: "Made-up A"},
		types.NewJID("15550000012", types.DefaultUserServer): {FullName: "Made-up B"},
	}
	h.wa.mu.Unlock()
	return h
}

func (h *pictureHarness) create(t *testing.T, name string, members ...string) (*http.Response, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "members": members})
	resp, err := http.Post(h.srv.URL+"/api/app/group/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *pictureHarness) creations() []whatsmeow.ReqCreateGroup {
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	return append([]whatsmeow.ReqCreateGroup{}, h.wa.createCalls...)
}

func TestCreatingAGroupSendsItOnceAndMakesAChat(t *testing.T) {
	h := newCreateHarness(t)
	long := "ABC-1007 - Unit 4B Feed Pump & ABC-1008 - Unit 2A Valve retrofit"
	resp, out := h.create(t, "  "+long+" ", friendA, friendB, friendA)
	if resp.StatusCode != 200 || resp.Header.Get(createResultHeader) != "created" || resp.Header.Get(createCountHeader) != "added=2 not_added=0" {
		t.Fatalf("%d %v %v", resp.StatusCode, resp.Header, out)
	}
	calls := h.creations()
	if len(calls) != 1 || calls[0].Name != long || len(calls[0].Participants) != 2 {
		t.Fatalf("%+v", calls)
	}
	var name string
	if err := h.db.QueryRow(`SELECT name FROM chats WHERE jid = ?`, out["jid"]).Scan(&name); err != nil || name != long {
		t.Fatalf("no chat row: %q %v", name, err)
	}
	var names struct {
		Chats []struct{ JID, Name string }
	}
	h.getJSON("/api/app/group-names", &names)
	found := false
	for _, c := range names.Chats {
		found = found || (c.Name == long)
	}
	if !found {
		t.Fatal("the job-number rule wouldn't see it")
	}
	var page struct {
		Chats []struct {
			JID string `json:"jid"`
		} `json:"chats"`
	}
	h.getJSON("/api/app/sync?after=0&limit=100", &page)
	found = false
	for _, c := range page.Chats {
		found = found || c.JID == out["jid"]
	}
	if !found {
		t.Fatalf("apps wouldn't hear about it: %+v", page.Chats)
	}
}

func TestOnlySavedContactsCanBeAddedAndNothingIsSentOtherwise(t *testing.T) {
	for name, members := range map[string][]string{
		"an unknown number": {friendA, "15559999999@s.whatsapp.net"},
		"a group":           {friendA, "120363000000000001@g.us"},
		"not an address":    {friendA, "../../x"},
		"a pasted number":   {"15559999999"},
	} {
		h := newCreateHarness(t)
		resp, out := h.create(t, "Made up", members...)
		if resp.StatusCode != 422 || resp.Header.Get(createResultHeader) != "not_a_contact" || out["ok"] != false {
			t.Errorf("%s: %d %v", name, resp.StatusCode, out)
		}
		if len(h.creations()) != 0 {
			t.Errorf("%s: called WhatsApp", name)
		}
	}
}

func TestBadRequestsNeverReachWhatsApp(t *testing.T) {
	h := newCreateHarness(t)
	many := make([]string, groupMembersMax+1)
	for i := range many {
		many[i] = friendA
	}
	for name, tc := range map[string]struct {
		group   string
		members []string
		result  string
	}{
		"no name":       {"   ", []string{friendA}, "bad_name"},
		"too long":      {strings.Repeat("x", groupNameMax+1), []string{friendA}, "bad_name"},
		"control chars": {"a\u0000b", []string{friendA}, "bad_name"},
		"nobody":        {"Made up", nil, "bad_members"},
		"too many":      {"Made up", many, "bad_members"},
		"only yourself": {"Made up", []string{"15559990000@s.whatsapp.net"}, "bad_members"},
	} {
		if resp, _ := h.create(t, tc.group, tc.members...); resp.StatusCode != 400 || resp.Header.Get(createResultHeader) != tc.result {
			t.Errorf("%s: %d %s", name, resp.StatusCode, resp.Header.Get(createResultHeader))
		}
	}
	if len(h.creations()) != 0 {
		t.Fatal("called WhatsApp")
	}
}

func TestItSaysExactlyWhoWasAndWasntAdded(t *testing.T) {
	h := newCreateHarness(t)
	h.wa.mu.Lock()
	h.wa.createReply = func(req whatsmeow.ReqCreateGroup) *types.GroupInfo {
		return &types.GroupInfo{JID: types.NewJID("120363000000000077", types.GroupServer), Participants: []types.GroupParticipant{
			{JID: req.Participants[0]},
			{JID: req.Participants[1], Error: 403, AddRequest: &types.GroupParticipantAddRequest{Code: "x"}},
		}}
	}
	h.wa.mu.Unlock()
	resp, out := h.create(t, "Made up", friendA, friendB)
	added, _ := out["added"].([]any)
	missed, _ := out["not_added"].([]any)
	if resp.StatusCode != 200 || len(added) != 1 || added[0] != friendA || len(missed) != 1 {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	first := missed[0].(map[string]any)
	if first["jid"] != friendB || first["code"] != float64(403) || first["invited"] != true {
		t.Fatalf("%v", first)
	}
	if got := resp.Header.Get(createCountHeader); got != "added=1 not_added=1" {
		t.Fatal(got)
	}
	// Someone WhatsApp doesn't mention counts as not added.
	h.wa.mu.Lock()
	h.wa.createReply = func(req whatsmeow.ReqCreateGroup) *types.GroupInfo {
		return &types.GroupInfo{JID: types.NewJID("120363000000000078", types.GroupServer), Participants: []types.GroupParticipant{{JID: req.Participants[0]}}}
	}
	h.wa.mu.Unlock()
	_, out = h.create(t, "Made up", friendA, friendB)
	if missed, _ := out["not_added"].([]any); len(missed) != 1 || missed[0].(map[string]any)["jid"] != friendB {
		t.Fatalf("%v", out)
	}
}

func TestWhatsAppsRefusalsSayWhetherAGroupExists(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		result string
		says   string
	}{
		{whatsmeow.ErrIQNotAcceptable, 422, "refused_name", "no group was created"},
		{whatsmeow.ErrIQForbidden, 409, "refused", "no group was created"},
		{whatsmeow.ErrIQRateOverLimit, 429, "refused_rate", "no group was created"},
		{whatsmeow.ErrNotConnected, 503, "not_connected", "no group was created"},
		{whatsmeow.ErrIQTimedOut, 503, "unknown_outcome", "may or may not exist"},
		{errors.New("something else"), 502, "unknown_outcome", "may or may not exist"},
	} {
		h := newCreateHarness(t)
		h.wa.mu.Lock()
		h.wa.createErr = tc.err
		h.wa.mu.Unlock()
		resp, out := h.create(t, "Made up", friendA)
		text, _ := out["error"].(string)
		if resp.StatusCode != tc.status || resp.Header.Get(createResultHeader) != tc.result || !strings.Contains(text, tc.says) {
			t.Errorf("%v: %d %s %q", tc.err, resp.StatusCode, resp.Header.Get(createResultHeader), text)
		}
		if len(h.creations()) != 1 {
			t.Errorf("%v: retried: %d", tc.err, len(h.creations()))
		}
		var chats int
		h.db.QueryRow(`SELECT COUNT(*) FROM chats WHERE jid LIKE '%@g.us'`).Scan(&chats)
		if chats != 0 {
			t.Errorf("%v: stored a chat", tc.err)
		}
	}
}

func (h *pictureHarness) createVia(t *testing.T, via, name string, members ...string) *http.Response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"name": name, "members": members, "via": via})
	resp, err := http.Post(h.srv.URL+"/api/app/group/create", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestTheConnectorStopsSoonerAndCountsTheSameTries(t *testing.T) {
	h := newCreateHarness(t)
	for i := 0; i < connectorCreatePerHour; i++ {
		if resp := h.createVia(t, "connector", "Made up", friendA); resp.StatusCode != 200 {
			t.Fatalf("try %d: %d", i, resp.StatusCode)
		}
	}
	if resp := h.createVia(t, "connector", "Made up", friendA); resp.StatusCode != 429 {
		t.Fatalf("over the connector's limit: %d", resp.StatusCode)
	}
	// The app can still reach its own limit, counting what the connector used.
	for i := connectorCreatePerHour; i < groupCreatePerHour; i++ {
		if resp, _ := h.create(t, "Made up", friendA); resp.StatusCode != 200 {
			t.Fatalf("app try %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := h.create(t, "Made up", friendA); resp.StatusCode != 429 {
		t.Fatalf("the two routes added up past the limit: %d", resp.StatusCode)
	}
	if len(h.creations()) != groupCreatePerHour {
		t.Fatalf("%d calls", len(h.creations()))
	}
}

func TestAppCreationsCountAgainstTheConnector(t *testing.T) {
	h := newCreateHarness(t)
	for i := 0; i < connectorCreatePerHour; i++ {
		if resp, _ := h.create(t, "Made up", friendA); resp.StatusCode != 200 {
			t.Fatalf("app try %d: %d", i, resp.StatusCode)
		}
	}
	if resp := h.createVia(t, "connector", "Made up", friendA); resp.StatusCode != 429 {
		t.Fatalf("the connector ignored what the app used: %d", resp.StatusCode)
	}
}

func TestAJobGroupFromTheConnectorIsSeenByTheJobRule(t *testing.T) {
	h := newCreateHarness(t)
	const job = "YEC-2699 - Made up job"
	if resp := h.createVia(t, "connector", job, friendA); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	var names struct {
		Chats []struct{ JID, Name string }
	}
	h.getJSON("/api/app/group-names", &names)
	for _, c := range names.Chats {
		if c.Name == job {
			return
		}
	}
	t.Fatal("the job-number rule wouldn't see it")
}

func TestCreatingIsLimitedAndTheLimitSurvivesARestart(t *testing.T) {
	h := newCreateHarness(t)
	for i := 0; i < groupCreatePerHour; i++ {
		if resp, _ := h.create(t, "Made up", friendA); resp.StatusCode != 200 {
			t.Fatalf("try %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := h.create(t, "Made up", friendA); resp.StatusCode != 429 || resp.Header.Get(createResultHeader) != "rate_limited" {
		t.Fatalf("over the limit: %d", resp.StatusCode)
	}
	if len(h.creations()) != groupCreatePerHour {
		t.Fatalf("%d calls", len(h.creations()))
	}
	again, err := NewPictures(h.p.dir, h.wa, h.hub, h.app.knownChat)
	if err != nil {
		t.Fatal(err)
	}
	path := h.p.dir + "/group-create-tries.json"
	if again.takeTry(path, time.Now(), groupCreatePerHour, groupCreatePerDay) {
		t.Fatal("a restart reset the limit")
	}
	// A photo change has its own count.
	if !again.takeGroupPhotoTry(time.Now()) {
		t.Fatal("creating groups used up the photo limit")
	}
}
