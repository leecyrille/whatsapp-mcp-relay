package main

// Real @mentions in a group. WhatsApp shows a mention, and notifies the person, when the text has @digits and
// the message lists that same address. The address has to be the one the group itself uses, which for groups
// using hidden IDs isn't the phone number, so each one is looked up in the group's own member list and the text
// is rewritten to match. Anything unsure is refused and nothing is sent.

import (
	"context"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// The most members a WhatsApp group can have.
const maxMentions = 1024

type mentionRefusal struct {
	status  int
	message string
}

func participantHas(m types.GroupParticipant, jid types.JID) bool {
	for _, known := range []types.JID{m.JID, m.PhoneNumber, m.LID} {
		if !known.IsEmpty() && known.User == jid.User && known.Server == jid.Server {
			return true
		}
	}
	return false
}

func findParticipant(info *types.GroupInfo, jid types.JID) (types.JID, bool) {
	if info == nil {
		return types.EmptyJID, false
	}
	for _, m := range info.Participants {
		if participantHas(m, jid) {
			return m.JID.ToNonAD(), true
		}
	}
	return types.EmptyJID, false
}

// swapMention changes every "@old" that's a whole mention to "@new". An email address or a longer number isn't
// one. Returns how many it found.
func swapMention(text, old, new string) (string, int) {
	mark := "@" + old
	var b strings.Builder
	found, rest := 0, text
	for {
		at := strings.Index(rest, mark)
		if at < 0 {
			b.WriteString(rest)
			return b.String(), found
		}
		before := b.String() + rest[:at]
		after := rest[at+len(mark):]
		prev, _ := utf8.DecodeLastRuneInString(before)
		next, _ := utf8.DecodeRuneInString(after)
		whole := (before == "" || !(unicode.IsLetter(prev) || unicode.IsDigit(prev))) &&
			(after == "" || !(unicode.IsLetter(next) || unicode.IsDigit(next)))
		b.WriteString(rest[:at])
		if whole {
			b.WriteString("@" + new)
			found++
		} else {
			b.WriteString(mark)
		}
		rest = after
	}
}

// resolveMentions returns the text to send and the addresses to list with it.
func (a *App) resolveMentions(ctx context.Context, chat types.JID, text string, asked []string) (string, []string, *mentionRefusal) {
	if len(asked) == 0 {
		return text, nil, nil
	}
	if chat.Server != types.GroupServer {
		return "", nil, &mentionRefusal{400, "Only a group message can mention someone."}
	}
	if len(asked) > maxMentions {
		return "", nil, &mentionRefusal{400, "That's too many mentions for one message."}
	}
	info, err := a.groupInfoKept(ctx, chat, groupMembersKeep)
	if err != nil {
		return "", nil, &mentionRefusal{503, "WhatsApp couldn't confirm this group's members, so nothing was sent."}
	}
	refreshed := false
	out := make([]string, 0, len(asked))
	seen := make(map[string]bool, len(asked))
	for _, raw := range asked {
		jid, err := types.ParseJID(strings.TrimSpace(raw))
		if err != nil || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) || jid.User == "" {
			return "", nil, &mentionRefusal{400, "That mention isn't a person, so nothing was sent."}
		}
		address, found := findParticipant(info, jid)
		if !found && !refreshed {
			// The kept list can be a few minutes old, and they may have only just joined.
			refreshed = true
			fresh, err := a.wa.GroupInfo(ctx, chat)
			if err != nil {
				return "", nil, &mentionRefusal{503, "WhatsApp couldn't confirm this group's members, so nothing was sent."}
			}
			info = fresh
			address, found = findParticipant(info, jid)
		}
		if !found {
			return "", nil, &mentionRefusal{409, "Someone you mentioned isn't in this group any more, so nothing was sent."}
		}
		next, count := swapMention(text, jid.User, address.User)
		if count == 0 {
			return "", nil, &mentionRefusal{400, "A mention doesn't match the text, so nothing was sent."}
		}
		text = next
		if !seen[address.String()] {
			seen[address.String()] = true
			out = append(out, address.String())
		}
	}
	return text, out, nil
}

func withMentions(ctx *waE2E.ContextInfo, mentions []string) *waE2E.ContextInfo {
	if len(mentions) == 0 {
		return ctx
	}
	if ctx == nil {
		ctx = &waE2E.ContextInfo{}
	}
	ctx.MentionedJID = mentions
	return ctx
}

func failMention(w http.ResponseWriter, refusal *mentionRefusal) {
	fail(w, refusal.status, refusal.message)
}
