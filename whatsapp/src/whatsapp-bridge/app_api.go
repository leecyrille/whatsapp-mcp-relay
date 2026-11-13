package main

// HTTP API used by wa-relay, on 127.0.0.1 only. It has no auth of its own, like the rest of the
// bridge: appGuard below stops browsers reaching it, and wa-relay checks the Access login before
// forwarding anything. The MCP server's /api/send and /api/download are separate and unchanged.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	maxUploadBytes  = 2 << 30 // WhatsApp's own limit for a document.
	maxChunkBytes   = 64 << 20
	maxThumbBytes   = 128 << 10
	maxTextBytes    = 128 << 10
	maxQuoteBytes   = 2 << 10
	mediaCacheBytes = 20 << 30
	mediaRetryWait  = 45 * time.Second
	mediaKeepDays   = 30
	dedupeKeepHours = 2
)

var uploadIDRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// waAPI is the part of WhatsApp the app uses, so tests can run without a real connection.
type waAPI interface {
	Connected() bool
	LoggedIn() bool
	Own() (pn, lid string)
	Send(ctx context.Context, to types.JID, msg *waE2E.Message) (whatsmeow.SendResponse, error)
	// MarkRead tells WhatsApp these messages were read. Quiet sends read-self: the user's own devices clear, and
	// the sender gets no blue ticks.
	MarkRead(ctx context.Context, ids []string, chat, sender types.JID, quiet bool) error
	Contacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
	Aliases(ctx context.Context) ([][2]string, error)
	Upload(ctx context.Context, r io.Reader, t whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	DownloadToFile(ctx context.Context, d whatsmeow.DownloadableMessage, f whatsmeow.File) error
	Canon(jid string) string
	RetryMedia(ctx context.Context, info *types.MessageInfo, key []byte) error
	RequestHistory(ctx context.Context, info *types.MessageInfo, count int) error
	BuildReaction(chat, sender types.JID, id, emoji string) *waE2E.Message
	BuildEdit(chat types.JID, id string, content *waE2E.Message) *waE2E.Message
	BuildRevoke(chat, sender types.JID, id string) *waE2E.Message
	ChatName(jid types.JID, sender string) string
	OwnSender(chat types.JID) types.JID
	ProfilePicture(ctx context.Context, jid types.JID, existingID string) (*types.ProfilePictureInfo, error)
	GroupInfo(ctx context.Context, jid types.JID) (*types.GroupInfo, error)
	// SetGroupPhoto changes what everyone in the group sees. Only app_group_photo.go calls it.
	SetGroupPhoto(ctx context.Context, jid types.JID, photo []byte) (string, error)
	// CreateGroup adds and notifies other people. Only app_group_create.go calls it.
	CreateGroup(ctx context.Context, req whatsmeow.ReqCreateGroup) (*types.GroupInfo, error)
	// Only app_group_admin.go calls these two, and other people see both.
	SetGroupName(ctx context.Context, jid types.JID, name string) error
	LeaveGroup(ctx context.Context, jid types.JID) error
}

// App is the state behind the app API.
type App struct {
	store     *AppStore
	wa        waAPI
	hub       *Hub
	mediaDir  string
	uploadDir string
	mediaCap  int64
	pictures  *Pictures
	names     *Names
	cards     *Cards

	mu        sync.Mutex
	downloads map[string]*dlState
	retries   map[string]chan *events.MediaRetry
	sent      map[string]*sendRecord
	dlSlots   chan struct{}

	reads    readCounts   // how read markers moved, for the log
	incoming atomic.Int64 // messages from others since the last read log line
	labels   groupLabels
}

type dlState struct {
	running bool
	err     string
	at      time.Time
}

type sendRecord struct {
	at   time.Time
	done bool
	resp map[string]any
}

// NewApp prepares the spool and media folders inside the store.
func NewApp(store *AppStore, wa waAPI, hub *Hub, root string) (*App, error) {
	a := &App{
		store: store, wa: wa, hub: hub,
		mediaDir:  filepath.Join(root, "app-media"),
		uploadDir: filepath.Join(root, "app-upload"),
		mediaCap:  mediaCacheBytes,
		downloads: map[string]*dlState{},
		retries:   map[string]chan *events.MediaRetry{},
		sent:      map[string]*sendRecord{},
		dlSlots:   make(chan struct{}, 4),
	}
	store.canon = wa.Canon
	names, err := NewNames(filepath.Join(root, "app-names.json"))
	if err != nil {
		return nil, err
	}
	a.names = names
	cards, err := NewCards(root, names, wa.Canon, func() { a.cardsChanged() })
	if err != nil {
		return nil, err
	}
	a.cards = cards
	pictures, err := NewPictures(filepath.Join(root, "app-pictures"), wa, hub, a.knownChat)
	if err != nil {
		return nil, err
	}
	a.pictures = pictures
	for _, dir := range []string{a.mediaDir, a.uploadDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// appGuard refuses anything a browser could send. Without it a web page open on this PC could POST
// to 127.0.0.1 and send WhatsApp messages. Applies to the MCP server's routes too; its requests
// carry no Origin header and use this Host.
func appGuard(port int, next http.Handler) http.Handler {
	hosts := map[string]bool{
		fmt.Sprintf("127.0.0.1:%d", port): true,
		fmt.Sprintf("localhost:%d", port): true,
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[strings.ToLower(r.Host)] || r.Header.Get("Origin") != "" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// parseChatJID accepts a person or a group, by phone number or LID. Broadcast lists and status are
// refused, the same rule the MCP server applies.
func parseChatJID(s string) (types.JID, error) {
	jid, err := types.ParseJID(s)
	if err != nil {
		return jid, err
	}
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		if jid.User == "" {
			return jid, errors.New("empty address")
		}
		return jid.ToNonAD(), nil
	}
	return jid, fmt.Errorf("can't send to %s", jid.Server)
}

// Register adds the routes to a mux.
func (a *App) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/app/status", a.handleStatus)
	mux.HandleFunc("GET /api/app/sync", a.handleSync)
	mux.HandleFunc("GET /api/app/events", a.handleEvents)
	mux.HandleFunc("GET /api/app/contacts", a.handleContacts)
	mux.HandleFunc("POST /api/app/send", a.handleSend)
	mux.HandleFunc("POST /api/app/react", a.handleReact)
	mux.HandleFunc("POST /api/app/edit", a.handleEdit)
	mux.HandleFunc("POST /api/app/revoke", a.handleRevoke)
	mux.HandleFunc("POST /api/app/hide", a.handleHide)
	mux.HandleFunc("POST /api/app/forward", a.handleForward)
	mux.HandleFunc("POST /api/app/read", a.handleRead)
	mux.HandleFunc("POST /api/app/history", a.handleHistory)
	mux.HandleFunc("GET /api/app/media", a.handleMedia)
	mux.HandleFunc("GET /api/app/upload/{uid}", a.handleUploadStat)
	mux.HandleFunc("PUT /api/app/upload/{uid}", a.handleUploadChunk)
	mux.HandleFunc("POST /api/app/upload/{uid}/finish", a.handleUploadFinish)
	mux.HandleFunc("GET /api/app/group-names", a.handleGroupNames)
	mux.HandleFunc("GET /api/app/pictures", a.handlePictureList)
	mux.HandleFunc("GET /api/app/picture", a.handlePicture)
	mux.HandleFunc("GET /api/app/picture/group-access", a.handleGroupAccess)
	mux.HandleFunc("POST /api/app/picture/group", a.handleGroupPhoto)
	mux.HandleFunc("GET /api/app/group/members", a.handleGroupMembers)
	mux.HandleFunc("POST /api/app/group/create", a.handleGroupCreate)
	mux.HandleFunc("POST /api/app/group/rename", a.handleGroupRename)
	mux.HandleFunc("POST /api/app/group/leave", a.handleGroupLeave)
	mux.HandleFunc("POST /api/app/picture/own", a.handlePictureSet)
	mux.HandleFunc("DELETE /api/app/picture/own", a.handlePictureRemove)
	mux.HandleFunc("POST /api/app/name", a.handleNameSet)
	mux.HandleFunc("GET /api/app/cards", a.handleCards)
	mux.HandleFunc("POST /api/app/cards/save", a.handleCardSave)
	mux.HandleFunc("POST /api/app/cards/delete", a.handleCardDelete)
	mux.HandleFunc("GET /api/app/cards/history", a.handleCardHistory)
	mux.HandleFunc("POST /api/app/cards/undo", a.handleCardUndo)
	mux.HandleFunc("POST /api/app/cards/suggest", a.handleCardSuggest)
	mux.HandleFunc("POST /api/app/cards/import", a.handleCardImport)
	mux.HandleFunc("DELETE /api/app/name", a.handleNameRemove)
}

// handleGroupNames lists group chats and their names, for the relay's project-code rule. Group renames keep
// these current. The relay doesn't forward this route.
func (a *App) handleGroupNames(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.db.QueryContext(r.Context(), `SELECT jid, COALESCE(name, '') FROM chats WHERE jid LIKE '%@g.us'`)
	if err != nil {
		fail(w, 500, "couldn't read chats")
		return
	}
	defer rows.Close()
	type namedChat struct {
		JID  string `json:"jid"`
		Name string `json:"name"`
	}
	chats := []namedChat{}
	for rows.Next() {
		var c namedChat
		if err := rows.Scan(&c.JID, &c.Name); err != nil {
			fail(w, 500, "couldn't read chats")
			return
		}
		chats = append(chats, c)
	}
	writeJSON(w, 200, map[string]any{"chats": chats})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	pn, lid := a.wa.Own()
	writeJSON(w, 200, map[string]any{
		"connected": a.wa.Connected(), "logged_in": a.wa.LoggedIn(),
		"me_pn": pn, "me_lid": lid, "head": a.store.Head(),
	})
}

func (a *App) statusEvent() map[string]any {
	pn, lid := a.wa.Own()
	return map[string]any{
		"type": "status", "connected": a.wa.Connected(), "logged_in": a.wa.LoggedIn(), "head": a.store.Head(),
		"me_pn": pn, "me_lid": lid,
	}
}

// PublishStatus tells clients the link state changed.
func (a *App) PublishStatus() { a.hub.Publish(a.statusEvent()) }

func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := a.store.Sync(after, limit)
	if err != nil {
		fail(w, 500, "sync failed")
		return
	}
	writeJSON(w, 200, page)
}

// handleEvents is the live stream, consumed by wa-relay. A slow reader gets disconnected rather
// than holding up the bridge, then reconnects and pulls what it missed by sequence number.
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "no streaming")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	sub := a.hub.Subscribe()
	defer a.hub.Unsubscribe(sub)
	first, _ := json.Marshal(a.statusEvent())
	fmt.Fprintf(w, "data: %s\n\n", first)
	flusher.Flush()
	beat := time.NewTicker(20 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": hb\n\n")
			flusher.Flush()
		case data, open := <-sub:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (a *App) handleContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := a.wa.Contacts(r.Context())
	if err != nil {
		fail(w, 500, "contacts unavailable")
		return
	}
	rows := make([]contactRow, 0, len(contacts))
	for jid, c := range contacts {
		rows = append(rows, contactRow{jid.String(), c.FullName, c.FirstName, c.PushName, c.BusinessName})
	}
	aliases, err := a.wa.Aliases(r.Context())
	if err != nil {
		aliases = nil
	}
	if aliases == nil {
		aliases = [][2]string{}
	}
	writeJSON(w, 200, map[string]any{"contacts": withNames(rows, a.names.All(), aliases), "aliases": aliases})
}

