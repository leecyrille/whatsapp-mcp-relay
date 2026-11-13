package main

// Change tracking for the desktop app. Everything here is additive: the MCP server's chats and
// messages tables keep their shape, and the extra per-message data lives in app_msg and app_chat.
//
// Every change to a message or chat gets a new sequence number, and clients sync by asking for
// everything after the last one they saw. Sequence numbers are assigned and committed under one
// lock, so a client can never see seq N+1 while seq N is still uncommitted.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const appSchema = `
CREATE TABLE IF NOT EXISTS app_msg (
	id TEXT NOT NULL,
	chat_jid TEXT NOT NULL,
	seq INTEGER NOT NULL,
	sender_jid TEXT NOT NULL DEFAULT '',
	push_name TEXT NOT NULL DEFAULT '',
	mime TEXT NOT NULL DEFAULT '',
	direct_path TEXT NOT NULL DEFAULT '',
	thumb BLOB,
	width INTEGER NOT NULL DEFAULT 0,
	height INTEGER NOT NULL DEFAULT 0,
	seconds INTEGER NOT NULL DEFAULT 0,
	quoted_id TEXT NOT NULL DEFAULT '',
	quoted_sender TEXT NOT NULL DEFAULT '',
	quoted_text TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT '',
	edited_ms INTEGER NOT NULL DEFAULT 0,
	revoked INTEGER NOT NULL DEFAULT 0,
	reactions TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (id, chat_jid)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_app_msg_seq ON app_msg(seq);
CREATE TABLE IF NOT EXISTS app_chat (
	jid TEXT PRIMARY KEY,
	seq INTEGER NOT NULL,
	last_read_ms INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_app_chat_seq ON app_chat(seq);
`

// AppMsg is one message as the app sees it.
type AppMsg struct {
	Seq          int64           `json:"seq"`
	ID           string          `json:"id"`
	Chat         string          `json:"chat"`
	Sender       string          `json:"sender"`
	SenderJID    string          `json:"sender_jid,omitempty"`
	PushName     string          `json:"push_name,omitempty"`
	Text         string          `json:"text"`
	TsMs         int64           `json:"ts"`
	FromMe       bool            `json:"from_me"`
	Media        string          `json:"media,omitempty"`
	Filename     string          `json:"filename,omitempty"`
	Mime         string          `json:"mime,omitempty"`
	Size         int64           `json:"size,omitempty"`
	Thumb        []byte          `json:"thumb,omitempty"`
	Width        int             `json:"width,omitempty"`
	Height       int             `json:"height,omitempty"`
	Seconds      int             `json:"seconds,omitempty"`
	QuotedID     string          `json:"quoted_id,omitempty"`
	QuotedSender string          `json:"quoted_sender,omitempty"`
	QuotedText   string          `json:"quoted_text,omitempty"`
	Status       string          `json:"status,omitempty"`
	EditedMs     int64           `json:"edited_ms,omitempty"`
	Revoked      bool            `json:"revoked,omitempty"`
	Reactions    json.RawMessage `json:"reactions,omitempty"`
	// Hidden is "delete for me": gone from WA Relay on every PC, still on the phone and for everyone else.
	Hidden bool `json:"hidden,omitempty"`
}

// AppChat is one chat as the app sees it.
type AppChat struct {
	Seq        int64  `json:"seq"`
	JID        string `json:"jid"`
	Name       string `json:"name"`
	LastMs     int64  `json:"last_ms"`
	LastReadMs int64  `json:"last_read_ms"`
}

// SyncPage is one page of changes.
type SyncPage struct {
	Messages []AppMsg  `json:"messages"`
	Chats    []AppChat `json:"chats"`
	Next     int64     `json:"next"`
	Head     int64     `json:"head"`
	More     bool      `json:"more"`
}

// AppStore owns the app tables and the sequence counter.
type AppStore struct {
	db  *sql.DB
	hub *Hub
	mu  sync.Mutex
	seq int64
	// canon maps a phone number or LID address to one form, so one person compares equal.
	canon func(string) string
}

// NewAppStore creates the tables if needed and gives any message or chat written before they
// existed a sequence number, so a new client receives the whole history.
func NewAppStore(db *sql.DB, hub *Hub) (*AppStore, error) {
	if _, err := db.Exec(appSchema); err != nil {
		return nil, fmt.Errorf("create app tables: %w", err)
	}
	if err := addHiddenColumn(db); err != nil {
		return nil, fmt.Errorf("add hidden column: %w", err)
	}
	if err := addLabelColumn(db); err != nil {
		return nil, fmt.Errorf("add group label: %w", err)
	}
	s := &AppStore{db: db, hub: hub, canon: func(jid string) string { return jid }}
	if err := s.backfill(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *AppStore) backfill() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.db.QueryRow(`SELECT MAX(COALESCE((SELECT MAX(seq) FROM app_msg), 0),
		COALESCE((SELECT MAX(seq) FROM app_chat), 0))`).Scan(&s.seq); err != nil {
		return fmt.Errorf("read sequence: %w", err)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO app_msg (id, chat_jid, seq)
		SELECT m.id, m.chat_jid, ? + ROW_NUMBER() OVER (ORDER BY m.timestamp)
		FROM messages m
		WHERE NOT EXISTS (SELECT 1 FROM app_msg x WHERE x.id = m.id AND x.chat_jid = m.chat_jid)`, s.seq)
	if err != nil {
		return fmt.Errorf("backfill messages: %w", err)
	}
	n, _ := res.RowsAffected()
	seq := s.seq + n
	// Existing chats count as read when tracking starts, or years of history would show as unread.
	res, err = tx.Exec(`INSERT INTO app_chat (jid, seq, last_read_ms)
		SELECT c.jid, ? + ROW_NUMBER() OVER (ORDER BY c.jid), ?
		FROM chats c
		WHERE NOT EXISTS (SELECT 1 FROM app_chat a WHERE a.jid = c.jid)`, seq, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("backfill chats: %w", err)
	}
	n, _ = res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return err
	}
	s.seq = seq + n
	return nil
}

