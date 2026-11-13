package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	waE2E "go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

// readOutcome says what a read did to a chat's marker.
type readOutcome int

const (
	readMoved   readOutcome = iota // the marker moved forward
	readAlready                    // the chat was already read that far
	readNoChat                     // no stored chat matched, so nothing moved
)

// ownRead says whether a receipt is the user reading a chat on another of their devices. With read receipts on, the
// phone reports that as an ordinary read from his own account; with them off, as read-self.
func ownRead(evt *events.Receipt) bool {
	switch evt.Type {
	case types.ReceiptTypeReadSelf, types.ReceiptTypePlayedSelf:
		return true
	case types.ReceiptTypeRead, types.ReceiptTypePlayed:
		return evt.IsFromMe
	}
	return false
}

// ApplyOwnRead moves the read marker for a chat the user read elsewhere, up to the newest message the receipt
// names, or to when he read it if none of them is stored.
func (s *AppStore) ApplyOwnRead(evt *events.Receipt) (readOutcome, error) {
	return s.markRead(evt.Chat.ToNonAD().String(), evt.Timestamp.UnixMilli(), evt.MessageIDs)
}

// MarkChatRead moves a chat's read marker forward, so every device agrees on what is unread.
func (s *AppStore) MarkChatRead(chat string, ms int64) (readOutcome, error) {
	return s.markRead(chat, ms, nil)
}

func (s *AppStore) markRead(chat string, ms int64, ids []string) (readOutcome, error) {
	out := readNoChat
	err := s.write(true, func(t *txState) error {
		targets, err := t.chatAddresses(chat)
		if err != nil || len(targets) == 0 {
			return err
		}
		if newest := t.newestOf(ids, targets); newest > 0 {
			ms = newest
		}
		out = readAlready
		for _, jid := range targets {
			var current int64
			err := t.tx.QueryRow(`SELECT last_read_ms FROM app_chat WHERE jid = ?`, jid).Scan(&current)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if ms <= current {
				continue
			}
			if err := t.touchChat(jid, ms); err != nil {
				return err
			}
			out = readMoved
		}
		return nil
	})
	if err != nil {
		return readNoChat, err
	}
	return out, nil
}

// chatAddresses finds every stored chat that is this one. WhatsApp may name a person by LID or by phone
// number, and the chat may be stored under the other, or under both.
func (t *txState) chatAddresses(chat string) ([]string, error) {
	var out []string
	seen := map[string]bool{}
	add := func(jid string) error {
		if jid == "" || seen[jid] {
			return nil
		}
		seen[jid] = true
		var one int
		err := t.tx.QueryRow(`SELECT 1 FROM app_chat WHERE jid = ? UNION ALL SELECT 1 FROM chats WHERE jid = ? LIMIT 1`, jid, jid).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		out = append(out, jid)
		return nil
	}
	if err := add(chat); err != nil {
		return nil, err
	}
	if strings.HasSuffix(chat, "@"+types.GroupServer) || t.store.canon == nil {
		return out, nil
	}
	want := t.store.canon(chat)
	if err := add(want); err != nil {
		return nil, err
	}
	rows, err := t.tx.Query(`SELECT jid FROM app_chat WHERE jid LIKE '%@` + types.HiddenUserServer + `'`)
	if err != nil {
		return nil, err
	}
	var lids []string
	for rows.Next() {
		var jid string
		if rows.Scan(&jid) == nil && !seen[jid] {
			lids = append(lids, jid)
		}
	}
	rows.Close()
	for _, lid := range lids {
		if t.store.canon(lid) == want {
			if err := add(lid); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// newestOf gives the time of the newest of these messages stored in these chats, in milliseconds, or 0.
func (t *txState) newestOf(ids, chats []string) int64 {
	var newest int64
	for _, id := range ids {
		for _, chat := range chats {
			var ts sql.NullTime
			if t.tx.QueryRow(`SELECT timestamp FROM messages WHERE id = ? AND chat_jid = ?`, id, chat).Scan(&ts) == nil && ts.Valid {
				if ms := ts.Time.UnixMilli(); ms > newest {
					newest = ms
				}
			}
		}
	}
	return newest
}

// isReply says whether a message is something the user wrote, as opposed to a reaction, edit or deletion. Writing
// in a chat means he has it open, so WhatsApp counts the chat as read up to there.
func isReply(id string, msg *waE2E.Message) bool {
	if msg == nil || msg.GetProtocolMessage() != nil || msg.GetReactionMessage() != nil {
		return false
	}
	return !parseMessage(id, msg).empty()
}

// readCounts counts how read markers move, so a path that stops working shows in the log. Counts only: no
// chat, person or message is ever written out.
type readCounts struct {
	mu    sync.Mutex
	since time.Time
	n     map[string]int
}

func (r *readCounts) add(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == nil {
		r.n = map[string]int{}
		r.since = time.Now()
	}
	r.n[key]++
}

func (r *readCounts) outcome(path string, out readOutcome, err error) {
	switch {
	case err != nil:
		r.add(path + " failed")
	case out == readMoved:
		r.add(path + " moved")
	case out == readAlready:
		r.add(path + " already")
	default:
		r.add(path + " no chat")
	}
}

// line takes the counts so far and starts again. Empty when nothing happened.
func (r *readCounts) line() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.n) == 0 {
		return ""
	}
	keys := []string{
		"phone read moved", "phone read already", "phone read no chat", "phone read failed",
		"marked read moved", "marked read already", "marked read no chat", "marked read failed", "marked unread",
		"own message moved", "own message already", "own message no chat", "own message failed",
		"app read moved", "app read already", "app read no chat", "app read failed", "receipts sent", "receipts failed",
	}
	var parts []string
	for _, k := range keys {
		if v := r.n[k]; v > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", k, v))
		}
	}
	mins := int(time.Since(r.since).Minutes())
	r.n = nil
	return fmt.Sprintf("Read markers, last %d min: %s", mins, strings.Join(parts, ", "))
}