// remember records a client-generated ID so a retried request can't send twice. It returns the
// earlier record if there was one.
func (a *App) remember(id string) (*sendRecord, bool) {
	if id == "" {
		return &sendRecord{at: time.Now()}, false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-dedupeKeepHours * time.Hour)
	for k, v := range a.sent {
		if v.at.Before(cutoff) {
			delete(a.sent, k)
		}
	}
	if rec, ok := a.sent[id]; ok {
		return rec, true
	}
	rec := &sendRecord{at: time.Now()}
	a.sent[id] = rec
	return rec, false
}

func (a *App) forget(id string) {
	a.mu.Lock()
	delete(a.sent, id)
	a.mu.Unlock()
}

func (a *App) finishRecord(rec *sendRecord, resp map[string]any) {
	a.mu.Lock()
	rec.done, rec.resp = true, resp
	a.mu.Unlock()
}

type quoteRef struct {
	ID     string `json:"id"`
	Sender string `json:"sender"`
	Text   string `json:"text"`
}

func (q *quoteRef) context() *waE2E.ContextInfo {
	if q == nil || q.ID == "" {
		return nil
	}
	ctx := &waE2E.ContextInfo{
		StanzaID:      proto.String(q.ID),
		QuotedMessage: &waE2E.Message{Conversation: proto.String(q.Text)},
	}
	if q.Sender != "" {
		ctx.Participant = proto.String(q.Sender)
	}
	return ctx
}