// addHiddenColumn adds "delete for me" to a store made before it existed.
func addHiddenColumn(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(app_msg)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "hidden" {
			found = true
		}
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE app_msg ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`)
	return err
}

// Hide is "delete for me". The row stays, so the MCP server and a later sync still agree on what exists, and
// it gets a new sequence number so every PC drops it.
func (s *AppStore) Hide(id, chat string) error {
	return s.write(true, func(t *txState) error {
		stored, ok := t.resolveMessage(id, chat)
		if !ok {
			return nil
		}
		var hidden bool
		if err := t.tx.QueryRow(`SELECT hidden FROM app_msg WHERE id = ? AND chat_jid = ?`, id, stored).Scan(&hidden); err != nil || hidden {
			return err
		}
		_, err := t.tx.Exec(`UPDATE app_msg SET hidden = 1, seq = ? WHERE id = ? AND chat_jid = ?`, t.bumpMessage(id, stored), id, stored)
		return err
	})
}

// Head is the newest sequence number.
func (s *AppStore) Head() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq
}

// txState collects what one write touched, so the delta can be published after commit.
type txState struct {
	store *AppStore
	tx    *sql.Tx
	next  int64
	msgs  map[[2]string]bool
	chats map[string]bool
}

func (t *txState) nextSeq() int64 {
	t.next++
	return t.next
}

// write runs fn in one transaction under the sequence lock, then announces the changes. When
// fullDelta is false (history sync) it only announces the new head and clients pull the rows.
func (s *AppStore) write(fullDelta bool, fn func(t *txState) error) error {
	s.mu.Lock()
	tx, err := s.db.Begin()
	if err != nil {
		s.mu.Unlock()
		return err
	}
	prev := s.seq
	t := &txState{store: s, tx: tx, next: prev, msgs: map[[2]string]bool{}, chats: map[string]bool{}}
	if err := fn(t); err != nil {
		tx.Rollback()
		s.mu.Unlock()
		return err
	}
	if err := tx.Commit(); err != nil {
		tx.Rollback()
		s.mu.Unlock()
		return err
	}
	s.seq = t.next
	s.mu.Unlock()

	if t.next == prev || s.hub == nil {
		return nil
	}
	if !fullDelta {
		s.hub.Publish(map[string]any{"type": "changed", "seq": t.next})
		return nil
	}
	delta := map[string]any{"type": "delta", "prev": prev, "seq": t.next}
	msgs := make([]AppMsg, 0, len(t.msgs))
	for key := range t.msgs {
		if m, err := s.GetMessage(key[0], key[1]); err == nil {
			msgs = append(msgs, *m)
		}
	}
	chats := make([]AppChat, 0, len(t.chats))
	for jid := range t.chats {
		if c, err := s.GetChat(jid); err == nil {
			chats = append(chats, *c)
		}
	}
	delta["messages"], delta["chats"] = msgs, chats
	s.hub.Publish(delta)
	return nil
}

const appMsgSelect = `SELECT x.seq, m.id, m.chat_jid, COALESCE(m.sender, ''), x.sender_jid, x.push_name,
	COALESCE(m.content, ''), m.timestamp, COALESCE(m.is_from_me, 0), COALESCE(m.media_type, ''),
	COALESCE(m.filename, ''), x.mime, COALESCE(m.file_length, 0), x.thumb, x.width, x.height, x.seconds,
	x.quoted_id, x.quoted_sender, x.quoted_text, x.status, x.edited_ms, x.revoked, x.reactions, x.hidden
	FROM app_msg x JOIN messages m ON m.id = x.id AND m.chat_jid = x.chat_jid `

type rowScanner interface{ Scan(dest ...any) error }

func scanMsg(row rowScanner) (*AppMsg, error) {
	var m AppMsg
	var ts time.Time
	var reactions string
	var size sql.NullInt64
	if err := row.Scan(&m.Seq, &m.ID, &m.Chat, &m.Sender, &m.SenderJID, &m.PushName, &m.Text, &ts, &m.FromMe,
		&m.Media, &m.Filename, &m.Mime, &size, &m.Thumb, &m.Width, &m.Height, &m.Seconds,
		&m.QuotedID, &m.QuotedSender, &m.QuotedText, &m.Status, &m.EditedMs, &m.Revoked, &reactions, &m.Hidden); err != nil {
		return nil, err
	}
	m.TsMs = ts.UnixMilli()
	m.Size = size.Int64
	if reactions != "" {
		m.Reactions = json.RawMessage(reactions)
	}
	return &m, nil
}

// GetMessage reads one message.
func (s *AppStore) GetMessage(id, chat string) (*AppMsg, error) {
	return scanMsg(s.db.QueryRow(appMsgSelect+"WHERE x.id = ? AND x.chat_jid = ?", id, chat))
}

// A group with no name goes by its members' names, kept apart from its name (app_group_label.go).
const appChatSelect = `SELECT a.seq, c.jid,
	COALESCE(NULLIF(c.name, ''), NULLIF(a.label, ''), CASE WHEN c.jid LIKE '%@g.us' THEN '` + unnamedGroup + `' ELSE '' END),
	c.last_message_time, a.last_read_ms
	FROM app_chat a JOIN chats c ON c.jid = a.jid `

func scanChat(row rowScanner) (*AppChat, error) {
	var c AppChat
	var last sql.NullTime
	if err := row.Scan(&c.Seq, &c.JID, &c.Name, &last, &c.LastReadMs); err != nil {
		return nil, err
	}
	if last.Valid {
		c.LastMs = last.Time.UnixMilli()
	}
	return &c, nil
}

// GetChat reads one chat.
func (s *AppStore) GetChat(jid string) (*AppChat, error) {
	return scanChat(s.db.QueryRow(appChatSelect+"WHERE a.jid = ?", jid))
}

// Sync returns changes after a sequence number. Messages are paged. Chats are few, so all those in
// the page's sequence range come along with it.
func (s *AppStore) Sync(after int64, limit int) (*SyncPage, error) {
	if limit < 1 || limit > 2000 {
		limit = 500
	}
	head := s.Head()
	page := &SyncPage{Messages: []AppMsg{}, Chats: []AppChat{}, Head: head}
	rows, err := s.db.Query(appMsgSelect+"WHERE x.seq > ? AND x.seq <= ? ORDER BY x.seq LIMIT ?", after, head, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		m, err := scanMsg(rows)
		if err != nil {
			return nil, err
		}
		page.Messages = append(page.Messages, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	upto := head
	if len(page.Messages) == limit {
		upto = page.Messages[len(page.Messages)-1].Seq
	}
	chatRows, err := s.db.Query(appChatSelect+"WHERE a.seq > ? AND a.seq <= ? ORDER BY a.seq", after, upto)
	if err != nil {
		return nil, err
	}
	defer chatRows.Close()
	for chatRows.Next() {
		c, err := scanChat(chatRows)
		if err != nil {
			return nil, err
		}
		page.Chats = append(page.Chats, *c)
	}
	if err := chatRows.Err(); err != nil {
		return nil, err
	}
	page.Next = upto
	page.More = upto < head
	return page, nil
}

// upsertChat creates or updates a chat, never moving its last-message time backwards.
func (t *txState) upsertChat(jid, name string, last time.Time, unreadFloor int64) error {
	if _, err := t.tx.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET name = CASE WHEN excluded.name != '' THEN excluded.name ELSE chats.name END`,
		jid, name, last); err != nil {
		return err
	}
	var current sql.NullTime
	if err := t.tx.QueryRow(`SELECT last_message_time FROM chats WHERE jid = ?`, jid).Scan(&current); err != nil {
		return err
	}
	if !current.Valid || last.After(current.Time) {
		if _, err := t.tx.Exec(`UPDATE chats SET last_message_time = ? WHERE jid = ?`, last, jid); err != nil {
			return err
		}
	}
	return t.touchChat(jid, unreadFloor)
}

