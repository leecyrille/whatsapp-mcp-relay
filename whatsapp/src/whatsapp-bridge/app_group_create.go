package main

// Creating a real WhatsApp group, which adds and notifies other people. The app reaches this through the relay,
// after the user has seen the name and members and confirmed. The connector reaches it directly, and its tool tells the
// model to confirm with the user first.
//
// Members must be saved contacts, so a pasted or unknown number can't be added, and there's a low limit
// because creating groups quickly adds ban risk. It never retries: a try that may have worked must not make
// a second group.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

const (
	// The library's comment says 25, but real groups have longer names, so WhatsApp decides. This only stops
	// something silly, and a name it refuses is reported.
	groupNameMax       = 100
	groupMembersMax    = 50
	groupCreatePerHour = 5
	groupCreatePerDay  = 15
	// The connector stops sooner than the app, counting the same tries, since a model can repeat itself far faster
	// than the user can. Saying you're the connector can only lower the limit, so nothing needs to check it.
	connectorCreatePerHour = 3
	connectorCreatePerDay  = 10
	groupCreateTimeout = 45 * time.Second

	createResultHeader = "X-Group-Create-Result" // The relay logs these, and they hold no member details.
	createChatHeader   = "X-Group-Create-Chat"
	createCountHeader  = "X-Group-Create-Count"
)

type notAdded struct {
	JID string `json:"jid"`
	// Code is WhatsApp's answer for this person, and 0 when they just weren't in its reply.
	Code int `json:"code"`
	// Invited means WhatsApp offered them an invitation instead.
	Invited bool `json:"invited,omitempty"`
}

func cleanGroupName(name string) (string, bool) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len([]rune(name)) > groupNameMax {
		return "", false
	}
	for _, c := range name {
		if unicode.IsControl(c) {
			return "", false
		}
	}
	return name, true
}

// memberJIDs keeps this to saved contacts. Anything else refuses the whole request, so nobody ends up in a
// group by mistake. The user themself is left out, since WhatsApp adds them.
func (a *App) memberJIDs(ctx context.Context, members []string) ([]types.JID, error) {
	contacts, err := a.wa.Contacts(ctx)
	if err != nil {
		return nil, errors.New("couldn't read your contacts")
	}
	pn, lid := a.wa.Own()
	seen := map[string]bool{}
	out := []types.JID{}
	for _, raw := range members {
		jid, err := parseChatJID(raw)
		if err != nil || jid.Server == types.GroupServer {
			return nil, errors.New("someone in that list isn't one of your contacts")
		}
		canon := a.wa.Canon(jid.String())
		if canon == pn || canon == lid || jid.String() == pn || jid.String() == lid {
			continue
		}
		parsed, err := types.ParseJID(canon)
		if err != nil {
			return nil, errors.New("someone in that list isn't one of your contacts")
		}
		_, known := contacts[parsed]
		if !known {
			_, known = contacts[jid]
		}
		if !known {
			return nil, errors.New("someone in that list isn't one of your contacts")
		}
		if !seen[canon] {
			seen[canon] = true
			out = append(out, parsed)
		}
	}
	return out, nil
}

// addedAndNot compares what was asked with WhatsApp's reply. Someone not listed counts as not added, since
// saying they were would be worse.
func addedAndNot(asked []types.JID, info *types.GroupInfo, canon func(string) string) (added []string, missed []notAdded) {
	replies := map[string]types.GroupParticipant{}
	for _, m := range info.Participants {
		for _, address := range []types.JID{m.JID, m.PhoneNumber, m.LID} {
			if !address.IsEmpty() {
				replies[address.ToNonAD().String()] = m
				replies[canon(address.ToNonAD().String())] = m
			}
		}
	}
	added, missed = []string{}, []notAdded{}
	for _, jid := range asked {
		key := jid.String()
		reply, found := replies[key]
		switch {
		case !found:
			missed = append(missed, notAdded{JID: key})
		case reply.Error != 0:
			missed = append(missed, notAdded{JID: key, Code: reply.Error, Invited: reply.AddRequest != nil})
		default:
			added = append(added, key)
		}
	}
	return added, missed
}

func (a *App) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	refuse := func(status int, result, message string) {
		w.Header().Set(createResultHeader, result)
		fail(w, status, message)
	}
	var req struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
		Via     string   `json:"via"`
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
	if err != nil || json.Unmarshal(raw, &req) != nil {
		refuse(400, "bad_request", "That request couldn't be read.")
		return
	}
	name, ok := cleanGroupName(req.Name)
	if !ok {
		refuse(400, "bad_name", fmt.Sprintf("A group name needs 1 to %d characters.", groupNameMax))
		return
	}
	if len(req.Members) == 0 || len(req.Members) > groupMembersMax {
		refuse(400, "bad_members", fmt.Sprintf("Choose between 1 and %d people.", groupMembersMax))
		return
	}
	members, err := a.memberJIDs(r.Context(), req.Members)
	if err != nil {
		refuse(422, "not_a_contact", "No group was created: "+err.Error()+".")
		return
	}
	if len(members) == 0 {
		refuse(400, "bad_members", "Choose at least one person besides yourself.")
		return
	}
	perHour, perDay := groupCreatePerHour, groupCreatePerDay
	if req.Via == "connector" {
		perHour, perDay = connectorCreatePerHour, connectorCreatePerDay
	}
	if !a.pictures.takeTry(filepath.Join(a.pictures.dir, "group-create-tries.json"), time.Now(), perHour, perDay) {
		refuse(429, "rate_limited", "That's a lot of new groups for now, and creating them quickly can get a number banned. No group was created. Try again later.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), groupCreateTimeout)
	defer cancel()
	info, err := a.wa.CreateGroup(ctx, whatsmeow.ReqCreateGroup{Name: name, Participants: members})
	switch {
	case errors.Is(err, whatsmeow.ErrIQNotAcceptable):
		refuse(422, "refused_name", "WhatsApp refused that, and no group was created. The name may be too long.")
	case errors.Is(err, whatsmeow.ErrIQForbidden), errors.Is(err, whatsmeow.ErrIQNotAuthorized), errors.Is(err, whatsmeow.ErrIQNotAllowed):
		refuse(409, "refused", "WhatsApp refused to create the group, and no group was created.")
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		refuse(429, "refused_rate", "WhatsApp asked to slow down, and no group was created. Try again later.")
	case errors.Is(err, whatsmeow.ErrNotConnected):
		refuse(503, "not_connected", "WhatsApp isn't connected, so no group was created.")
	case errors.Is(err, whatsmeow.ErrIQTimedOut), errors.Is(err, context.DeadlineExceeded):
		refuse(503, "unknown_outcome", "WhatsApp didn't answer, so the group may or may not exist. Check your chats before trying again.")
	case err != nil || info == nil:
		refuse(502, "unknown_outcome", "Something went wrong, so the group may or may not exist. Check your chats before trying again.")
	default:
		jid := info.JID.String()
		// There's no message yet, so this is what makes it a chat that apps and the job-number rule can see.
		if err := a.store.AddCreatedChat(jid, name, time.Now()); err != nil {
			fmt.Printf("Failed to store a new group: %v\n", err)
		}
		added, missed := addedAndNot(members, info, a.wa.Canon)
		w.Header().Set(createResultHeader, "created")
		w.Header().Set(createChatHeader, jid)
		w.Header().Set(createCountHeader, fmt.Sprintf("added=%d not_added=%d", len(added), len(missed)))
		writeJSON(w, 200, map[string]any{"ok": true, "jid": jid, "name": name, "added": added, "not_added": missed})
	}
}