// sendAndStore sends a message and writes it to the store as if it had been received, which is
// what makes a sent message visible on read-back to the app and the MCP server alike.
func (a *App) sendAndStore(ctx context.Context, jid types.JID, msg *waE2E.Message) (*AppMsg, error) {
	resp, err := a.wa.Send(ctx, jid, msg)
	if err != nil {
		return nil, err
	}
	if err := a.recordSent(jid, msg, resp); err != nil {
		return nil, fmt.Errorf("sent, but not stored: %w", err)
	}
	return a.store.GetMessage(resp.ID, jid.String())
}

// recordSent stores a message that has just been sent, through the same path as a received one.
func (a *App) recordSent(jid types.JID, msg *waE2E.Message, resp whatsmeow.SendResponse) error {
	evt := &events.Message{Message: msg, Info: types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat: jid, Sender: a.wa.OwnSender(jid), IsFromMe: true, IsGroup: jid.Server == types.GroupServer,
		},
		ID: resp.ID, Timestamp: resp.Timestamp,
	}}
	return a.store.Ingest(evt, a.wa.ChatName(jid, ""), "sent")
}

func (a *App) handleSend(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat     string    `json:"chat"`
		Text     string    `json:"text"`
		ClientID string    `json:"client_id"`
		Quote    *quoteRef `json:"quote"`
		Mentions []string  `json:"mentions"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	jid, err := parseChatJID(req.Chat)
	if err != nil {
		fail(w, 400, "bad chat")
		return
	}
	if strings.TrimSpace(req.Text) == "" {
		fail(w, 400, "empty message")
		return
	}
	rec, seen := a.remember(req.ClientID)
	if seen {
		if rec.done {
			writeJSON(w, 200, rec.resp)
		} else {
			fail(w, 409, "already sending")
		}
		return
	}
	text, mentions, refusal := a.resolveMentions(r.Context(), jid, req.Text, req.Mentions)
	if refusal != nil {
		a.forget(req.ClientID)
		failMention(w, refusal)
		return
	}
	req.Text = text
	quote, refusal := a.quoteFor(jid, req.Quote)
	if refusal != nil {
		a.forget(req.ClientID)
		failMention(w, refusal)
		return
	}
	var msg *waE2E.Message
	if ctx := withMentions(quote, mentions); ctx != nil {
		msg = &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(req.Text), ContextInfo: ctx}}
	} else {
		msg = &waE2E.Message{Conversation: proto.String(req.Text)}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	stored, err := a.sendAndStore(ctx, jid, msg)
	if err != nil {
		a.forget(req.ClientID)
		fail(w, 502, err.Error())
		return
	}
	resp := map[string]any{"ok": true, "message": stored}
	a.finishRecord(rec, resp)
	writeJSON(w, 200, resp)
}

func (a *App) handleReact(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat   string `json:"chat"`
		ID     string `json:"id"`
		Sender string `json:"sender_jid"`
		Emoji  string `json:"emoji"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	jid, err := parseChatJID(req.Chat)
	if err != nil || req.ID == "" {
		fail(w, 400, "bad chat")
		return
	}
	stored, err := a.store.GetMessage(req.ID, jid.String())
	if err != nil {
		fail(w, 404, "no such message")
		return
	}
	// Whose message it is comes from the store. The group's own ID here would put the reaction on nobody's message.
	sender, known := a.reactSender(jid, stored)
	if !known {
		fail(w, 409, "This message has no sender recorded, so a reaction can't be sent.")
		return
	}
	msg := a.wa.BuildReaction(jid, sender, req.ID, req.Emoji)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	resp, err := a.wa.Send(ctx, jid, msg)
	if err != nil {
		fail(w, 502, err.Error())
		return
	}
	own := a.wa.OwnSender(jid)
	if err := a.store.Ingest(&events.Message{Message: msg, Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, Sender: own, IsFromMe: true},
		ID:            resp.ID, Timestamp: resp.Timestamp,
	}}, "", ""); err != nil {
		fail(w, 500, "sent, but not stored")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleRead tells WhatsApp the listed messages were read, so the phone clears too, and moves the shared read
// marker. With the user's blue-ticks setting off, the receipts are read-self, which only their own devices see. The
// marker moves even if a receipt fails, so one failure doesn't leave a chat stuck as unread on every device.
func (a *App) handleRead(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat     string `json:"chat"`
		UpToMs   int64  `json:"up_to_ms"`
		Receipts bool   `json:"receipts"`
		Messages []struct {
			ID     string `json:"id"`
			Sender string `json:"sender_jid"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	jid, err := parseChatJID(req.Chat)
	if err != nil {
		fail(w, 400, "bad chat")
		return
	}
	failed := 0
	{
		bySender := map[string][]string{}
		for _, m := range req.Messages {
			if m.ID != "" {
				bySender[m.Sender] = append(bySender[m.Sender], m.ID)
			}
		}
		for sender, ids := range bySender {
			var senderJID types.JID
			if jid.Server == types.GroupServer {
				parsed, err := types.ParseJID(sender)
				if err != nil || sender == "" {
					failed += len(ids)
					continue
				}
				senderJID = parsed
			}
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			err := a.wa.MarkRead(ctx, ids, jid, senderJID, !req.Receipts)
			cancel()
			if err != nil {
				failed += len(ids)
			} else {
				a.reads.add("receipts sent")
			}
		}
		if failed > 0 {
			a.reads.add("receipts failed")
		}
	}
	out, err := a.store.MarkChatRead(jid.String(), req.UpToMs)
	a.reads.outcome("app read", out, err)
	if err != nil {
		fail(w, 500, "couldn't save read state")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "receipts_failed": failed})
}

// handleHistory asks the phone for messages older than the oldest stored for this chat. The answer
// arrives later as a history sync and reaches clients through normal sync. Nothing is sent to the
// other person; the request goes only to the user's own phone.
func (a *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Chat  string `json:"chat"`
		Count int    `json:"count"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	jid, err := parseChatJID(req.Chat)
	if err != nil {
		fail(w, 400, "bad chat")
		return
	}
	id, fromMe, ts, err := a.store.oldestMessage(jid.String())
	if err != nil {
		fail(w, 404, "no messages to anchor on")
		return
	}
	if req.Count < 1 || req.Count > 200 {
		req.Count = 50
	}
	info := &types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, IsFromMe: fromMe, IsGroup: jid.Server == types.GroupServer},
		ID:            id, Timestamp: ts,
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := a.wa.RequestHistory(ctx, info, req.Count); err != nil {
		fail(w, 502, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true})
}

func mediaFileName(chat, id string) string {
	sum := sha256.Sum256([]byte(chat + "|" + id))
	return hex.EncodeToString(sum[:20]) + ".bin"
}

func waMediaType(media string) (whatsmeow.MediaType, bool) {
	switch media {
	case "image", "sticker":
		return whatsmeow.MediaImage, true
	case "video":
		return whatsmeow.MediaVideo, true
	case "audio":
		return whatsmeow.MediaAudio, true
	case "document":
		return whatsmeow.MediaDocument, true
	}
	return "", false
}

// handleMedia serves a message's file. A download from WhatsApp can outlast Cloudflare's 100 second
// wait for a first byte, so it runs in the background: the caller gets 202 and asks again.
func (a *App) handleMedia(w http.ResponseWriter, r *http.Request) {
	chat, id := r.URL.Query().Get("chat"), r.URL.Query().Get("id")
	if chat == "" || id == "" {
		fail(w, 400, "chat and id required")
		return
	}
	row, err := a.store.getMediaRow(id, chat)
	if errors.Is(err, sql.ErrNoRows) {
		fail(w, 404, "no such message")
		return
	} else if err != nil {
		fail(w, 500, "lookup failed")
		return
	}
	mediaType, ok := waMediaType(row.media)
	if !ok {
		fail(w, 404, "not a media message")
		return
	}
	path := filepath.Join(a.mediaDir, mediaFileName(chat, id))
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			fail(w, 500, "read failed")
			return
		}
		now := time.Now()
		_ = os.Chtimes(path, now, now) // Keeps media in use from being pruned.
		contentType := row.mime
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "private, no-store")
		http.ServeContent(w, r, "", info.ModTime(), f)
		return
	}
	a.mu.Lock()
	key := chat + "|" + id
	state := a.downloads[key]
	if state != nil && !state.running && time.Since(state.at) > time.Minute {
		delete(a.downloads, key)
		state = nil
	}
	if state != nil && !state.running {
		msg := state.err
		a.mu.Unlock()
		fail(w, 410, msg)
		return
	}
	if state == nil {
		state = &dlState{running: true, at: time.Now()}
		a.downloads[key] = state
		go a.download(key, id, chat, path, row, mediaType, state)
	}
	a.mu.Unlock()
	writeJSON(w, 202, map[string]any{"ok": true, "state": "downloading", "size": row.size})
}

