package main

// Renaming and leaving a real WhatsApp group. Other people see both, so each one asks WhatsApp again just
// before acting and never retries. The app reaches both, through the relay, after the user confirms. The connector
// can only rename, and the same limit covers both.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	groupRenamePerHour = 10
	groupRenamePerDay  = 30
	groupLeavePerHour  = 5
	groupLeavePerDay   = 15
	groupActionTimeout = 30 * time.Second

	actionResultHeader = "X-Group-Action-Result" // The relay logs this.
)

func refuseAction(w http.ResponseWriter, status int, result, message string) {
	w.Header().Set(actionResultHeader, result)
	fail(w, status, message)
}

func (a *App) groupActionJID(w http.ResponseWriter, r *http.Request) (types.JID, bool) {
	jid, ok := a.pictureJID(w, r)
	if !ok {
		return types.JID{}, false
	}
	parsed, err := parseChatJID(jid)
	if err != nil || parsed.Server != types.GroupServer {
		refuseAction(w, 400, "not_a_group", "That isn't a group.")
		return types.JID{}, false
	}
	if !a.knownChat(r.Context(), jid) {
		refuseAction(w, 404, "unknown_chat", "That isn't one of your chats.")
		return types.JID{}, false
	}
	return parsed, true
}

// freshGroupInfo asks again, since what's kept could be a minute old and this is about to change something.
func (a *App) freshGroupInfo(r *http.Request, jid types.JID) (*types.GroupInfo, error) {
	ctx, cancel := context.WithTimeout(r.Context(), groupInfoTimeout)
	defer cancel()
	return a.wa.GroupInfo(ctx, jid)
}

// refuseFromWhatsApp reports what WhatsApp said. notDone is true when it can only have said no, so nothing
// changed. maybe is for when it didn't answer, and the change may have happened.
func refuseFromWhatsApp(w http.ResponseWriter, err error, notDone, maybe string) {
	switch {
	case errors.Is(err, whatsmeow.ErrIQForbidden), errors.Is(err, whatsmeow.ErrIQNotAuthorized), errors.Is(err, whatsmeow.ErrIQNotAllowed):
		refuseAction(w, 409, "refused", "WhatsApp refused. "+notDone)
	case errors.Is(err, whatsmeow.ErrIQNotAcceptable):
		refuseAction(w, 422, "refused", "WhatsApp refused that. "+notDone)
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		refuseAction(w, 429, "refused_rate", "WhatsApp asked to slow down. "+notDone+" Try again later.")
	case errors.Is(err, whatsmeow.ErrNotConnected):
		refuseAction(w, 503, "not_connected", "WhatsApp isn't connected. "+notDone)
	case errors.Is(err, whatsmeow.ErrIQTimedOut), errors.Is(err, context.DeadlineExceeded):
		refuseAction(w, 503, "unknown_outcome", "WhatsApp didn't answer. "+maybe)
	default:
		refuseAction(w, 502, "unknown_outcome", "Something went wrong. "+maybe)
	}
}

func (a *App) handleGroupRename(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.groupActionJID(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		refuseAction(w, 400, "bad_request", "That request couldn't be read.")
		return
	}
	name, valid := cleanGroupName(req.Name)
	if !valid {
		refuseAction(w, 400, "bad_name", fmt.Sprintf("A group name needs 1 to %d characters.", groupNameMax))
		return
	}
	info, infoErr := a.freshGroupInfo(r, jid)
	access := a.accessFrom(info, infoErr)
	if !access.CanChange {
		message := "WhatsApp couldn't confirm you may rename this group, so nothing changed."
		if access.Why == "admins_only" {
			message = "Only this group's admins can rename it, so nothing changed."
		} else if access.Why == "not_member" {
			message = "You aren't a member of this group, so nothing changed."
		}
		refuseAction(w, 409, access.Why, message)
		return
	}
	if info.Name == name {
		w.Header().Set(actionResultHeader, "unchanged")
		writeJSON(w, 200, map[string]any{"ok": true, "jid": jid.String(), "name": name, "changed": false})
		return
	}
	p := a.pictures
	if !p.takeTry(filepath.Join(p.dir, "group-rename-tries.json"), time.Now(), groupRenamePerHour, groupRenamePerDay) {
		refuseAction(w, 429, "rate_limited", "That's a lot of group renames for now. Nothing changed. Try again later.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), groupActionTimeout)
	defer cancel()
	if err := a.wa.SetGroupName(ctx, jid, name); err != nil {
		refuseFromWhatsApp(w, err, "The name didn't change.", "The name may or may not have changed, so check before trying again.")
		return
	}
	if err := a.store.RenameChat(jid.String(), name); err != nil {
		fmt.Printf("Failed to store a group name: %v\n", err)
	}
	w.Header().Set(actionResultHeader, "renamed")
	writeJSON(w, 200, map[string]any{"ok": true, "jid": jid.String(), "name": name, "changed": true})
}

func (a *App) handleGroupLeave(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.groupActionJID(w, r)
	if !ok {
		return
	}
	info, infoErr := a.freshGroupInfo(r, jid)
	switch access := a.accessFrom(info, infoErr); access.Why {
	case "not_member":
		refuseAction(w, 409, "not_member", "You've already left this group.")
		return
	case "unknown":
		refuseAction(w, 503, "unknown", "WhatsApp couldn't confirm you're in this group, so you haven't left it.")
		return
	}
	p := a.pictures
	if !p.takeTry(filepath.Join(p.dir, "group-leave-tries.json"), time.Now(), groupLeavePerHour, groupLeavePerDay) {
		refuseAction(w, 429, "rate_limited", "That's a lot of groups to leave for now. You haven't left this one. Try again later.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), groupActionTimeout)
	defer cancel()
	if err := a.wa.LeaveGroup(ctx, jid); err != nil {
		refuseFromWhatsApp(w, err, "You haven't left the group.", "You may or may not have left, so check before trying again.")
		return
	}
	a.groupChanged(jid)
	w.Header().Set(actionResultHeader, "left")
	writeJSON(w, 200, map[string]any{"ok": true, "jid": jid.String()})
}
