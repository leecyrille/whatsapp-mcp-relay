package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"google.golang.org/protobuf/proto"
)

const otherGroup = "120363000000000002@g.us"

func (h *pictureHarness) groupAction(t *testing.T, action, jid string, body any) (*http.Response, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	resp, err := http.Post(h.srv.URL+"/api/app/group/"+action+"?jid="+jid, "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func (h *pictureHarness) adminCalls() (renames, leaves []string) {
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	return append([]string{}, h.wa.renames...), append([]string{}, h.wa.leaves...)
}

func (h *pictureHarness) setAdminErr(err error) {
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	h.wa.adminErr = err
}

func someText(text string) *waE2E.Message {
	return &waE2E.Message{Conversation: proto.String(text)}
}

// With a few messages in the group, and some in another group that must stay.
func (h *pictureHarness) fillGroups(t *testing.T) {
	t.Helper()
	at := time.Now().Add(-time.Hour)
	for i, chat := range []string{photoGroup, photoGroup, otherGroup} {
		evt := incoming(t, chat, "15550000009@s.whatsapp.net", "m"+string(rune('a'+i)), at.Add(time.Duration(i)*time.Minute), someText("made up"))
		if err := h.app.store.Ingest(evt, "Made-up name", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func (h *pictureHarness) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	n := -1
	if err := h.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestRenameGoesToWhatsAppAndTheChatList(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, ownAs(false, false), someoneElse()), nil)
	h.fillGroups(t)
	resp, out := h.groupAction(t, "rename", photoGroup, map[string]string{"name": "  New   made-up name "})
	if resp.StatusCode != 200 || resp.Header.Get(actionResultHeader) != "renamed" || out["name"] != "New made-up name" {
		t.Fatalf("%d %v %v", resp.StatusCode, resp.Header, out)
	}
	if renames, _ := h.adminCalls(); len(renames) != 1 || renames[0] != "New made-up name" {
		t.Fatalf("%v", renames)
	}
	if c, err := h.app.store.GetChat(photoGroup); err != nil || c.Name != "New made-up name" {
		t.Fatalf("%v %v", c, err)
	}
}

func TestRenameIsRefusedWhenWhatsAppWouldSayNo(t *testing.T) {
	for name, tc := range map[string]struct {
		info   *types.GroupInfo
		err    error
		status int
		result string
	}{
		"not an admin of a locked group": {groupInfo(true, ownAs(false, false)), nil, 409, "admins_only"},
		"left the group":                 {groupInfo(false, someoneElse()), nil, 409, "not_member"},
		"WhatsApp says not in the group": {nil, whatsmeow.ErrNotInGroup, 409, "not_member"},
		"can't tell":                     {nil, errors.New("made up"), 409, "unknown"},
	} {
		h := newPhotoHarness(t, tc.info, tc.err)
		resp, out := h.groupAction(t, "rename", photoGroup, map[string]string{"name": "Made up"})
		if resp.StatusCode != tc.status || resp.Header.Get(actionResultHeader) != tc.result {
			t.Errorf("%s: %d %v %v", name, resp.StatusCode, resp.Header, out)
		}
		if renames, _ := h.adminCalls(); len(renames) != 0 {
			t.Errorf("%s: reached WhatsApp", name)
		}
	}
}

func TestABadRenameNeverReachesWhatsApp(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, ownAs(true, false)), nil)
	h.db.Exec(`INSERT INTO app_chat (jid, seq) VALUES (?, 3)`, "15550000001@s.whatsapp.net")
	for name, tc := range map[string]struct {
		jid, value string
		status     int
	}{
		"empty":           {photoGroup, "   ", 400},
		"too long":        {photoGroup, strings.Repeat("a", groupNameMax+1), 400},
		"control char":    {photoGroup, "made\x07up", 400},
		"a person":        {"15550000001@s.whatsapp.net", "Made up", 400},
		"not one of ours": {"120363000000000999@g.us", "Made up", 404},
	} {
		resp, _ := h.groupAction(t, "rename", tc.jid, map[string]string{"name": tc.value})
		if resp.StatusCode != tc.status {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if renames, _ := h.adminCalls(); len(renames) != 0 {
		t.Fatalf("%v", renames)
	}
}

func TestRenamingToTheSameNameDoesNothing(t *testing.T) {
	info := groupInfo(false, ownAs(true, false))
	info.Name = "Already this"
	h := newPhotoHarness(t, info, nil)
	resp, out := h.groupAction(t, "rename", photoGroup, map[string]string{"name": "Already this"})
	if resp.StatusCode != 200 || out["changed"] != false {
		t.Fatalf("%d %v", resp.StatusCode, out)
	}
	if renames, _ := h.adminCalls(); len(renames) != 0 {
		t.Fatalf("%v", renames)
	}
}

func TestGroupActionsHaveALimitThatSurvivesARestart(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(false, ownAs(true, false)), nil)
	for i := 0; i < groupRenamePerHour; i++ {
		if resp, _ := h.groupAction(t, "rename", photoGroup, map[string]string{"name": "Made up " + strings.Repeat("x", i+1)}); resp.StatusCode != 200 {
			t.Fatalf("%d: %d", i, resp.StatusCode)
		}
	}
	resp, _ := h.groupAction(t, "rename", photoGroup, map[string]string{"name": "One more"})
	if resp.StatusCode != 429 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(h.p.dir, "group-rename-tries.json")); err != nil {
		t.Fatal(err)
	}
	if renames, _ := h.adminCalls(); len(renames) != groupRenamePerHour {
		t.Fatalf("%d", len(renames))
	}
}