func (a *App) download(key, id, chat, path string, row *mediaRow, mediaType whatsmeow.MediaType, state *dlState) {
	a.dlSlots <- struct{}{}
	defer func() { <-a.dlSlots }()
	err := a.fetchMedia(id, chat, path, row, mediaType)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err == nil {
		delete(a.downloads, key)
		go a.PruneMedia()
		return
	}
	state.running, state.err, state.at = false, err.Error(), time.Now()
}

var errTooBig = errors.New("this file is bigger than its message said it was, so it was refused")

// cappedFile stops a download once it passes the size the message claimed. A stranger can claim a
// small image and serve a huge file, which would otherwise fill this PC's disk. Written out method by
// method so nothing, such as io.Copy's ReadFrom shortcut, can reach the file around the limit.
type cappedFile struct {
	f     *os.File
	limit int64
}

func (c *cappedFile) Read(p []byte) (int, error)                { return c.f.Read(p) }
func (c *cappedFile) ReadAt(p []byte, off int64) (int, error)   { return c.f.ReadAt(p, off) }
func (c *cappedFile) Seek(off int64, whence int) (int64, error) { return c.f.Seek(off, whence) }
func (c *cappedFile) Truncate(size int64) error                 { return c.f.Truncate(size) }
func (c *cappedFile) Stat() (os.FileInfo, error)                { return c.f.Stat() }
func (c *cappedFile) Write(p []byte) (int, error) {
	at, err := c.f.Seek(0, io.SeekCurrent)
	if err != nil {
		return 0, err
	}
	if at+int64(len(p)) > c.limit {
		return 0, errTooBig
	}
	return c.f.Write(p)
}
func (c *cappedFile) WriteAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > c.limit {
		return 0, errTooBig
	}
	return c.f.WriteAt(p, off)
}

