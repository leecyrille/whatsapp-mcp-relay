package main

// The live side of the app API: the real WhatsApp client behind waAPI, the event fan-out, and
// history sync.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Hub fans events out to subscribers. A subscriber that falls behind is dropped, not waited for.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub makes an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

// Subscribe adds a listener.
func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 512)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes a listener.
func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// Publish sends one event to every listener.
func (h *Hub) Publish(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- data:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// liveWA is the real client.
type liveWA struct {
	client  *whatsmeow.Client
	store   *MessageStore
	logger  waLog.Logger
	aliasDB *sql.DB
}

func newLiveWA(client *whatsmeow.Client, store *MessageStore, logger waLog.Logger) *liveWA {
	// Read-only handle on whatsmeow's own database, used only to list the LID to phone number map.
	db, err := sql.Open("sqlite3", "file:store/whatsapp.db?mode=ro&_busy_timeout=5000")
	if err != nil {
		logger.Warnf("Can't open LID map: %v", err)
		db = nil
	}
	return &liveWA{client: client, store: store, logger: logger, aliasDB: db}
}

func (l *liveWA) ProfilePicture(ctx context.Context, jid types.JID, existingID string) (*types.ProfilePictureInfo, error) {
	return l.client.GetProfilePictureInfo(ctx, jid, &whatsmeow.GetProfilePictureParams{ExistingID: existingID})
}

func (l *liveWA) GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error) {
	return l.client.GetGroupInfo(ctx, jid)
}

func (l *liveWA) SetGroupPhoto(ctx context.Context, jid types.JID, photo []byte) (string, error) {
	return l.client.SetGroupPhoto(ctx, jid, photo)
}

func (l *liveWA) CreateGroup(ctx context.Context, req whatsmeow.ReqCreateGroup) (*types.GroupInfo, error) {
	return l.client.CreateGroup(ctx, req)
}

func (l *liveWA) SetGroupName(ctx context.Context, jid types.JID, name string) error {
	return l.client.SetGroupName(ctx, jid, name)
}

func (l *liveWA) LeaveGroup(ctx context.Context, jid types.JID) error {
	return l.client.LeaveGroup(ctx, jid)
}

func (l *liveWA) Connected() bool { return l.client.IsConnected() }
func (l *liveWA) LoggedIn() bool  { return l.client.IsLoggedIn() }

func (l *liveWA) Own() (string, string) {
	if l.client.Store.ID == nil {
		return "", ""
	}
	return l.client.Store.ID.ToNonAD().String(), l.client.Store.GetLID().ToNonAD().String()
}

func (l *liveWA) OwnSender(chat types.JID) types.JID {
	if chat.Server == types.HiddenUserServer {
		if lid := l.client.Store.GetLID(); !lid.IsEmpty() {
			return lid.ToNonAD()
		}
	}
	if l.client.Store.ID == nil {
		return types.EmptyJID
	}
	return l.client.Store.ID.ToNonAD()
}

func (l *liveWA) Send(ctx context.Context, to types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error) {
	return l.client.SendMessage(ctx, to, msg)
}

func (l *liveWA) MarkRead(ctx context.Context, ids []string, chat, sender types.JID, quiet bool) error {
	if quiet {
		return l.client.MarkRead(ctx, ids, time.Now(), chat, sender, types.ReceiptTypeReadSelf)
	}
	return l.client.MarkRead(ctx, ids, time.Now(), chat, sender)
}

func (l *liveWA) Contacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return l.client.Store.Contacts.GetAllContacts(ctx)
}

func (l *liveWA) Aliases(ctx context.Context) ([][2]string, error) {
	if l.aliasDB == nil {
		return nil, nil
	}
	rows, err := l.aliasDB.QueryContext(ctx, `SELECT lid, pn FROM whatsmeow_lid_map`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var lid, pn string
		if err := rows.Scan(&lid, &pn); err != nil {
			return nil, err
		}
		out = append(out, [2]string{lid + "@" + types.HiddenUserServer, pn + "@" + types.DefaultUserServer})
	}
	return out, rows.Err()
}