func (t *txState) touchChat(jid string, lastReadMs int64) error {
	_, err := t.tx.Exec(`INSERT INTO app_chat (jid, seq, last_read_ms) VALUES (?, ?, ?)
		ON CONFLICT(jid) DO UPDATE SET seq = excluded.seq, last_read_ms = MAX(app_chat.last_read_ms, excluded.last_read_ms)`,
		jid, t.nextSeq(), lastReadMs)
	if err == nil {
		t.chats[jid] = true
	}
	return err
}

// parsed is everything taken from one protobuf message.
type parsed struct {
	text, media, filename, url, directPath, mime string
	key, sha, encSha                             []byte
	size                                         uint64
	thumb                                        []byte
	width, height, seconds                       uint32
	quotedID, quotedSender, quotedText           string
}

func (p *parsed) empty() bool { return p.text == "" && p.media == "" }

func mediaExt(mime, fallback string) string {
	mime, _, _ = strings.Cut(mime, ";")
	switch strings.TrimSpace(mime) {
	case "image/jpeg":
		return "jpg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "video/mp4":
		return "mp4"
	case "video/quicktime":
		return "mov"
	case "audio/ogg":
		return "ogg"
	case "audio/mpeg":
		return "mp3"
	case "audio/mp4", "audio/aac":
		return "m4a"
	}
	return fallback
}