func (a *App) fetchMedia(id, chat, path string, row *mediaRow, mediaType whatsmeow.MediaType) error {
	if len(row.key) == 0 || len(row.encSha) == 0 {
		return errors.New("this media has no download details")
	}
	directPath := row.directPath
	if directPath == "" {
		directPath = extractDirectPathFromURL(row.url)
	}
	tmp := path + ".part"
	defer os.Remove(tmp)
	attempt := func(dp string) error {
		f, err := os.Create(tmp)
		if err != nil {
			return err
		}
		defer f.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
		defer cancel()
		// The slack covers encryption padding and the MAC, which are on disk during the download.
		limit := int64(maxUploadBytes)
		if row.size > 0 && row.size+1024 < limit {
			limit = row.size + 1024
		}
		return a.wa.DownloadToFile(ctx, &MediaDownloader{
			URL: row.url, DirectPath: dp, MediaKey: row.key, FileLength: uint64(row.size),
			FileSHA256: row.sha, FileEncSHA256: row.encSha, MediaType: mediaType,
		}, &cappedFile{f: f, limit: limit})
	}
	err := attempt(directPath)
	if err != nil && isExpiredMedia(err) {
		fresh, retryErr := a.requestReupload(id, chat, row)
		if retryErr != nil {
			return retryErr
		}
		_ = a.store.SetDirectPath(id, chat, fresh)
		err = attempt(fresh)
	}
	if err != nil {
		return fmt.Errorf("download failed: %w", err)
	}
	return os.Rename(tmp, path)
}