func (l *liveWA) Upload(ctx context.Context, r io.Reader, t whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return l.client.UploadReader(ctx, r, nil, t)
}

func (l *liveWA) DownloadToFile(ctx context.Context, d whatsmeow.DownloadableMessage, f whatsmeow.File) error {
	return l.client.DownloadToFile(ctx, d, f)
}

// Canon gives one address per person: the phone number when a LID has a known one.
func (l *liveWA) Canon(jid string) string {
	parsed, err := types.ParseJID(jid)
	if err != nil || parsed.Server != types.HiddenUserServer {
		return jid
	}
	pn, err := l.client.Store.LIDs.GetPNForLID(context.Background(), parsed.ToNonAD())
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn.ToNonAD().String()
}

func (l *liveWA) RetryMedia(ctx context.Context, info *types.MessageInfo, key []byte) error {
	return l.client.SendMediaRetryReceipt(ctx, info, key)
}

func (l *liveWA) RequestHistory(ctx context.Context, info *types.MessageInfo, count int) error {
	_, err := l.client.SendPeerMessage(ctx, l.client.BuildHistorySyncRequest(info, count))
	return err
}

func (l *liveWA) BuildEdit(chat types.JID, id string, content *waE2E.Message) *waE2E.Message {
	return l.client.BuildEdit(chat, id, content)
}

func (l *liveWA) BuildRevoke(chat, sender types.JID, id string) *waE2E.Message {
	return l.client.BuildRevoke(chat, sender, id)
}

func (l *liveWA) BuildReaction(chat, sender types.JID, id, emoji string) *waE2E.Message {
	return l.client.BuildReaction(chat, sender, id, emoji)
}

func (l *liveWA) ChatName(jid types.JID, sender string) string {
	return GetChatName(l.client, l.store, jid, jid.String(), nil, sender, l.logger)
}

// handleLiveMessage stores one incoming message event.
func (a *App) handleLiveMessage(client *whatsmeow.Client, store *MessageStore, msg *events.Message, logger waLog.Logger) {
	if msg.Info.Chat.Server == types.BroadcastServer {
		return // Status updates and broadcast lists aren't chats.
	}
	name := GetChatName(client, store, msg.Info.Chat, msg.Info.Chat.String(), nil, msg.Info.Sender.User, logger)
	if err := a.store.Ingest(msg, name, ""); err != nil {
		logger.Warnf("Failed to store message: %v", err)
		return
	}
	a.afterLiveMessage(msg)
	a.noticeUnnamed(msg.Info.Chat)
}

// afterLiveMessage moves the read marker when the user wrote in a chat on another device: they had it open, and
// WhatsApp counts it read up to there. Sends from this PC aren't events, so Claude's sends don't count.
func (a *App) afterLiveMessage(msg *events.Message) {
	if !msg.Info.IsFromMe {
		a.incoming.Add(1)
		return
	}
	if !isReply(msg.Info.ID, msg.Message) {
		return
	}
	out, err := a.store.MarkChatRead(msg.Info.Chat.ToNonAD().String(), msg.Info.Timestamp.UnixMilli())
	a.reads.outcome("own message", out, err)
}

// handleHistorySync stores a history sync. Going through ParseWebMessage gives history messages the same
// sender, caption, thumbnail and quote handling as live ones.
func (a *App) handleHistorySync(client *whatsmeow.Client, store *MessageStore, sync *events.HistorySync, logger waLog.Logger) {
	total := 0
	for _, conv := range sync.Data.GetConversations() {
		chatJID, err := types.ParseJID(conv.GetID())
		if err != nil {
			logger.Warnf("Skipping history for an unreadable chat ID: %v", err)
			continue
		}
		if chatJID.Server == types.BroadcastServer {
			continue
		}
		chat := HistoryChat{
			Name:   GetChatName(client, store, chatJID, conv.GetID(), conv, "", logger),
			Unread: conv.GetUnreadCount(),
		}
		for _, item := range conv.GetMessages() {
			if item == nil || item.GetMessage() == nil {
				continue
			}
			evt, err := client.ParseWebMessage(chatJID, item.GetMessage())
			if err != nil {
				continue
			}
			chat.Messages = append(chat.Messages, evt)
		}
		if len(chat.Messages) == 0 {
			continue
		}
		stored, err := a.store.IngestHistory(conv.GetID(), chat)
		if err != nil {
			logger.Warnf("Failed to store history for a chat: %v", err)
			continue
		}
		total += stored
	}
	fmt.Printf("History sync: %d conversations, %d messages stored.\n", len(sync.Data.GetConversations()), total)
}