// safeFilename keeps a remote sender's filename from carrying a path.
func safeFilename(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = strings.TrimRight(path.Base(name), " .") // Windows ignores trailing dots and spaces.
	if name == "" || name == "/" {
		return ""
	}
	return strings.Map(func(r rune) rune {
		// Right-to-left overrides can make "report<RLO>gpj.exe" read as "reportexe.jpg".
		if r < 32 || strings.ContainsRune(`<>:"|?*`, r) || r == 0x200E || r == 0x200F ||
			(r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) {
			return '_'
		}
		return r
	}, name)
}

// parseMessage pulls text, media and quote details out of an unwrapped message.
func parseMessage(id string, msg *waE2E.Message) *parsed {
	p := &parsed{}
	if msg == nil {
		return p
	}
	var ctx *waE2E.ContextInfo
	switch {
	case msg.GetConversation() != "":
		p.text = msg.GetConversation()
	case msg.GetExtendedTextMessage() != nil:
		p.text = msg.GetExtendedTextMessage().GetText()
		ctx = msg.GetExtendedTextMessage().GetContextInfo()
	case msg.GetImageMessage() != nil:
		m := msg.GetImageMessage()
		p.media, p.text, p.mime, p.url, p.directPath = "image", m.GetCaption(), m.GetMimetype(), m.GetURL(), m.GetDirectPath()
		p.key, p.sha, p.encSha, p.size = m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
		p.thumb, p.width, p.height = m.GetJPEGThumbnail(), m.GetWidth(), m.GetHeight()
		p.filename = safeFilename("image_" + id + "." + mediaExt(m.GetMimetype(), "jpg"))
		ctx = m.GetContextInfo()
	case msg.GetVideoMessage() != nil || msg.GetPtvMessage() != nil:
		m := msg.GetVideoMessage()
		if m == nil {
			m = msg.GetPtvMessage()
		}
		p.media, p.text, p.mime, p.url, p.directPath = "video", m.GetCaption(), m.GetMimetype(), m.GetURL(), m.GetDirectPath()
		p.key, p.sha, p.encSha, p.size = m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
		p.thumb, p.width, p.height, p.seconds = m.GetJPEGThumbnail(), m.GetWidth(), m.GetHeight(), m.GetSeconds()
		p.filename = safeFilename("video_" + id + "." + mediaExt(m.GetMimetype(), "mp4"))
		ctx = m.GetContextInfo()
	case msg.GetAudioMessage() != nil:
		m := msg.GetAudioMessage()
		p.media, p.mime, p.url, p.directPath = "audio", m.GetMimetype(), m.GetURL(), m.GetDirectPath()
		p.key, p.sha, p.encSha, p.size = m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
		p.seconds = m.GetSeconds()
		p.filename = safeFilename("audio_" + id + "." + mediaExt(m.GetMimetype(), "ogg"))
		ctx = m.GetContextInfo()
	case msg.GetDocumentMessage() != nil:
		m := msg.GetDocumentMessage()
		p.media, p.text, p.mime, p.url, p.directPath = "document", m.GetCaption(), m.GetMimetype(), m.GetURL(), m.GetDirectPath()
		p.key, p.sha, p.encSha, p.size = m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
		p.thumb = m.GetJPEGThumbnail()
		p.filename = safeFilename(m.GetFileName())
		if p.filename == "" {
			p.filename = safeFilename(m.GetTitle())
		}
		if p.filename == "" {
			p.filename = safeFilename("document_" + id)
		}
		ctx = m.GetContextInfo()
	case msg.GetStickerMessage() != nil:
		m := msg.GetStickerMessage()
		p.media, p.mime, p.url, p.directPath = "sticker", m.GetMimetype(), m.GetURL(), m.GetDirectPath()
		p.key, p.sha, p.encSha, p.size = m.GetMediaKey(), m.GetFileSHA256(), m.GetFileEncSHA256(), m.GetFileLength()
		p.thumb, p.width, p.height = m.GetPngThumbnail(), m.GetWidth(), m.GetHeight()
		p.filename = safeFilename("sticker_" + id + ".webp")
		ctx = m.GetContextInfo()
	case msg.GetContactMessage() != nil:
		p.text = "[Contact: " + msg.GetContactMessage().GetDisplayName() + "]"
		ctx = msg.GetContactMessage().GetContextInfo()
	case msg.GetContactsArrayMessage() != nil:
		p.text = fmt.Sprintf("[%d contacts]", len(msg.GetContactsArrayMessage().GetContacts()))
	case msg.GetLocationMessage() != nil:
		m := msg.GetLocationMessage()
		p.text = strings.TrimSpace(fmt.Sprintf("[Location: %.5f, %.5f] %s", m.GetDegreesLatitude(), m.GetDegreesLongitude(), m.GetName()))
		ctx = m.GetContextInfo()
	case msg.GetLiveLocationMessage() != nil:
		p.text = "[Live location]"
	case msg.GetPollCreationMessage() != nil:
		p.text = "[Poll: " + msg.GetPollCreationMessage().GetName() + "]"
	case msg.GetPollCreationMessageV3() != nil:
		p.text = "[Poll: " + msg.GetPollCreationMessageV3().GetName() + "]"
	case msg.GetEventMessage() != nil:
		p.text = "[Event: " + msg.GetEventMessage().GetName() + "]"
	}
	if ctx != nil && ctx.GetStanzaID() != "" {
		p.quotedID = ctx.GetStanzaID()
		p.quotedSender = ctx.GetParticipant()
		q := parseMessage(ctx.GetStanzaID(), ctx.GetQuotedMessage())
		p.quotedText = q.text
		if p.quotedText == "" && q.media != "" {
			p.quotedText = "[" + q.media + "]"
		}
	}
	// Whatever a stranger's client claims, these travel in every sync page, so they are capped here.
	if len(p.thumb) > maxThumbBytes {
		p.thumb = nil
	}
	p.text = clipUTF8(p.text, maxTextBytes)
	p.quotedText = clipUTF8(p.quotedText, maxQuoteBytes)
	return p
}