func isExpiredMedia(err error) bool {
	return errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith403) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith404) ||
		errors.Is(err, whatsmeow.ErrMediaDownloadFailedWith410)
}

// requestReupload asks the user's phone to upload expired media again, and waits for the new path.
func (a *App) requestReupload(id, chatStr string, row *mediaRow) (string, error) {
	chat, err := types.ParseJID(chatStr)
	if err != nil {
		return "", err
	}
	info := &types.MessageInfo{
		ID: id, Timestamp: row.ts,
		MessageSource: types.MessageSource{Chat: chat, IsFromMe: row.fromMe, IsGroup: chat.Server == types.GroupServer},
	}
	if info.IsGroup && !row.fromMe {
		sender, err := types.ParseJID(row.senderJID)
		if err != nil || row.senderJID == "" {
			return "", errors.New("this media has expired and its sender wasn't recorded")
		}
		info.Sender = sender
	}
	wait := make(chan *events.MediaRetry, 1)
	a.mu.Lock()
	a.retries[id] = wait
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.retries, id)
		a.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.wa.RetryMedia(ctx, info, row.key); err != nil {
		return "", fmt.Errorf("couldn't ask the phone for this media: %w", err)
	}
	select {
	case evt := <-wait:
		notif, err := whatsmeow.DecryptMediaRetryNotification(evt, row.key)
		if err != nil {
			return "", fmt.Errorf("phone couldn't re-send this media: %w", err)
		}
		if notif.GetDirectPath() == "" {
			return "", errors.New("this media is no longer on the phone")
		}
		return notif.GetDirectPath(), nil
	case <-time.After(mediaRetryWait):
		return "", errors.New("the phone didn't respond; it may be offline")
	}
}

// OnMediaRetry hands the phone's reply to the waiting download.
func (a *App) OnMediaRetry(evt *events.MediaRetry) {
	a.mu.Lock()
	wait := a.retries[evt.MessageID]
	a.mu.Unlock()
	if wait != nil {
		select {
		case wait <- evt:
		default:
		}
	}
}

