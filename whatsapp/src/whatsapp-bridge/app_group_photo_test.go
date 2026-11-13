package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"image/color"
	"image/jpeg"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const photoGroup = "120363000000000001@g.us"

func ownAs(admin, superAdmin bool) types.GroupParticipant {
	return types.GroupParticipant{
		JID: types.NewJID("15559990000", types.DefaultUserServer), LID: types.NewJID("11111", types.HiddenUserServer),
		IsAdmin: admin, IsSuperAdmin: superAdmin,
	}
}

func someoneElse() types.GroupParticipant {
	return types.GroupParticipant{JID: types.NewJID("15550000009", types.DefaultUserServer), IsAdmin: true}
}

func groupInfo(locked bool, members ...types.GroupParticipant) *types.GroupInfo {
	return &types.GroupInfo{GroupLocked: types.GroupLocked{IsLocked: locked}, Participants: members}
}

func TestWhoMayChangeAGroupPhoto(t *testing.T) {
	pn, lid := "15559990000@s.whatsapp.net", "11111@lid"
	byLID := ownAs(false, false)
	byLID.JID = byLID.LID
	byLID.LID = types.EmptyJID
	parent := groupInfo(false, ownAs(true, false))
	parent.IsParent = true
	for name, tc := range map[string]struct {
		info *types.GroupInfo
		want groupAccess
	}{
		"admin of a locked group":       {groupInfo(true, ownAs(true, false), someoneElse()), groupAccess{true, "admin"}},
		"owner of a locked group":       {groupInfo(true, ownAs(false, true)), groupAccess{true, "admin"}},
		"member where members may edit": {groupInfo(false, ownAs(false, false), someoneElse()), groupAccess{true, "members_can_edit"}},
		"member of a locked group":      {groupInfo(true, ownAs(false, false), someoneElse()), groupAccess{false, "admins_only"}},
		"only known by LID, locked":     {groupInfo(true, byLID), groupAccess{false, "admins_only"}},
		"only known by LID, not locked": {groupInfo(false, byLID), groupAccess{true, "members_can_edit"}},
		"not listed, even with no lock": {groupInfo(false, someoneElse()), groupAccess{false, "not_member"}},
		"no members listed":             {groupInfo(false), groupAccess{false, "not_member"}},
		"nothing known":                 {nil, groupAccess{false, "unknown"}},
		"a community can't be told":     {parent, groupAccess{false, "unknown"}},
	} {
		if got := groupPhotoAccess(tc.info, pn, lid); got != tc.want {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func newPhotoHarness(t *testing.T, info *types.GroupInfo, infoErr error) *pictureHarness {
	t.Helper()
	h := newPictureHarness(t)
	h.db.Exec(`INSERT INTO app_chat (jid, seq) VALUES (?, 2)`, photoGroup)
	h.wa.mu.Lock()
	h.wa.groupInfo, h.wa.groupInfoErr = info, infoErr
	h.wa.mu.Unlock()
	return h
}

func (h *pictureHarness) photoCalls() [][]byte {
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	return append([][]byte{}, h.wa.photoCalls...)
}

func (h *pictureHarness) postPhoto(t *testing.T, jid string, body []byte) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(h.srv.URL+"/api/app/picture/group?jid="+jid, "application/octet-stream", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestAChangeGoesToWhatsAppAsACleanPhotoAndIsFetchedBack(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(true, ownAs(true, false)), nil)
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "new", URL: "https://pps.whatsapp.net/x"}, nil)
	runPictures(t, h.p)
	resp, out := h.postPhoto(t, photoGroup, pngBytes(t, solid(1000, 800, color.RGBA{0, 200, 0, 255})))
	if resp.StatusCode != 200 || resp.Header.Get(resultHeader) != "changed" || out["picture_id"] != "photo-id" || out["custom_showing"] != false {
		t.Fatalf("%d %v %v", resp.StatusCode, resp.Header, out)
	}
	calls := h.photoCalls()
	if len(calls) != 1 {
		t.Fatalf("%d calls", len(calls))
	}
	img, err := jpeg.Decode(bytes.NewReader(calls[0]))
	if err != nil || img.Bounds().Dx() != 640 || img.Bounds().Dy() != 640 {
		t.Fatalf("sent %v, %v", img.Bounds(), err)
	}
	waitFor(t, "the real picture to be fetched", func() bool { return len(h.p.Versions()) == 1 })
	if got := h.wa.lookups(); len(got) != 1 || got[0] != photoGroup {
		t.Fatalf("lookups: %v", got)
	}
}

func TestACustomPictureIsReportedNotHidden(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, ownAs(false, false)), nil)
	if _, err := h.p.SetOwn(photoGroup, pngBytes(t, solid(50, 50, color.White))); err != nil {
		t.Fatal(err)
	}
	if resp, out := h.postPhoto(t, photoGroup, pngBytes(t, solid(50, 50, color.Black))); resp.StatusCode != 200 || out["custom_showing"] != true {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if _, own := h.p.list(); len(own) != 1 {
		t.Fatalf("the custom picture changed: %v", own)
	}
}