func clipUTF8(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return strings.ToValidUTF8(s[:limit], "")
}

// resolveMessage finds which stored chat a message lives in. The same person can be addressed by
// phone number or by LID, so a reaction or receipt may name a different JID from the stored chat.
func (t *txState) resolveMessage(id, chat string) (string, bool) {
	var found string
	if err := t.tx.QueryRow(`SELECT chat_jid FROM app_msg WHERE id = ? AND chat_jid = ?`, id, chat).Scan(&found); err == nil {
		return found, true
	}
	// Only another address of this same chat counts. An ID from elsewhere must never match, or
	// someone who learned a message ID could change a message in a chat they aren't part of.
	rows, err := t.tx.Query(`SELECT chat_jid FROM app_msg WHERE id = ?`, id)
	if err != nil {
		return "", false
	}
	defer rows.Close()
	want := t.store.canon(chat)
	for rows.Next() {
		if rows.Scan(&found) == nil && t.store.canon(found) == want {
			return found, true
		}
	}
	return "", false
}

// mayChange says whether this event's sender may edit or delete a stored message. Clients enforce
// this, but the protocol doesn't, so a modified client could otherwise rewrite or erase anyone's
// message. A group admin deleting someone else's message isn't applied: it can't be told apart from
// a forgery here, and keeping a message that should have gone is the safer mistake.
func (t *txState) mayChange(evt *events.Message, id, stored string, isRevoke bool) (bool, error) {
	var fromMe bool
	var senderJID string
	err := t.tx.QueryRow(`SELECT m.is_from_me, x.sender_jid FROM messages m
		JOIN app_msg x ON x.id = m.id AND x.chat_jid = m.chat_jid WHERE m.id = ? AND m.chat_jid = ?`, id, stored).Scan(&fromMe, &senderJID)
	if err != nil {
		return false, err
	}
	isGroup := strings.HasSuffix(stored, "@"+types.GroupServer)
	if evt.Info.IsFromMe {
		return fromMe || (isRevoke && isGroup), nil
	}
	if fromMe {
		return false, nil
	}
	if !isGroup {
		return true, nil // In a one-to-one chat, only the other person can have written it.
	}
	return t.store.samePerson(evt, senderJID), nil
}