// PruneMedia removes server-side copies not used for a month, abandoned uploads, and the least
// recently used copies once they pass the overall cap.
func (a *App) PruneMedia() {
	sweep := func(dir string, older time.Duration) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > older {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	sweep(a.mediaDir, mediaKeepDays*24*time.Hour)
	sweep(a.uploadDir, 24*time.Hour)
	entries, err := os.ReadDir(a.mediaDir)
	if err != nil {
		return
	}
	type item struct {
		name string
		size int64
		at   time.Time
	}
	var items []item
	var total int64
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		if info, err := e.Info(); err == nil {
			items = append(items, item{e.Name(), info.Size(), info.ModTime()})
			total += info.Size()
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	for _, it := range items {
		if total <= a.mediaCap {
			break
		}
		if os.Remove(filepath.Join(a.mediaDir, it.name)) == nil {
			total -= it.size
		}
	}
}

func (a *App) uploadPath(uid string) (string, bool) {
	if !uploadIDRe.MatchString(uid) {
		return "", false
	}
	return filepath.Join(a.uploadDir, uid+".part"), true
}

func (a *App) handleUploadStat(w http.ResponseWriter, r *http.Request) {
	path, ok := a.uploadPath(r.PathValue("uid"))
	if !ok {
		fail(w, 400, "bad upload id")
		return
	}
	var size int64
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "size": size})
}

// handleUploadChunk appends one chunk. Files arrive in pieces because Cloudflare limits each request
// to 100 MB, and the offset check lets an interrupted upload resume instead of corrupting the file.
func (a *App) handleUploadChunk(w http.ResponseWriter, r *http.Request) {
	path, ok := a.uploadPath(r.PathValue("uid"))
	if !ok {
		fail(w, 400, "bad upload id")
		return
	}
	offset, err := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
	if err != nil || offset < 0 {
		fail(w, 400, "bad offset")
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		fail(w, 500, "spool failed")
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		fail(w, 500, "spool failed")
		return
	}
	if info.Size() != offset {
		writeJSON(w, 409, map[string]any{"ok": false, "error": "wrong offset", "size": info.Size()})
		return
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		fail(w, 500, "spool failed")
		return
	}
	n, err := io.Copy(f, http.MaxBytesReader(w, r.Body, maxChunkBytes))
	if err != nil {
		_ = f.Truncate(offset)
		fail(w, 400, "chunk failed")
		return
	}
	if offset+n > maxUploadBytes {
		_ = f.Close()
		_ = os.Remove(path)
		fail(w, 413, "file is over WhatsApp's 2 GB limit")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "size": offset + n})
}

// uploadMeta describes a finished upload.
type uploadMeta struct {
	Chat     string    `json:"chat"`
	Kind     string    `json:"kind"`
	Filename string    `json:"filename"`
	Caption  string    `json:"caption"`
	ClientID string    `json:"client_id"`
	Thumb    []byte    `json:"thumb"`
	Width    uint32    `json:"width"`
	Height   uint32    `json:"height"`
	Seconds  uint32    `json:"seconds"`
	Quote    *quoteRef `json:"quote"`
	Mentions []string  `json:"mentions"`
	// The quote as checked against the store, set before sending.
	quoteCtx *waE2E.ContextInfo
}

// handleUploadFinish starts the send and returns at once. Encrypting and uploading a big file can
// outlast Cloudflare's timeout, so the result arrives as a send_result event.
func (a *App) handleUploadFinish(w http.ResponseWriter, r *http.Request) {
	path, ok := a.uploadPath(r.PathValue("uid"))
	if !ok {
		fail(w, 400, "bad upload id")
		return
	}
	var meta uploadMeta
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&meta); err != nil {
		fail(w, 400, "bad request")
		return
	}
	jid, err := parseChatJID(meta.Chat)
	if err != nil {
		fail(w, 400, "bad chat")
		return
	}
	if len(meta.Thumb) > maxThumbBytes {
		meta.Thumb = nil
	}
	if meta.ClientID == "" {
		meta.ClientID = r.PathValue("uid")
	}
	rec, seen := a.remember(meta.ClientID)
	if seen {
		if rec.done {
			writeJSON(w, 200, rec.resp)
		} else {
			writeJSON(w, 202, map[string]any{"ok": true, "state": "sending"})
		}
		return
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		a.forget(meta.ClientID)
		fail(w, 400, "nothing uploaded")
		return
	}
	// Checked before the file goes anywhere, so a refusal is shown at once and the file can be sent again.
	caption, mentions, refusal := a.resolveMentions(r.Context(), jid, meta.Caption, meta.Mentions)
	if refusal != nil {
		a.forget(meta.ClientID)
		failMention(w, refusal)
		return
	}
	meta.Caption, meta.Mentions = caption, mentions
	quote, refusal := a.quoteFor(jid, meta.Quote)
	if refusal != nil {
		a.forget(meta.ClientID)
		failMention(w, refusal)
		return
	}
	meta.quoteCtx = quote
	go a.sendUpload(path, jid, meta, rec)
	writeJSON(w, 202, map[string]any{"ok": true, "state": "sending"})
}

