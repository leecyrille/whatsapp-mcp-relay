package main

// Groups with no name. WhatsApp shows these by their members' names, such as "A & B", and so does
// everything here. That label is kept apart from the group's name in chats.name, which stays empty, so a
// name given on any device replaces it and nothing ever sends the label to WhatsApp as a name.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow/types"
)

const (
	groupLabelKeep  = 10 * time.Minute
	groupLabelNames = 6 // past this, "& N others"
	unnamedGroup    = "Unnamed group"
)

const groupLabelSchema = `
CREATE TABLE IF NOT EXISTS app_group_member (
	chat TEXT NOT NULL,
	member TEXT NOT NULL,
	PRIMARY KEY (chat, member)
) WITHOUT ROWID;
`

// addLabelColumn gives app_chat the label column in a store made before it existed.
func addLabelColumn(db *sql.DB) error {
	if _, err := db.Exec(groupLabelSchema); err != nil {
		return err
	}
	rows, err := db.Query(`PRAGMA table_info(app_chat)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, kind string
		var def sql.NullString
		if err := rows.Scan(&cid, &name, &kind, &notNull, &def, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == "label" {
			found = true
		}
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = db.Exec(`ALTER TABLE app_chat ADD COLUMN label TEXT NOT NULL DEFAULT ''`)
	return err
}

// groupLabel joins names the way WhatsApp does: "A", "A & B", "A, B & C".
func groupLabel(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sort.SliceStable(names, func(i, j int) bool {
		ni, nj := strings.HasPrefix(names[i], "+"), strings.HasPrefix(names[j], "+")
		if ni != nj {
			return nj // names before bare numbers
		}
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	shown, others := names, 0
	if len(names) > groupLabelNames {
		shown, others = names[:groupLabelNames], len(names)-groupLabelNames
	}
	switch {
	case others == 1:
		return strings.Join(shown, ", ") + " & 1 other"
	case others > 1:
		return fmt.Sprintf("%s & %d others", strings.Join(shown, ", "), others)
	case len(shown) == 1:
		return shown[0]
	default:
		return strings.Join(shown[:len(shown)-1], ", ") + " & " + shown[len(shown)-1]
	}
}

// --- store ---

// ClearPlaceholderNames empties the "Group <id>" names older bridges stored for groups with no name.
func (s *AppStore) ClearPlaceholderNames() (int, error) {
	n := 0
	err := s.write(true, func(t *txState) error {
		rows, err := t.tx.Query(`SELECT jid FROM chats WHERE jid LIKE '%@g.us'
			AND name = 'Group ' || substr(jid, 1, instr(jid, '@') - 1)`)
		if err != nil {
			return err
		}
		var jids []string
		for rows.Next() {
			var jid string
			if err := rows.Scan(&jid); err != nil {
				rows.Close()
				return err
			}
			jids = append(jids, jid)
		}
		rows.Close()
		for _, jid := range jids {
			if _, err := t.tx.Exec(`UPDATE chats SET name = '' WHERE jid = ?`, jid); err != nil {
				return err
			}
			if err := t.touchChat(jid, 0); err != nil {
				return err
			}
		}
		n = len(jids)
		return nil
	})
	return n, err
}

// UnnamedGroups lists the groups with no name.
func (s *AppStore) UnnamedGroups() ([]string, error) {
	rows, err := s.db.Query(`SELECT jid FROM chats WHERE jid LIKE '%@g.us' AND COALESCE(name, '') = '' ORDER BY jid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var jid string
		if err := rows.Scan(&jid); err != nil {
			return nil, err
		}
		out = append(out, jid)
	}
	return out, rows.Err()
}

// IsUnnamedGroup is true for a group that's stored with no name.
func (s *AppStore) IsUnnamedGroup(chat string) bool {
	var name string
	err := s.db.QueryRow(`SELECT COALESCE(name, '') FROM chats WHERE jid = ? AND jid LIKE '%@g.us'`, chat).Scan(&name)
	return err == nil && name == ""
}

