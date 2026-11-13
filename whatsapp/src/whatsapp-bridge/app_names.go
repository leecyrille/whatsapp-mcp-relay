package main

// The user's own names for people, for the ones WhatsApp gives no useful name. They show in WA Relay and the
// connector only. Nothing is sent to WhatsApp or the phone, because whatsmeow can't edit a contact there.
// A name also doesn't make someone a recipient: send checks read the chat and contact tables, not this.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode"

	"go.mau.fi/whatsmeow/types"
)

const maxNameRunes = 80

// Names is the file of overrides, keyed by the address Canon gives.
type Names struct {
	path  string
	mu    sync.RWMutex
	byJID map[string]string
}

// NewNames loads the file. A damaged one is set aside, so it can't stop WhatsApp from working and isn't
// overwritten by the next change.
func NewNames(path string) (*Names, error) {
	n := &Names{path: path, byJID: map[string]string{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &n.byJID); err != nil || n.byJID == nil {
		n.byJID = map[string]string{}
		fmt.Printf("Names file is damaged, setting it aside: %v\n", os.Rename(path, path+".bad"))
	}
	return n, nil
}

// All returns a copy.
func (n *Names) All() map[string]string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make(map[string]string, len(n.byJID))
	for k, v := range n.byJID {
		out[k] = v
	}
	return out
}

// Has reports whether an address has a name.
func (n *Names) Has(jid string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	_, ok := n.byJID[jid]
	return ok
}

// Set stores a name. Nothing changes in memory if the file can't be written.
func (n *Names) Set(jid, name string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	next := make(map[string]string, len(n.byJID)+1)
	for k, v := range n.byJID {
		next[k] = v
	}
	next[jid] = name
	if err := n.write(next); err != nil {
		return err
	}
	n.byJID = next
	return nil
}

// Remove deletes names for any of these addresses, and reports whether there were any.
func (n *Names) Remove(jids ...string) (bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	next := make(map[string]string, len(n.byJID))
	for k, v := range n.byJID {
		next[k] = v
	}
	removed := false
	for _, jid := range jids {
		if _, ok := next[jid]; ok {
			delete(next, jid)
			removed = true
		}
	}
	if !removed {
		return false, nil
	}
	if err := n.write(next); err != nil {
		return false, err
	}
	n.byJID = next
	return true, nil
}

func (n *Names) write(names map[string]string) error {
	raw, err := json.MarshalIndent(names, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(n.path), "app-names-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), n.path)
}

// cleanName tidies a name. Control and invisible characters go, including right-to-left overrides that
// could make a name read as something else. The joiner inside emoji stays.
func cleanName(raw string) (string, error) {
	var b strings.Builder
	for _, r := range raw {
		if unicode.IsSpace(r) {
			b.WriteRune(' ')
		} else if r == '‍' || (!unicode.IsControl(r) && !unicode.Is(unicode.Cf, r)) {
			b.WriteRune(r)
		}
	}
	name := strings.Join(strings.Fields(b.String()), " ")
	if name == "" {
		return "", errors.New("the name is empty")
	}
	if len([]rune(name)) > maxNameRunes {
		return "", fmt.Errorf("names can be up to %d characters", maxNameRunes)
	}
	return name, nil
}

// personJID accepts a person's address only. A group's name belongs to the group.
func personJID(raw string) (types.JID, error) {
	jid, err := parseChatJID(strings.TrimSpace(raw))
	if err != nil {
		return jid, errors.New("bad address")
	}
	if jid.Server == types.GroupServer {
		return jid, errors.New("that's a group, not a person")
	}
	return jid, nil
}

func (a *App) handleNameSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JID  string `json:"jid"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	jid, err := personJID(req.JID)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	name, err := cleanName(req.Name)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	key := a.wa.Canon(jid.String())
	// Someone with a contact card is named by it, so the card changes and the two can't disagree.
	if card := a.cards.ForLink(key); card != nil {
		card.Display = name
		if _, err := a.cards.Save(card, card.Version, whoCalled(r)); err != nil {
			fail(w, 500, "couldn't save the name on their contact card")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "jid": key, "name": name, "card": card.ID})
		return
	}
	if err := a.names.Set(key, name); err != nil {
		fail(w, 500, "couldn't save the name")
		return
	}
	a.contactsChanged()
	writeJSON(w, 200, map[string]any{"ok": true, "jid": key, "name": name})
}

func (a *App) handleNameRemove(w http.ResponseWriter, r *http.Request) {
	jid, err := personJID(r.URL.Query().Get("jid"))
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	if card := a.cards.ForLink(jid.String()); card != nil {
		fail(w, 409, "This person has a contact card, which gives their name. Change the name there, or unlink them from it.")
		return
	}
	targets := []string{jid.String(), a.wa.Canon(jid.String())}
	if aliases, err := a.wa.Aliases(r.Context()); err == nil {
		for _, pair := range aliases {
			if pair[0] == jid.String() || pair[1] == jid.String() {
				targets = append(targets, pair[0], pair[1])
			}
		}
	}
	changed, err := a.names.Remove(targets...)
	if err != nil {
		fail(w, 500, "couldn't save the change")
		return
	}
	if changed {
		a.contactsChanged()
	}
	writeJSON(w, 200, map[string]any{"ok": true, "changed": changed})
}

// contactRow is one person in the app's contact list.
type contactRow struct {
	JID      string `json:"jid"`
	Full     string `json:"full_name,omitempty"`
	First    string `json:"first_name,omitempty"`
	Push     string `json:"push_name,omitempty"`
	Business string `json:"business_name,omitempty"`
}

// withNames puts the user's names over the contact list, as the saved name, which the app already ranks first.
// Both a person's addresses get it, so it holds whichever one a message or a group member arrives under. If
// someone has names under both, the phone number's wins, in a fixed order so the result doesn't vary.
func withNames(rows []contactRow, names map[string]string, aliases [][2]string) []contactRow {
	if len(names) == 0 {
		return rows
	}
	index := make(map[string]int, len(rows))
	for i, row := range rows {
		index[row.JID] = i
	}
	apply := func(jid, name string) {
		if i, ok := index[jid]; ok {
			rows[i].Full = name
			return
		}
		index[jid] = len(rows)
		rows = append(rows, contactRow{JID: jid, Full: name})
	}
	keys := make([]string, 0, len(names))
	for jid := range names {
		keys = append(keys, jid)
	}
	sort.Slice(keys, func(i, j int) bool {
		li, lj := strings.HasSuffix(keys[i], "@lid"), strings.HasSuffix(keys[j], "@lid")
		if li != lj {
			return li
		}
		return keys[i] < keys[j]
	})
	for _, jid := range keys {
		apply(jid, names[jid])
		for _, pair := range aliases {
			if pair[0] == jid {
				apply(pair[1], names[jid])
			} else if pair[1] == jid {
				apply(pair[0], names[jid])
			}
		}
	}
	return rows
}

// knownNamed lets a named person, such as a group member with no chat or contact, count for pictures.
func (a *App) knownNamed(_ context.Context, jid string) bool {
	return a.names != nil && a.names.Has(jid)
}
