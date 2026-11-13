package main

// Who's in a group, for the chat header. It only reads. This sends addresses and admin flags, and the app
// works out names from its contacts, the same way it names message senders.

import (
	"fmt"
	"net/http"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const groupMembersKeep = 5 * time.Minute

type groupMember struct {
	JID   string `json:"jid"`
	Phone string `json:"phone,omitempty"`
	LID   string `json:"lid,omitempty"`
	Admin bool   `json:"admin,omitempty"`
}

func address(jid types.JID) string {
	if jid.IsEmpty() {
		return ""
	}
	return jid.ToNonAD().String()
}

func (a *App) handleGroupMembers(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.groupPhotoJID(w, r)
	if !ok {
		return
	}
	info, err := a.groupInfoKept(r.Context(), jid, groupMembersKeep)
	if err != nil || info == nil {
		fail(w, 503, "Couldn't get this group's members from WhatsApp.")
		return
	}
	if a.store.IsUnnamedGroup(jid.String()) {
		withJID := *info
		withJID.JID = jid
		if err := a.applyGroupInfo(r.Context(), &withJID); err != nil {
			fmt.Printf("Group with no name: couldn't store its members: %v\n", err)
		}
	}
	members := make([]groupMember, 0, len(info.Participants))
	for _, m := range info.Participants {
		members = append(members, groupMember{address(m.JID), address(m.PhoneNumber), address(m.LID), m.IsAdmin || m.IsSuperAdmin})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jid": jid.String(), "members": members})
}