// handleEvent routes the WhatsApp events the app cares about.
func (a *App) handleEvent(evt any) {
	switch v := evt.(type) {
	case *events.Receipt:
		if ownRead(v) {
			out, err := a.store.ApplyOwnRead(v)
			a.reads.outcome("phone read", out, err)
			if err != nil {
				fmt.Printf("Failed to store a read marker: %v\n", err)
			}
		} else if err := a.store.ApplyReceipt(v); err != nil {
			fmt.Printf("Failed to store a receipt: %v\n", err)
		}
	case *events.MarkChatAsRead:
		if !v.Action.GetRead() {
			a.reads.add("marked unread") // The app has no unread flag, so this is only counted.
			break
		}
		out, err := a.store.MarkChatRead(v.JID.ToNonAD().String(), v.Timestamp.UnixMilli())
		a.reads.outcome("marked read", out, err)
		if err != nil {
			fmt.Printf("Failed to store a read marker: %v\n", err)
		}
	case *events.GroupInfo:
		relearn := false
		if v.Name != nil {
			if strings.TrimSpace(v.Name.Name) == "" {
				// The name was taken away: it goes back to its members' names.
				if err := a.store.ClearGroupName(v.JID.String()); err != nil {
					fmt.Printf("Failed to store a group name: %v\n", err)
				}
				relearn = true
			} else if err := a.store.RenameChat(v.JID.String(), v.Name.Name); err != nil {
				fmt.Printf("Failed to store a group name: %v\n", err)
			}
		}
		if len(v.Join)+len(v.Leave)+len(v.Promote)+len(v.Demote) > 0 || v.Locked != nil {
			a.groupChanged(v.JID)
			relearn = relearn || len(v.Join)+len(v.Leave) > 0
		}
		if relearn && a.store.IsUnnamedGroup(v.JID.String()) {
			go a.learnGroup(context.Background(), v.JID)
		}
	case *events.JoinedGroup:
		if err := a.store.RenameChat(v.JID.String(), v.GroupName.Name); err != nil {
			fmt.Printf("Failed to store a group name: %v\n", err)
		}
		a.groupChanged(v.JID)
		info := v.GroupInfo
		go func() {
			if err := a.applyGroupInfo(context.Background(), &info); err != nil {
				fmt.Printf("Group with no name: couldn't store its members: %v\n", err)
			}
		}()
	case *events.Contact, *events.PushName, *events.BusinessName:
		a.contactsChanged()
	case *events.MediaRetry:
		a.OnMediaRetry(v)
	case *events.DeleteForMe:
		a.hideDeletedForMe(v)
	case *events.Connected, *events.Disconnected, *events.LoggedOut, *events.StreamReplaced, *events.TemporaryBan, *events.ClientOutdated:
		a.PublishStatus()
		if _, ok := v.(*events.Connected); ok {
			go a.sweepUnnamedGroups(context.Background(), true)
		}
	}
}

var (
	contactsTimerMu sync.Mutex
	contactsTimer   *time.Timer
)

// contactsChanged tells clients to refetch contacts, at most every 5 seconds: a batch of contact
// updates shouldn't cost a refetch each.
func (a *App) contactsChanged() {
	contactsTimerMu.Lock()
	defer contactsTimerMu.Unlock()
	if contactsTimer != nil {
		return
	}
	contactsTimer = time.AfterFunc(5*time.Second, func() {
		contactsTimerMu.Lock()
		contactsTimer = nil
		contactsTimerMu.Unlock()
		a.hub.Publish(map[string]any{"type": "contacts"})
		// A renamed person renames the groups with no name they're in.
		a.sweepUnnamedGroups(context.Background(), false)
	})
}
