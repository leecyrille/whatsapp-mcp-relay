package main

// What the app's message menu does: edit, delete for everyone, delete for me, and forward. Also where a reply's
// quote and a reaction get their sender, from the stored message rather than from what the app says, so neither
// can go out naming the wrong person or no one.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	// WhatsApp's help pages: a message can be edited for 15 minutes, and deleted for everyone for about two days.
	// whatsmeow itself allows 20 minutes for an edit, so this is the stricter of the two.
	editWindow   = 15 * time.Minute
	revokeWindow = 60 * time.Hour
	// WhatsApp's own app forwards to at most 5 chats at a time.
	forwardMaxChats = 5
	// Forwarding sends the same thing to several chats quickly, which is the pattern WhatsApp's spam checks
	// watch. The user's typed messages have no limit; forwards do, counted per chat and kept across restarts.
	forwardPerHour = 30
	forwardPerDay  = 100
	forwardGap     = 1500 * time.Millisecond
	maxEditBytes   = 65536
)

var errNoSender = "This message has no sender recorded, so it can't be quoted. Send it without the reply."

// quoteFor builds a reply's quote. In a group the quoted person comes from the stored message: a quote with
// no person, or the wrong one, shows the wrong author on everyone's phone.
func (a *App) quoteFor(chat types.JID, q *quoteRef) (*waE2E.ContextInfo, *mentionRefusal) {
	if q == nil || q.ID == "" {
		return nil, nil
	}
	stored, err := a.store.GetMessage(q.ID, chat.String())
	if chat.Server != types.GroupServer {
		if err == nil && stored.Text != "" {
			q = &quoteRef{ID: q.ID, Sender: q.Sender, Text: stored.Text}
		}
		return q.context(), nil
	}
	if err != nil {
		return nil, &mentionRefusal{409, "The message you're replying to isn't stored here, so nothing was sent. Send it without the reply."}
	}
	sender := stored.SenderJID
	if stored.FromMe {
		sender = a.wa.OwnSender(chat).String()
	}
	if sender == "" {
		return nil, &mentionRefusal{409, errNoSender}
	}
	text := stored.Text
	if text == "" && stored.Media != "" {
		text = "[" + stored.Media + "]"
	}
	return (&quoteRef{ID: q.ID, Sender: sender, Text: text}).context(), nil
}

// reactSender is whose message a reaction is on. A group message with no recorded sender is refused.
func (a *App) reactSender(chat types.JID, stored *AppMsg) (types.JID, bool) {
	if stored.FromMe {
		return a.wa.OwnSender(chat), true
	}
	if chat.Server != types.GroupServer {
		return chat, true
	}
	if stored.SenderJID == "" {
		return types.EmptyJID, false
	}
	parsed, err := types.ParseJID(stored.SenderJID)
	if err != nil {
		return types.EmptyJID, false
	}
	return parsed, true
}

// recordChange stores an edit or delete the user just sent, through the same path as one from their phone.
func (a *App) recordChange(jid types.JID, msg *waE2E.Message, resp whatsmeow.SendResponse) error {
	inner := msg
	if fp := msg.GetEditedMessage(); fp != nil && fp.GetMessage() != nil {
		inner = fp.GetMessage()
	}
	return a.recordSent(jid, inner, resp)
}

type ownMessageReq struct {
	Chat     string   `json:"chat"`
	ID       string   `json:"id"`
	Text     string   `json:"text"`
	Mentions []string `json:"mentions"`
}

func (a *App) readOwnRequest(w http.ResponseWriter, r *http.Request) (*ownMessageReq, types.JID, *AppMsg, bool) {
	var req ownMessageReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return nil, types.EmptyJID, nil, false
	}
	jid, err := parseChatJID(req.Chat)
	if err != nil || req.ID == "" {
		fail(w, 400, "bad chat")
		return nil, types.EmptyJID, nil, false
	}
	stored, err := a.store.GetMessage(req.ID, jid.String())
	if err != nil {
		fail(w, 404, "That message isn't stored here.")
		return nil, types.EmptyJID, nil, false
	}
	return &req, jid, stored, true
}