func TestNoCallWhenItIsntAllowedOrCantBeTold(t *testing.T) {
	photo := func(t *testing.T) []byte { return pngBytes(t, solid(50, 50, color.White)) }
	for name, tc := range map[string]struct {
		info   *types.GroupInfo
		err    error
		jid    string
		status int
		result string
	}{
		"admins only":   {groupInfo(true, ownAs(false, false)), nil, photoGroup, 409, "admins_only"},
		"not a member":  {groupInfo(false, someoneElse()), nil, photoGroup, 409, "not_member"},
		"WhatsApp says": {nil, whatsmeow.ErrNotInGroup, photoGroup, 409, "not_member"},
		"can't tell":    {nil, errors.New("timed out"), photoGroup, 409, "unknown"},
		"a person":      {groupInfo(false, ownAs(true, false)), nil, pictureChat, 400, "not_a_group"},
		"unknown group": {groupInfo(false, ownAs(true, false)), nil, "120363999999999999@g.us", 404, "unknown_chat"},
	} {
		h := newPhotoHarness(t, tc.info, tc.err)
		resp, out := h.postPhoto(t, tc.jid, photo(t))
		if resp.StatusCode != tc.status || resp.Header.Get(resultHeader) != tc.result || out["ok"] != false || out["error"] == "" {
			t.Errorf("%s: %d %s %v", name, resp.StatusCode, resp.Header.Get(resultHeader), out)
		}
		if len(h.photoCalls()) != 0 {
			t.Errorf("%s: called WhatsApp", name)
		}
		if _, own := h.p.list(); len(own) != 0 {
			t.Errorf("%s: fell back to a custom picture", name)
		}
	}
}

func TestABadImageNeverReachesWhatsApp(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, ownAs(true, false)), nil)
	for _, bad := range [][]byte{nil, []byte("<svg xmlns='x'/>"), []byte("not an image")} {
		if resp, _ := h.postPhoto(t, photoGroup, bad); resp.StatusCode != 400 || resp.Header.Get(resultHeader) != "bad_image" {
			t.Errorf("%q: %d", bad, resp.StatusCode)
		}
	}
	if len(h.photoCalls()) != 0 {
		t.Fatal("called WhatsApp")
	}
}

func TestWhatsAppsRefusalsAreReportedAndNothingElseChanges(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		result string
		says   string
	}{
		{whatsmeow.ErrInvalidImageFormat, 422, "refused_image", "refused that image"},
		{whatsmeow.ErrIQForbidden, 409, "refused", "refused the change"},
		{whatsmeow.ErrIQNotAuthorized, 409, "refused", "refused the change"},
		{whatsmeow.ErrIQRateOverLimit, 429, "refused_rate", "slow down"},
		{whatsmeow.ErrNotConnected, 503, "unreachable", "may not have changed"},
		{errors.New("something else"), 502, "failed", "something else"},
	} {
		h := newPhotoHarness(t, groupInfo(false, ownAs(true, false)), nil)
		h.wa.mu.Lock()
		h.wa.photoErr = tc.err
		h.wa.mu.Unlock()
		resp, out := h.postPhoto(t, photoGroup, pngBytes(t, solid(50, 50, color.White)))
		text, _ := out["error"].(string)
		if resp.StatusCode != tc.status || resp.Header.Get(resultHeader) != tc.result || !strings.Contains(text, tc.says) {
			t.Errorf("%v: %d %s %q", tc.err, resp.StatusCode, resp.Header.Get(resultHeader), text)
		}
		versions, own := h.p.list()
		if len(own) != 0 || len(versions) != 0 {
			t.Errorf("%v: changed pictures: %v %v", tc.err, versions, own)
		}
	}
}