func (a *App) sendUpload(path string, jid types.JID, meta uploadMeta, rec *sendRecord) {
	result := map[string]any{"type": "send_result", "client_id": meta.ClientID}
	msg, err := a.buildMediaMessage(path, meta)
	var stored *AppMsg
	if err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		stored, err = a.sendAndStore(ctx, jid, msg)
		cancel()
	}
	_ = os.Remove(path)
	if err != nil {
		a.forget(meta.ClientID)
		result["ok"], result["error"] = false, err.Error()
	} else {
		result["ok"], result["message"] = true, stored
		a.finishRecord(rec, map[string]any{"ok": true, "message": stored})
	}
	a.hub.Publish(result)
}

// detectKind chooses how a file is sent. WhatsApp's players only take certain formats, so anything
// else goes as a document, which always arrives intact.
func detectKind(requested, filename string, head []byte) (kind, mimeType string) {
	sniffed := http.DetectContentType(head)
	sniffed, _, _ = strings.Cut(sniffed, ";")
	byExt := mime.TypeByExtension(strings.ToLower(filepath.Ext(filename)))
	byExt, _, _ = strings.Cut(byExt, ";")
	docMime := byExt
	if docMime == "" {
		docMime = sniffed
	}
	if requested == "document" {
		return "document", docMime
	}
	switch sniffed {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		if requested == "image" {
			return "image", sniffed
		}
	case "video/mp4":
		if requested == "video" {
			return "video", "video/mp4"
		}
	}
	if requested == "audio" {
		switch strings.ToLower(filepath.Ext(filename)) {
		case ".mp3":
			return "audio", "audio/mpeg"
		case ".m4a", ".aac":
			return "audio", "audio/mp4"
		case ".ogg", ".opus":
			return "audio", "audio/ogg; codecs=opus"
		}
	}
	return "document", docMime
}

func (a *App) buildMediaMessage(path string, meta uploadMeta) (*waE2E.Message, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	kind, mimeType := detectKind(meta.Kind, meta.Filename, head[:n])
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	mediaType, _ := waMediaType(kind)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	up, err := a.wa.Upload(ctx, f, mediaType)
	if err != nil {
		return nil, fmt.Errorf("upload to WhatsApp failed: %w", err)
	}
	quote := withMentions(meta.quoteCtx, meta.Mentions)
	var caption *string
	if meta.Caption != "" {
		caption = proto.String(meta.Caption)
	}
	switch kind {
	case "image":
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			Caption: caption, Mimetype: proto.String(mimeType), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			JPEGThumbnail: meta.Thumb, Width: positive(meta.Width), Height: positive(meta.Height), ContextInfo: quote,
		}}, nil
	case "video":
		return &waE2E.Message{VideoMessage: &waE2E.VideoMessage{
			Caption: caption, Mimetype: proto.String(mimeType), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			StreamingSidecar: up.StreamingSidecar, JPEGThumbnail: meta.Thumb,
			Width: positive(meta.Width), Height: positive(meta.Height), Seconds: positive(meta.Seconds), ContextInfo: quote,
		}}, nil
	case "audio":
		return &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
			Mimetype: proto.String(mimeType), URL: &up.URL, DirectPath: &up.DirectPath,
			MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: &up.FileLength,
			Seconds: positive(meta.Seconds), ContextInfo: quote,
		}}, nil
	}
	name := safeFilename(meta.Filename)
	if name == "" {
		name = "file"
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		Caption: caption, Mimetype: proto.String(mimeType), Title: proto.String(name), FileName: proto.String(name),
		URL: &up.URL, DirectPath: &up.DirectPath, MediaKey: up.MediaKey, FileEncSHA256: up.FileEncSHA256,
		FileSHA256: up.FileSHA256, FileLength: &up.FileLength, JPEGThumbnail: meta.Thumb, ContextInfo: quote,
	}}, nil
}

func positive(v uint32) *uint32 {
	if v == 0 {
		return nil
	}
	return proto.Uint32(v)
}