func (a *App) handleEdit(w http.ResponseWriter, r *http.Request) {
	req, jid, stored, ok := a.readOwnRequest(w, r)
	if !ok {
		return
	}
	switch {
	case !stored.FromMe:
		fail(w, 409, "You can only edit your own messages.")
		return
	case stored.Revoked:
		fail(w, 409, "That message was deleted.")
		return
	case stored.Media != "":
		fail(w, 409, "Only text messages can be edited here.")
		return
	case time.Since(time.UnixMilli(stored.TsMs)) > editWindow:
		fail(w, 409, "WhatsApp only allows editing for 15 minutes after sending.")
		return
	case strings.TrimSpace(req.Text) == "":
		fail(w, 400, "An edit can't be empty. Delete the message instead.")
		return
	case len(req.Text) > maxEditBytes:
		fail(w, 400, "That's too long.")
		return
	}
	text, mentions, refusal := a.resolveMentions(r.Context(), jid, req.Text, req.Mentions)
	if refusal != nil {
		failMention(w, refusal)
		return
	}
	content := &waE2E.Message{Conversation: proto.String(text)}
	if len(mentions) > 0 {
		content = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text), ContextInfo: withMentions(nil, mentions)}}
	}
	msg := a.wa.BuildEdit(jid, req.ID, content)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := a.wa.Send(ctx, jid, msg)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	if err := a.recordChange(jid, msg, resp); err != nil {
		fail(w, 500, "Edited, but not stored here. It will show once your phone syncs it.")
		return
	}
	updated, _ := a.store.GetMessage(req.ID, jid.String())
	writeJSON(w, 200, map[string]any{"ok": true, "message": updated})
}

func (a *App) handleRevoke(w http.ResponseWriter, r *http.Request) {
	req, jid, stored, ok := a.readOwnRequest(w, r)
	if !ok {
		return
	}
	switch {
	case !stored.FromMe:
		fail(w, 409, "You can only delete your own messages for everyone.")
		return
	case stored.Revoked:
		writeJSON(w, 200, map[string]any{"ok": true, "message": stored})
		return
	case time.Since(time.UnixMilli(stored.TsMs)) > revokeWindow:
		fail(w, 409, "WhatsApp only allows deleting for everyone for about 2 days after sending. You can still delete it for you.")
		return
	}
	msg := a.wa.BuildRevoke(jid, types.EmptyJID, req.ID)
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := a.wa.Send(ctx, jid, msg)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	if err := a.recordChange(jid, msg, resp); err != nil {
		fail(w, 500, "Deleted, but not stored here. It will show once your phone syncs it.")
		return
	}
	updated, _ := a.store.GetMessage(req.ID, jid.String())
	writeJSON(w, 200, map[string]any{"ok": true, "message": updated})
}