func TestTheRateLimitCountsTriesAndSurvivesARestart(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, ownAs(true, false)), nil)
	body := pngBytes(t, solid(50, 50, color.White))
	h.wa.mu.Lock()
	h.wa.photoErr = whatsmeow.ErrIQForbidden
	h.wa.mu.Unlock()
	for i := 0; i < groupPhotoPerHour; i++ {
		if resp, _ := h.postPhoto(t, photoGroup, body); resp.StatusCode != 409 {
			t.Fatalf("try %d: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := h.postPhoto(t, photoGroup, body); resp.StatusCode != 429 || resp.Header.Get(resultHeader) != "rate_limited" {
		t.Fatalf("over the limit: %d", resp.StatusCode)
	}
	if len(h.photoCalls()) != groupPhotoPerHour {
		t.Fatalf("%d calls", len(h.photoCalls()))
	}
	again, err := NewPictures(h.p.dir, h.wa, h.hub, h.app.knownChat)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if again.takeGroupPhotoTry(now) {
		t.Fatal("a restart reset the hourly limit")
	}
	if !again.takeGroupPhotoTry(now.Add(time.Hour + time.Minute)) {
		t.Fatal("the hourly limit should clear")
	}
	for i := 0; i < groupPhotoPerDay-groupPhotoPerHour-1; i++ {
		again.takeGroupPhotoTry(now.Add(time.Hour + time.Minute))
	}
	if again.takeGroupPhotoTry(now.Add(3 * time.Hour)) {
		t.Fatal("the daily limit should hold")
	}
	if !again.takeGroupPhotoTry(now.Add(25 * time.Hour)) {
		t.Fatal("the daily limit should clear")
	}
}

func TestAskingWhatsAllowedIsKeptForAMinuteAndOnlyReads(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(true, ownAs(true, false)), nil)
	for i := 0; i < 3; i++ {
		var out groupAccess
		if status := h.getJSON("/api/app/picture/group-access?jid="+photoGroup, &out); status != 200 || !out.CanChange || out.Why != "admin" {
			t.Fatalf("%d %+v", status, out)
		}
	}
	h.wa.mu.Lock()
	asked := h.wa.groupInfoCalls
	h.wa.mu.Unlock()
	if asked != 1 {
		t.Fatalf("asked WhatsApp %d times", asked)
	}
	if len(h.photoCalls()) != 0 {
		t.Fatal("asking changed something")
	}
	var out map[string]any
	if status := h.getJSON("/api/app/picture/group-access?jid="+pictureChat, &out); status != 400 {
		t.Fatalf("a person: %d", status)
	}
}

func TestMembersAreKeptForAWhileAndAskedAgainWhenTheGroupChanges(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(true, ownAs(true, false), someoneElse()), nil)
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	var out struct {
		Members []groupMember `json:"members"`
	}
	for i := 0; i < 3; i++ {
		if status := h.getJSON("/api/app/group/members?jid="+photoGroup, &out); status != 200 {
			t.Fatalf("%d", status)
		}
	}
	if len(out.Members) != 2 || !out.Members[0].Admin || out.Members[0].LID != "11111@lid" || out.Members[0].JID != "15559990000@s.whatsapp.net" {
		t.Fatalf("%+v", out.Members)
	}
	asks := func() int {
		h.wa.mu.Lock()
		defer h.wa.mu.Unlock()
		return h.wa.groupInfoCalls
	}
	if asks() != 1 {
		t.Fatalf("asked %d times", asks())
	}
	group := types.NewJID("120363000000000001", types.GroupServer)
	h.app.handleEvent(&events.GroupInfo{JID: group, Join: []types.JID{types.NewJID("15550000010", types.DefaultUserServer)}})
	if event := string(<-sub); !strings.Contains(event, `"type":"group_members"`) || !strings.Contains(event, photoGroup) {
		t.Fatalf("event: %s", event)
	}
	h.getJSON("/api/app/group/members?jid="+photoGroup, &out)
	if asks() != 2 {
		t.Fatalf("a membership change should ask again: %d", asks())
	}
	h.app.handleEvent(&events.GroupInfo{JID: group, Name: &types.GroupName{Name: "A new made-up name"}})
	h.getJSON("/api/app/group/members?jid="+photoGroup, &out)
	if asks() != 2 {
		t.Fatalf("a rename shouldn't ask again: %d", asks())
	}
}

func TestAFailureKeepsNoMembersButReportsIt(t *testing.T) {
	h := newPhotoHarness(t, nil, errors.New("offline"))
	var out map[string]any
	if status := h.getJSON("/api/app/group/members?jid="+photoGroup, &out); status != 503 || out["ok"] != false {
		t.Fatalf("%d %v", status, out)
	}
	if status := h.getJSON("/api/app/group/members?jid="+pictureChat, &out); status != 400 {
		t.Fatalf("a person: %d", status)
	}
}