// ClearGroupName records that a group's name was taken away.
func (s *AppStore) ClearGroupName(chat string) error {
	return s.write(true, func(t *txState) error {
		res, err := t.tx.Exec(`UPDATE chats SET name = '' WHERE jid = ? AND COALESCE(name, '') != ''`, chat)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
		return t.touchChat(chat, 0)
	})
}

// GroupMembers is who was in the group when last asked.
func (s *AppStore) GroupMembers(chat string) ([]string, error) {
	rows, err := s.db.Query(`SELECT member FROM app_group_member WHERE chat = ? ORDER BY member`, chat)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SetGroupMembers replaces who's in a group.
func (s *AppStore) SetGroupMembers(chat string, members []string) error {
	return s.write(false, func(t *txState) error {
		if _, err := t.tx.Exec(`DELETE FROM app_group_member WHERE chat = ?`, chat); err != nil {
			return err
		}
		for _, m := range members {
			if _, err := t.tx.Exec(`INSERT OR IGNORE INTO app_group_member (chat, member) VALUES (?, ?)`, chat, m); err != nil {
				return err
			}
		}
		return nil
	})
}

// SetGroupLabel stores the members' names for a group with no name, and sends the chat again if they
// changed. Only for a chat that's already stored.
func (s *AppStore) SetGroupLabel(chat, label string) (bool, error) {
	changed := false
	err := s.write(true, func(t *txState) error {
		var current string
		err := t.tx.QueryRow(`SELECT a.label FROM app_chat a JOIN chats c ON c.jid = a.jid WHERE a.jid = ?`, chat).Scan(&current)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil || current == label {
			return err
		}
		if _, err := t.tx.Exec(`UPDATE app_chat SET label = ? WHERE jid = ?`, label, chat); err != nil {
			return err
		}
		changed = true
		return t.touchChat(chat, 0)
	})
	return changed, err
}

// --- app ---

type groupLabels struct {
	mu       sync.Mutex
	learning map[string]bool
	sweeping bool
}

// memberAddress is how a member is kept: by phone number where known, so names match either way.
func (a *App) memberAddress(m types.GroupParticipant) string {
	if !m.PhoneNumber.IsEmpty() {
		return m.PhoneNumber.ToNonAD().String()
	}
	if m.JID.IsEmpty() {
		return ""
	}
	return a.wa.Canon(m.JID.ToNonAD().String())
}

// applyGroupInfo records what WhatsApp says about a group: its name if it has one, otherwise its members
// and the label made from them.
func (a *App) applyGroupInfo(ctx context.Context, info *types.GroupInfo) error {
	chat := info.JID.ToNonAD().String()
	if name := strings.TrimSpace(info.Name); name != "" {
		return a.store.RenameChat(chat, name)
	}
	if !a.store.IsUnnamedGroup(chat) {
		return nil // a group stored with a name keeps it until WhatsApp says it changed
	}
	members := make([]string, 0, len(info.Participants))
	for _, m := range info.Participants {
		if addr := a.memberAddress(m); addr != "" {
			members = append(members, addr)
		}
	}
	if err := a.store.SetGroupMembers(chat, members); err != nil {
		return err
	}
	_, err := a.relabel(ctx, chat, nil)
	return err
}

// learnGroup asks WhatsApp about a group with no name, once at a time per group. WhatsApp is asked at most
// once per groupLabelKeep, and a failure is only kept for a minute.
func (a *App) learnGroup(ctx context.Context, jid types.JID) {
	chat := jid.ToNonAD().String()
	l := &a.labels
	l.mu.Lock()
	if l.learning == nil {
		l.learning = map[string]bool{}
	}
	if l.learning[chat] {
		l.mu.Unlock()
		return
	}
	l.learning[chat] = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		delete(l.learning, chat)
		l.mu.Unlock()
	}()
	info, err := a.groupInfoKept(ctx, jid, groupLabelKeep)
	if err != nil || info == nil {
		return
	}
	if info.JID.IsEmpty() {
		copied := *info
		copied.JID = jid
		info = &copied
	}
	if err := a.applyGroupInfo(ctx, info); err != nil {
		fmt.Printf("Group with no name: couldn't store its members: %v\n", err)
	}
}