func (s *AppStore) samePerson(evt *events.Message, senderJID string) bool {
	if senderJID == "" {
		return false
	}
	want := s.canon(senderJID)
	for _, candidate := range []types.JID{evt.Info.Sender, evt.Info.SenderAlt} {
		if !candidate.IsEmpty() && s.canon(candidate.ToNonAD().String()) == want {
			return true
		}
	}
	return false
}

// mayReplace stops a live message reusing an existing ID to overwrite someone else's message.
// History comes from the user's own phone, so it is allowed to correct rows.
func (t *txState) mayReplace(evt *events.Message, chat string) (bool, error) {
	var fromMe bool
	var senderJID string
	err := t.tx.QueryRow(`SELECT m.is_from_me, COALESCE(x.sender_jid, '') FROM messages m
		LEFT JOIN app_msg x ON x.id = m.id AND x.chat_jid = m.chat_jid WHERE m.id = ? AND m.chat_jid = ?`, evt.Info.ID, chat).Scan(&fromMe, &senderJID)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if fromMe != evt.Info.IsFromMe {
		return false, nil
	}
	if fromMe || !strings.HasSuffix(chat, "@"+types.GroupServer) {
		return true, nil
	}
	return t.store.samePerson(evt, senderJID), nil
}

func (t *txState) bumpMessage(id, chat string) int64 {
	seq := t.nextSeq()
	t.msgs[[2]string{id, chat}] = true
	return seq
}