// logReads writes the read counts every interval, and says so when there were none while messages arrived,
// which is how the last failure went unnoticed.
func (a *App) logReads(every time.Duration) {
	for {
		time.Sleep(every)
		if line := a.reads.line(); line != "" {
			fmt.Println(line)
		} else if n := a.incoming.Swap(0); n > 0 {
			fmt.Printf("Read markers, last %d min: none moved, while %d messages arrived\n", int(every.Minutes()), n)
			continue
		}
		a.incoming.Store(0)
	}
}

// CatchUpOwnReplies moves each chat's read marker up to the user's newest message in it. Until 2026-11-09 neither their
// reads on the phone nor his replies moved markers, so chats he had answered still showed unread. It runs once,
// marked by the record file, because Claude's sends through the connector are stored as the user's too, and later
// ones mustn't count as reading. The record keeps each chat's marker before and after, for undoing by hand.
func (s *AppStore) CatchUpOwnReplies(record string) (int, error) {
	if _, err := os.Stat(record); err == nil {
		return 0, nil
	}
	type change struct {
		Chat   string `json:"chat"`
		Before int64  `json:"before_ms"`
		After  int64  `json:"after_ms"`
	}
	changes := []change{}
	err := s.write(true, func(t *txState) error {
		rows, err := t.tx.Query(`SELECT jid, last_read_ms FROM app_chat`)
		if err != nil {
			return err
		}
		var all []change
		for rows.Next() {
			var c change
			if err := rows.Scan(&c.Chat, &c.Before); err != nil {
				rows.Close()
				return err
			}
			all = append(all, c)
		}
		rows.Close()
		for _, c := range all {
			// Compared as times, not as stored text: the text carries the UTC offset, which changes with DST.
			own, err := t.tx.Query(`SELECT timestamp FROM messages WHERE chat_jid = ? AND is_from_me = 1`, c.Chat)
			if err != nil {
				return err
			}
			for own.Next() {
				var ts sql.NullTime
				if own.Scan(&ts) == nil && ts.Valid && ts.Time.UnixMilli() > c.After {
					c.After = ts.Time.UnixMilli()
				}
			}
			own.Close()
			if c.After <= c.Before {
				continue
			}
			if err := t.touchChat(c.Chat, c.After); err != nil {
				return err
			}
			changes = append(changes, c)
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	data, _ := json.MarshalIndent(changes, "", " ")
	if err := os.WriteFile(record, data, 0o600); err != nil {
		return len(changes), err
	}
	return len(changes), nil
}
