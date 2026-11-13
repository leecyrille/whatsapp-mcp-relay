package main

// Changing a group's real photo on WhatsApp, the one picture call here that other people see. Only the app
// reaches this, through the relay, after the user confirms it. The connector has no tool for it.
//
// What the user may do comes from WhatsApp's own group info, asked again just before each change. If it can't be
// told, the answer is no, and a refusal is reported as one, never turned into a WA Relay-only picture.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	groupPhotoSize    = 640 // What WhatsApp's own apps send.
	groupPhotoPerHour = 5
	groupPhotoPerDay  = 10
	groupAccessKeep   = time.Minute
	groupInfoTimeout  = 15 * time.Second
	groupPhotoTimeout = 30 * time.Second

	resultHeader = "X-Group-Photo-Result" // The relay logs this.
)

type groupAccess struct {
	CanChange bool   `json:"can_change"`
	Why       string `json:"why"`
}

// groupPhotoAccess reads WhatsApp's answer. Anything it can't be sure of is a no.
func groupPhotoAccess(info *types.GroupInfo, pn, lid string) groupAccess {
	if info == nil || info.IsParent {
		return groupAccess{false, "unknown"}
	}
	for _, member := range info.Participants {
		me := false
		for _, address := range []types.JID{member.JID, member.LID, member.PhoneNumber} {
			if text := address.ToNonAD().String(); !address.IsEmpty() && (text == pn || text == lid) {
				me = true
			}
		}
		if !me {
			continue
		}
		switch {
		case member.IsAdmin || member.IsSuperAdmin:
			return groupAccess{true, "admin"}
		case !info.IsLocked:
			return groupAccess{true, "members_can_edit"}
		default:
			return groupAccess{false, "admins_only"}
		}
	}
	return groupAccess{false, "not_member"}
}

func (a *App) accessFrom(info *types.GroupInfo, err error) groupAccess {
	if errors.Is(err, whatsmeow.ErrNotInGroup) {
		return groupAccess{false, "not_member"}
	}
	if err != nil {
		return groupAccess{false, "unknown"}
	}
	pn, lid := a.wa.Own()
	return groupPhotoAccess(info, pn, lid)
}

// groupInfoKept asks WhatsApp at most once per keep, and a failure is only kept for a minute. Each ask is a
// request to WhatsApp, so they only come from the user opening a chat or a menu.
func (a *App) groupInfoKept(ctx context.Context, jid types.JID, keep time.Duration) (*types.GroupInfo, error) {
	p := a.pictures
	key := jid.String()
	p.mu.Lock()
	kept, found := p.groupInfos[key]
	p.mu.Unlock()
	if found && time.Since(kept.at) < keep && (kept.err == nil || time.Since(kept.at) < groupAccessKeep) {
		return kept.info, kept.err
	}
	ctx, cancel := context.WithTimeout(ctx, groupInfoTimeout)
	defer cancel()
	info, err := a.wa.GroupInfo(ctx, jid)
	p.mu.Lock()
	if err != nil && found && kept.err == nil {
		// Keep what's known for the member list, and just note the failure.
		p.groupInfos[key] = keptInfo{info: kept.info, err: err, at: time.Now()}
	} else {
		p.groupInfos[key] = keptInfo{info: info, err: err, at: time.Now()}
	}
	p.mu.Unlock()
	return info, err
}

// groupChanged forgets what's known about a group, and tells apps to ask again.
func (a *App) groupChanged(jid types.JID) {
	p := a.pictures
	if p == nil {
		return
	}
	p.mu.Lock()
	delete(p.groupInfos, jid.String())
	p.mu.Unlock()
	a.hub.Publish(map[string]any{"type": "group_members", "jid": jid.String()})
}

func (a *App) groupPhotoJID(w http.ResponseWriter, r *http.Request) (types.JID, bool) {
	jid, ok := a.pictureJID(w, r)
	if !ok {
		return types.JID{}, false
	}
	parsed, err := parseChatJID(jid)
	if err != nil || parsed.Server != types.GroupServer {
		w.Header().Set(resultHeader, "not_a_group")
		fail(w, 400, "Only a group has a photo you can change here.")
		return types.JID{}, false
	}
	if !a.knownChat(r.Context(), jid) {
		w.Header().Set(resultHeader, "unknown_chat")
		fail(w, 404, "That isn't one of your chats.")
		return types.JID{}, false
	}
	return parsed, true
}