// applyEvent stores one message event inside a transaction. Protocol messages (revoke, edit) and
// reactions change an existing message instead of adding a row. Returns false if nothing changed.
func (t *txState) applyEvent(evt *events.Message, chatName, status string, trusted bool) (bool, error) {
	chat := evt.Info.Chat.String()
	msg := evt.Message
	if msg == nil {
		return false, nil
	}
	if proto := msg.GetProtocolMessage(); proto != nil {
		target := proto.GetKey().GetID()
		switch proto.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			stored, ok := t.resolveMessage(target, chat)
			if !ok {
				return false, nil
			}
			if allowed, err := t.mayChange(evt, target, stored, true); err != nil || !allowed {
				return false, err
			}
			if _, err := t.tx.Exec(`UPDATE messages SET content = '', media_type = '', filename = '', url = '',
				media_key = NULL, file_sha256 = NULL, file_enc_sha256 = NULL, file_length = 0 WHERE id = ? AND chat_jid = ?`, target, stored); err != nil {
				return false, err
			}
			_, err := t.tx.Exec(`UPDATE app_msg SET revoked = 1, thumb = NULL, reactions = '', seq = ? WHERE id = ? AND chat_jid = ?`,
				t.bumpMessage(target, stored), target, stored)
			return err == nil, err
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			stored, ok := t.resolveMessage(target, chat)
			if !ok {
				return false, nil
			}
			if allowed, err := t.mayChange(evt, target, stored, false); err != nil || !allowed {
				return false, err
			}
			edited := parseMessage(target, proto.GetEditedMessage())
			if _, err := t.tx.Exec(`UPDATE messages SET content = ? WHERE id = ? AND chat_jid = ?`, edited.text, target, stored); err != nil {
				return false, err
			}
			_, err := t.tx.Exec(`UPDATE app_msg SET edited_ms = ?, seq = ? WHERE id = ? AND chat_jid = ?`,
				evt.Info.Timestamp.UnixMilli(), t.bumpMessage(target, stored), target, stored)
			return err == nil, err
		}
		return false, nil
	}
	if reaction := msg.GetReactionMessage(); reaction != nil {
		target := reaction.GetKey().GetID()
		stored, ok := t.resolveMessage(target, chat)
		if !ok {
			return false, nil
		}
		var raw string
		if err := t.tx.QueryRow(`SELECT reactions FROM app_msg WHERE id = ? AND chat_jid = ?`, target, stored).Scan(&raw); err != nil {
			return false, err
		}
		reactions := map[string]string{}
		if raw != "" {
			_ = json.Unmarshal([]byte(raw), &reactions)
		}
		who := evt.Info.Sender.ToNonAD().String()
		if evt.Info.IsFromMe {
			who = "me"
		}
		if reaction.GetText() == "" {
			delete(reactions, who)
		} else {
			reactions[who] = reaction.GetText()
		}
		out := ""
		if len(reactions) > 0 {
			b, _ := json.Marshal(reactions)
			out = string(b)
		}
		_, err := t.tx.Exec(`UPDATE app_msg SET reactions = ?, seq = ? WHERE id = ? AND chat_jid = ?`,
			out, t.bumpMessage(target, stored), target, stored)
		return err == nil, err
	}

	p := parseMessage(evt.Info.ID, msg)
	if p.empty() {
		return false, nil
	}
	if !trusted {
		if allowed, err := t.mayReplace(evt, chat); err != nil || !allowed {
			return false, err
		}
	}
	if err := t.upsertChat(chat, chatName, evt.Info.Timestamp, 0); err != nil {
		return false, err
	}
	if _, err := t.tx.Exec(`INSERT OR REPLACE INTO messages
		(id, chat_jid, sender, content, timestamp, is_from_me, media_type, filename, url, media_key, file_sha256, file_enc_sha256, file_length)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		evt.Info.ID, chat, evt.Info.Sender.User, p.text, evt.Info.Timestamp, evt.Info.IsFromMe,
		p.media, p.filename, p.url, p.key, p.sha, p.encSha, p.size); err != nil {
		return false, err
	}
	// The status, edit and reaction columns are left alone: a history sync can redeliver a message
	// that has already moved on.
	_, err := t.tx.Exec(`INSERT INTO app_msg (id, chat_jid, seq, sender_jid, push_name, mime, direct_path, thumb,
			width, height, seconds, quoted_id, quoted_sender, quoted_text, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id, chat_jid) DO UPDATE SET seq = excluded.seq, sender_jid = excluded.sender_jid,
			push_name = excluded.push_name, mime = excluded.mime, direct_path = excluded.direct_path,
			thumb = COALESCE(excluded.thumb, app_msg.thumb), width = excluded.width, height = excluded.height,
			seconds = excluded.seconds, quoted_id = excluded.quoted_id, quoted_sender = excluded.quoted_sender,
			quoted_text = excluded.quoted_text,
			status = CASE WHEN app_msg.status = '' THEN excluded.status ELSE app_msg.status END`,
		evt.Info.ID, chat, t.bumpMessage(evt.Info.ID, chat), evt.Info.Sender.ToNonAD().String(), evt.Info.PushName,
		p.mime, p.directPath, p.thumb, p.width, p.height, p.seconds, p.quotedID, p.quotedSender, p.quotedText, status)
	return err == nil, err
}

// Ingest stores one live message and publishes it.
func (s *AppStore) Ingest(evt *events.Message, chatName, status string) error {
	return s.write(true, func(t *txState) error {
		_, err := t.applyEvent(evt, chatName, status, false)
		return err
	})
}

// HistoryChat is one conversation from a history sync.
type HistoryChat struct {
	Name     string
	Unread   uint32
	Messages []*events.Message
}

// IngestHistory stores one conversation in one transaction. A chat first seen here takes its unread
// count from the phone by placing the read marker just before the Nth newest incoming message.
func (s *AppStore) IngestHistory(jid string, chat HistoryChat) (int, error) {
	stored := 0
	err := s.write(false, func(t *txState) error {
		newest := time.Time{}
		for _, evt := range chat.Messages {
			changed, err := t.applyEvent(evt, chat.Name, "", true)
			if err != nil {
				return err
			}
			if changed {
				stored++
			}
			if evt.Info.Timestamp.After(newest) {
				newest = evt.Info.Timestamp
			}
		}
		if newest.IsZero() {
			return nil
		}
		var readMs int64
		if err := t.tx.QueryRow(`SELECT last_read_ms FROM app_chat WHERE jid = ?`, jid).Scan(&readMs); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if readMs > 0 {
			return nil
		}
		marker := newest.UnixMilli()
		if chat.Unread > 0 {
			var nth sql.NullTime
			err := t.tx.QueryRow(`SELECT timestamp FROM messages WHERE chat_jid = ? AND is_from_me = 0
				ORDER BY timestamp DESC LIMIT 1 OFFSET ?`, jid, chat.Unread-1).Scan(&nth)
			if err == nil && nth.Valid {
				marker = nth.Time.UnixMilli() - 1
			}
		}
		_, err := t.tx.Exec(`UPDATE app_chat SET last_read_ms = ? WHERE jid = ?`, marker, jid)
		return err
	})
	return stored, err
}

var statusRank = map[string]int{"": 0, "sent": 1, "delivered": 2, "read": 3, "played": 4}

// ApplyReceipt records delivered, read or played on messages I sent. Status only moves forward.
// A read from the user's own account is them reading on another device, and moves the chat's read marker instead.
func (s *AppStore) ApplyReceipt(evt *events.Receipt) error {
	if ownRead(evt) {
		_, err := s.ApplyOwnRead(evt)
		return err
	}
	var status string
	switch evt.Type {
	case types.ReceiptTypeDelivered:
		status = "delivered"
	case types.ReceiptTypeRead:
		status = "read"
	case types.ReceiptTypePlayed:
		status = "played"
	default:
		return nil
	}
	chat := evt.Chat.String()
	return s.write(true, func(t *txState) error {
		for _, id := range evt.MessageIDs {
			stored, ok := t.resolveMessage(id, chat)
			if !ok {
				continue
			}
			var current string
			var fromMe bool
			if err := t.tx.QueryRow(`SELECT x.status, m.is_from_me FROM app_msg x
				JOIN messages m ON m.id = x.id AND m.chat_jid = x.chat_jid
				WHERE x.id = ? AND x.chat_jid = ?`, id, stored).Scan(&current, &fromMe); err != nil {
				return err
			}
			if !fromMe || statusRank[status] <= statusRank[current] {
				continue
			}
			if _, err := t.tx.Exec(`UPDATE app_msg SET status = ?, seq = ? WHERE id = ? AND chat_jid = ?`,
				status, t.bumpMessage(id, stored), id, stored); err != nil {
				return err
			}
		}
		return nil
	})
}

// RenameChat records a new group name.
func (s *AppStore) RenameChat(chat, name string) error {
	if name == "" {
		return nil
	}
	return s.write(true, func(t *txState) error {
		res, err := t.tx.Exec(`UPDATE chats SET name = ? WHERE jid = ? AND COALESCE(name, '') != ?`, name, chat, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return t.touchChat(chat, 0)
	})
}

// AddCreatedChat records a group the user just made. It has no message yet, and this is what makes it a chat.
func (s *AppStore) AddCreatedChat(chat, name string, at time.Time) error {
	return s.write(true, func(t *txState) error {
		if _, err := t.tx.Exec(`INSERT INTO chats (jid, name, last_message_time) VALUES (?, ?, ?)
			ON CONFLICT(jid) DO UPDATE SET name = excluded.name`, chat, name, at); err != nil {
			return err
		}
		return t.touchChat(chat, 0)
	})
}

// SetDirectPath saves a new CDN path after the phone re-uploads expired media.
func (s *AppStore) SetDirectPath(id, chat, directPath string) error {
	_, err := s.db.ExecContext(context.Background(), `UPDATE app_msg SET direct_path = ? WHERE id = ? AND chat_jid = ?`, directPath, id, chat)
	return err
}

// mediaRow is what a download needs.
type mediaRow struct {
	media, filename, url, directPath, mime, senderJID string
	key, sha, encSha                                  []byte
	size                                              int64
	fromMe                                            bool
	ts                                                time.Time
}

func (s *AppStore) getMediaRow(id, chat string) (*mediaRow, error) {
	var r mediaRow
	var size sql.NullInt64
	var url, filename, media sql.NullString
	err := s.db.QueryRow(`SELECT m.media_type, m.filename, m.url, x.direct_path, x.mime, x.sender_jid,
		m.media_key, m.file_sha256, m.file_enc_sha256, m.file_length, m.is_from_me, m.timestamp
		FROM messages m JOIN app_msg x ON x.id = m.id AND x.chat_jid = m.chat_jid
		WHERE m.id = ? AND m.chat_jid = ?`, id, chat).Scan(&media, &filename, &url, &r.directPath, &r.mime, &r.senderJID,
		&r.key, &r.sha, &r.encSha, &size, &r.fromMe, &r.ts)
	if err != nil {
		return nil, err
	}
	r.media, r.filename, r.url, r.size = media.String, filename.String, url.String, size.Int64
	return &r, nil
}

// oldestMessage is the anchor for asking the phone for earlier history.
func (s *AppStore) oldestMessage(chat string) (id string, fromMe bool, ts time.Time, err error) {
	err = s.db.QueryRow(`SELECT id, is_from_me, timestamp FROM messages WHERE chat_jid = ? ORDER BY timestamp ASC LIMIT 1`, chat).Scan(&id, &fromMe, &ts)
	return
}