func TestWhatsAppsAnswerIsReportedAndNeverLooksLikeALapsedLogin(t *testing.T) {
	for name, tc := range map[string]struct {
		err    error
		status int
		result string
	}{
		"forbidden":      {whatsmeow.ErrIQForbidden, 409, "refused"},
		"not authorized": {whatsmeow.ErrIQNotAuthorized, 409, "refused"},
		"too fast":       {whatsmeow.ErrIQRateOverLimit, 429, "refused_rate"},
		"not connected":  {whatsmeow.ErrNotConnected, 503, "not_connected"},
		"no answer":      {whatsmeow.ErrIQTimedOut, 503, "unknown_outcome"},
		"other":          {errors.New("made up"), 502, "unknown_outcome"},
	} {
		h := newPhotoHarness(t, groupInfo(false, ownAs(true, false)), nil)
		h.setAdminErr(tc.err)
		for _, action := range []string{"rename", "leave"} {
			resp, _ := h.groupAction(t, action, photoGroup, map[string]string{"name": "Made up"})
			if resp.StatusCode != tc.status || resp.Header.Get(actionResultHeader) != tc.result {
				t.Errorf("%s %s: %d %v", name, action, resp.StatusCode, resp.Header)
			}
		}
	}
}

func TestLeavingAGroup(t *testing.T) {
	h := newPhotoHarness(t, groupInfo(true, ownAs(false, false), someoneElse()), nil)
	h.fillGroups(t)
	resp, out := h.groupAction(t, "leave", photoGroup, nil)
	if resp.StatusCode != 200 || resp.Header.Get(actionResultHeader) != "left" {
		t.Fatalf("%d %v %v", resp.StatusCode, resp.Header, out)
	}
	if _, leaves := h.adminCalls(); len(leaves) != 1 || leaves[0] != photoGroup {
		t.Fatalf("%v", leaves)
	}
	// Leaving keeps the chat, as WhatsApp does. Deleting it is a separate step.
	if n := h.count(t, `SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, photoGroup); n != 2 {
		t.Fatalf("%d", n)
	}
}

func TestLeavingIsRefusedWhenItCantBeSure(t *testing.T) {
	for name, tc := range map[string]struct {
		info   *types.GroupInfo
		err    error
		status int
	}{
		"already left":    {groupInfo(false, someoneElse()), nil, 409},
		"WhatsApp agrees": {nil, whatsmeow.ErrNotInGroup, 409},
		"can't tell":      {nil, errors.New("made up"), 503},
	} {
		h := newPhotoHarness(t, tc.info, tc.err)
		if resp, _ := h.groupAction(t, "leave", photoGroup, nil); resp.StatusCode != tc.status {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
		if _, leaves := h.adminCalls(); len(leaves) != 0 {
			t.Errorf("%s: reached WhatsApp", name)
		}
	}
}

func TestThereIsNoWayToDeleteAGroup(t *testing.T) {
	h := newPhotoHarness(t, nil, whatsmeow.ErrNotInGroup)
	h.fillGroups(t)
	if resp, _ := h.groupAction(t, "delete", photoGroup, nil); resp.StatusCode != 404 && resp.StatusCode != 405 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if n := h.count(t, `SELECT COUNT(*) FROM messages WHERE chat_jid = ?`, photoGroup); n != 2 {
		t.Fatalf("%d", n)
	}
}