// handleHide is "delete for me". whatsmeow can receive this from the phone but has no way to send it, so it
// hides the message in WA Relay, on every PC, and leaves it on the phone and for everyone else.
func (a *App) handleHide(w http.ResponseWriter, r *http.Request) {
	req, jid, _, ok := a.readOwnRequest(w, r)
	if !ok {
		return
	}
	if err := a.store.Hide(req.ID, jid.String()); err != nil {
		fail(w, 500, "Couldn't hide it.")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// hideDeletedForMe applies a "delete for me" done on the phone or another device.
func (a *App) hideDeletedForMe(evt *events.DeleteForMe) {
	if err := a.store.Hide(evt.MessageID, evt.ChatJID.String()); err != nil {
		fmt.Printf("Failed to hide a message deleted for me: %v\n", err)
	}
}

type forwardFailure struct {
	Chat  string `json:"chat"`
	Error string `json:"error"`
}

func (a *App) handleForward(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat     string   `json:"chat"`
		ID       string   `json:"id"`
		To       []string `json:"to"`
		ClientID string   `json:"client_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	from, err := parseChatJID(req.Chat)
	if err != nil || req.ID == "" {
		fail(w, 400, "bad chat")
		return
	}
	var targets []types.JID
	seen := map[string]bool{}
	for _, raw := range req.To {
		to, err := parseChatJID(raw)
		if err != nil {
			fail(w, 400, "One of those chats isn't valid, so nothing was forwarded.")
			return
		}
		if !seen[to.String()] {
			seen[to.String()] = true
			targets = append(targets, to)
		}
	}
	if len(targets) == 0 {
		fail(w, 400, "Pick at least one chat.")
		return
	}
	if len(targets) > forwardMaxChats {
		fail(w, 400, fmt.Sprintf("You can forward to up to %d chats at a time, as on WhatsApp.", forwardMaxChats))
		return
	}
	stored, err := a.store.GetMessage(req.ID, from.String())
	if err != nil {
		fail(w, 404, "That message isn't stored here.")
		return
	}
	if stored.Revoked || stored.Hidden {
		fail(w, 409, "That message was deleted.")
		return
	}
	if stored.Media != "" {
		if _, ok := waMediaType(stored.Media); !ok || stored.Media == "sticker" {
			fail(w, 409, "This kind of message can't be forwarded from WA Relay.")
			return
		}
	} else if strings.TrimSpace(stored.Text) == "" {
		fail(w, 409, "There's nothing in that message to forward.")
		return
	}
	if req.ClientID == "" {
		req.ClientID = fmt.Sprintf("fwd-%s-%d", req.ID, time.Now().UnixNano())
	}
	rec, dup := a.remember(req.ClientID)
	if dup {
		if rec.done {
			writeJSON(w, 200, rec.resp)
		} else {
			writeJSON(w, 202, map[string]any{"ok": true, "state": "sending"})
		}
		return
	}
	// Counted now, per chat, so a limit hit names exactly which chats it stopped.
	var allowed []types.JID
	var limited []string
	tries := filepath.Join(a.pictures.dir, "forward-tries.json")
	for _, to := range targets {
		if a.pictures.takeTry(tries, time.Now(), forwardPerHour, forwardPerDay) {
			allowed = append(allowed, to)
		} else {
			limited = append(limited, to.String())
		}
	}
	go a.forward(from, stored, allowed, limited, req.ClientID, rec)
	writeJSON(w, 202, map[string]any{"ok": true, "state": "sending", "limited": limited})
}

// markForwarded sets WhatsApp's forwarded label on whichever kind of message this is. A mention list isn't
// carried over, since it names people in the chat it came from.
func markForwarded(msg *waE2E.Message) {
	ctx := &waE2E.ContextInfo{IsForwarded: proto.Bool(true), ForwardingScore: proto.Uint32(1)}
	switch {
	case msg.ExtendedTextMessage != nil:
		msg.ExtendedTextMessage.ContextInfo = ctx
	case msg.ImageMessage != nil:
		msg.ImageMessage.ContextInfo = ctx
	case msg.VideoMessage != nil:
		msg.VideoMessage.ContextInfo = ctx
	case msg.AudioMessage != nil:
		msg.AudioMessage.ContextInfo = ctx
	case msg.DocumentMessage != nil:
		msg.DocumentMessage.ContextInfo = ctx
	}
}

// forwardContent builds the message once. A file is fetched if needed and uploaded once, and every chat gets
// the same upload, as WhatsApp's own app does.
func (a *App) forwardContent(from types.JID, stored *AppMsg) (*waE2E.Message, error) {
	if stored.Media == "" {
		msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(stored.Text)}}
		markForwarded(msg)
		return msg, nil
	}
	row, err := a.store.getMediaRow(stored.ID, from.String())
	if err != nil {
		return nil, err
	}
	mediaType, _ := waMediaType(row.media)
	path := filepath.Join(a.mediaDir, mediaFileName(from.String(), stored.ID))
	if _, err := os.Stat(path); err != nil {
		a.dlSlots <- struct{}{}
		err := a.fetchMedia(stored.ID, from.String(), path, row, mediaType)
		<-a.dlSlots
		if err != nil {
			return nil, fmt.Errorf("couldn't get the file to forward: %w", err)
		}
	}
	msg, err := a.buildMediaMessage(path, uploadMeta{
		Kind: stored.Media, Filename: stored.Filename, Caption: stored.Text, Thumb: stored.Thumb,
		Width: uint32(stored.Width), Height: uint32(stored.Height), Seconds: uint32(stored.Seconds),
	})
	if err != nil {
		return nil, err
	}
	markForwarded(msg)
	return msg, nil
}

func (a *App) forward(from types.JID, stored *AppMsg, targets []types.JID, limited []string, clientID string, rec *sendRecord) {
	result := map[string]any{"type": "forward_result", "client_id": clientID, "limited": limited}
	sent := []string{}
	failed := []forwardFailure{}
	if len(targets) > 0 {
		msg, err := a.forwardContent(from, stored)
		for i, to := range targets {
			if err != nil {
				failed = append(failed, forwardFailure{to.String(), err.Error()})
				continue
			}
			if i > 0 {
				time.Sleep(forwardGap)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			_, sendErr := a.sendAndStore(ctx, to, proto.Clone(msg).(*waE2E.Message))
			cancel()
			if sendErr != nil {
				failed = append(failed, forwardFailure{to.String(), sendErr.Error()})
			} else {
				sent = append(sent, to.String())
			}
		}
	}
	result["sent"], result["failed"] = sent, failed
	result["ok"] = len(failed) == 0 && len(limited) == 0
	a.finishRecord(rec, result)
	a.hub.Publish(result)
}