// noticeUnnamed asks about a group with no name the first time a message arrives in it.
func (a *App) noticeUnnamed(chat types.JID) {
	chat = chat.ToNonAD()
	if chat.Server != types.GroupServer || !a.store.IsUnnamedGroup(chat.String()) {
		return
	}
	if members, _ := a.store.GroupMembers(chat.String()); len(members) == 0 {
		go a.learnGroup(context.Background(), chat)
	}
}

// memberNames names each member other than the user: their own name for them, then a saved name, then the
// name they gave themselves, then the number. A hidden ID with no number and no name is left out.
func (a *App) memberNames(ctx context.Context, members []string, contacts map[types.JID]types.ContactInfo) []string {
	pn, lid := a.wa.Own()
	custom := map[string]string{}
	if a.names != nil {
		custom = a.names.All()
	}
	var out []string
	for _, m := range members {
		canon := a.wa.Canon(m)
		if m == pn || m == lid || canon == pn {
			continue
		}
		if name := custom[m]; name != "" {
			out = append(out, name)
			continue
		}
		if name := custom[canon]; name != "" {
			out = append(out, name)
			continue
		}
		if name := contactName(contacts, m, canon); name != "" {
			out = append(out, name)
			continue
		}
		if j, err := types.ParseJID(canon); err == nil && j.Server == types.DefaultUserServer {
			out = append(out, "+"+j.User)
		}
	}
	return out
}

func contactName(contacts map[types.JID]types.ContactInfo, addresses ...string) string {
	for _, pick := range []func(types.ContactInfo) string{
		func(c types.ContactInfo) string { return c.FullName },
		func(c types.ContactInfo) string { return c.FirstName },
		func(c types.ContactInfo) string { return c.BusinessName },
		func(c types.ContactInfo) string { return c.PushName },
	} {
		for _, addr := range addresses {
			j, err := types.ParseJID(addr)
			if err != nil {
				continue
			}
			if c, ok := contacts[j]; ok {
				if name := strings.TrimSpace(pick(c)); name != "" {
					return name
				}
			}
		}
	}
	return ""
}

// relabel works out a group's label from its stored members. contacts may be nil, to read them here.
func (a *App) relabel(ctx context.Context, chat string, contacts map[types.JID]types.ContactInfo) (bool, error) {
	members, err := a.store.GroupMembers(chat)
	if err != nil || len(members) == 0 {
		return false, err
	}
	if contacts == nil {
		contacts, _ = a.wa.Contacts(ctx)
	}
	return a.store.SetGroupLabel(chat, groupLabel(a.memberNames(ctx, members, contacts)))
}

// sweepUnnamedGroups clears old placeholder names, relabels the groups with no name whose members are
// known, and asks WhatsApp about the rest, a few seconds apart. One sweep runs at a time.
func (a *App) sweepUnnamedGroups(ctx context.Context, ask bool) {
	l := &a.labels
	l.mu.Lock()
	if l.sweeping {
		l.mu.Unlock()
		return
	}
	l.sweeping = true
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.sweeping = false
		l.mu.Unlock()
	}()
	if n, err := a.store.ClearPlaceholderNames(); err != nil {
		fmt.Printf("Groups with no name: clearing placeholder names failed: %v\n", err)
	} else if n > 0 {
		fmt.Printf("Groups with no name: cleared %d placeholder names\n", n)
	}
	groups, err := a.store.UnnamedGroups()
	if err != nil {
		return
	}
	contacts, _ := a.wa.Contacts(ctx)
	var unknown []string
	for _, chat := range groups {
		members, _ := a.store.GroupMembers(chat)
		if len(members) == 0 {
			unknown = append(unknown, chat)
			continue
		}
		_, _ = a.relabel(ctx, chat, contacts)
	}
	if !ask {
		return
	}
	for i, chat := range unknown {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(3 * time.Second):
			}
		}
		if jid, err := types.ParseJID(chat); err == nil {
			a.learnGroup(ctx, jid)
		}
	}
}