// handleGroupAccess only reads. It's asked when the picture menu opens, and kept for a minute, so opening
// the menu repeatedly doesn't keep asking WhatsApp.
func (a *App) handleGroupAccess(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.groupPhotoJID(w, r)
	if !ok {
		return
	}
	info, err := a.groupInfoKept(r.Context(), jid, groupAccessKeep)
	writeJSON(w, 200, a.accessFrom(info, err))
}

func (a *App) handleGroupPhoto(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.groupPhotoJID(w, r)
	if !ok {
		return
	}
	refuse := func(status int, result, message string) {
		w.Header().Set(resultHeader, result)
		fail(w, status, message)
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPictureBytes+1))
	if err != nil {
		refuse(413, "too_big", "The image must be under 10 MB.")
		return
	}
	photo, err := cleanPictureSized(raw, groupPhotoSize)
	if err != nil {
		refuse(400, "bad_image", "Couldn't use that image: "+err.Error()+".")
		return
	}
	// Asked fresh, since this is about to change something.
	ctxInfo, cancelInfo := context.WithTimeout(r.Context(), groupInfoTimeout)
	defer cancelInfo()
	info, infoErr := a.wa.GroupInfo(ctxInfo, jid)
	access := a.accessFrom(info, infoErr)
	if !access.CanChange {
		message := "WhatsApp couldn't confirm you may change this group's photo, so nothing changed."
		if access.Why == "admins_only" {
			message = "Only this group's admins can change its photo, so nothing changed."
		} else if access.Why == "not_member" {
			message = "You aren't a member of this group, so nothing changed."
		}
		refuse(409, access.Why, message)
		return
	}
	p := a.pictures
	if !p.takeGroupPhotoTry(time.Now()) {
		refuse(429, "rate_limited", "That's a lot of group photo changes for now. Nothing changed. Try again later.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), groupPhotoTimeout)
	defer cancel()
	id, err := a.wa.SetGroupPhoto(ctx, jid, photo)
	switch {
	case errors.Is(err, whatsmeow.ErrInvalidImageFormat):
		refuse(422, "refused_image", "WhatsApp refused that image, so nothing changed.")
	case errors.Is(err, whatsmeow.ErrIQForbidden), errors.Is(err, whatsmeow.ErrIQNotAuthorized), errors.Is(err, whatsmeow.ErrIQNotAllowed):
		refuse(409, "refused", "WhatsApp refused the change. You may not be allowed to change this group's photo.")
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		refuse(429, "refused_rate", "WhatsApp asked to slow down, so nothing changed. Try again later.")
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrIQTimedOut), errors.Is(err, context.DeadlineExceeded):
		refuse(503, "unreachable", "Couldn't reach WhatsApp. The photo may not have changed, so check before trying again.")
	case err != nil:
		refuse(502, "failed", "WhatsApp didn't accept the change: "+err.Error()+".")
	default:
		custom := p.refreshFromWhatsApp(jid.String())
		w.Header().Set(resultHeader, "changed")
		writeJSON(w, 200, map[string]any{"ok": true, "jid": jid.String(), "picture_id": id, "custom_showing": custom})
	}
}

// refreshFromWhatsApp forgets what it knew, so the next lookup fetches the picture WhatsApp now has. It says
// whether a WA Relay-only picture is showing instead.
func (p *Pictures) refreshFromWhatsApp(jid string) bool {
	p.mu.Lock()
	entry := p.entryLocked(jid)
	entry.WAID, entry.Checked = "", 0
	own := entry.Own
	delete(p.groupInfos, jid)
	p.saveLocked()
	p.mu.Unlock()
	p.Enqueue(jid)
	return own
}

func (p *Pictures) triesPath() string { return filepath.Join(p.dir, "group-photo-tries.json") }

// takeGroupPhotoTry counts every try that reaches WhatsApp, refused or not.
func (p *Pictures) takeGroupPhotoTry(now time.Time) bool {
	return p.takeTry(p.triesPath(), now, groupPhotoPerHour, groupPhotoPerDay)
}

// takeTry keeps its count on disk, so restarting doesn't reset it.
func (p *Pictures) takeTry(path string, now time.Time, perHour, perDay int) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	var tries []int64
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &tries)
	}
	kept := tries[:0]
	lastHour := 0
	for _, at := range tries {
		age := now.Sub(time.Unix(at, 0))
		if age < 24*time.Hour {
			kept = append(kept, at)
			if age < time.Hour {
				lastHour++
			}
		}
	}
	if len(kept) >= perDay || lastHour >= perHour {
		return false
	}
	kept = append(kept, now.Unix())
	raw, _ := json.Marshal(kept)
	return writeFileAtomic(path, raw) == nil
}
