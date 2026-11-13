package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Group is a user-defined category such as Personal or Work.
type Group struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Order int    `json:"order"`
}

// GroupDoc is what clients see.
type GroupDoc struct {
	Type   string            `json:"type"`
	Groups []Group           `json:"groups"`
	Assign map[string]string `json:"assign"`
	// ProjectRule is which chats the name rule (rule.go) has already decided. Stored here, never sent to apps.
	ProjectRule map[string]string `json:"project_rule,omitempty"`
}

// Groups stores the categories in one JSON file. There are at most a few thousand assignments, so
// rewriting the file on a change is cheap and leaves nothing to migrate. It lives on the home PC so
// every device sees the same sorting.
type Groups struct {
	path string
	hub  *Hub
	mu   sync.Mutex
	doc  GroupDoc
}

const (
	maxGroups     = 30
	maxAssignment = 5000
)

var (
	jidRe   = regexp.MustCompile(`^[0-9A-Za-z.:_-]{1,64}@(s\.whatsapp\.net|lid|g\.us)$`)
	colorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)
)

// LoadGroups reads the file, starting with Personal and Work when there isn't one yet.
func LoadGroups(path string, hub *Hub) (*Groups, error) {
	g := &Groups{path: path, hub: hub}
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(raw, &g.doc); err != nil {
			return nil, fmt.Errorf("%s is damaged: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		g.doc = GroupDoc{Groups: []Group{
			{ID: "personal", Name: "Personal", Color: "#2f9e6e", Order: 0},
			{ID: "work", Name: "Work", Color: "#3b7ddd", Order: 1},
		}}
	default:
		return nil, err
	}
	if g.doc.Assign == nil {
		g.doc.Assign = map[string]string{}
	}
	g.doc.Type = "groups"
	return g, nil
}

// Snapshot returns a copy of the current state.
func (g *Groups) Snapshot() GroupDoc {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.copyLocked()
}

func (g *Groups) copyLocked() GroupDoc {
	doc := GroupDoc{Type: "groups", Groups: append([]Group{}, g.doc.Groups...), Assign: make(map[string]string, len(g.doc.Assign))}
	for k, v := range g.doc.Assign {
		doc.Assign[k] = v
	}
	sort.SliceStable(doc.Groups, func(i, j int) bool { return doc.Groups[i].Order < doc.Groups[j].Order })
	return doc
}

// saveLocked writes through a temp file, so a crash can't leave half a file. Clients hear about the
// change only after it is on disk.
func (g *Groups) saveLocked() error {
	raw, err := json.MarshalIndent(g.doc, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(g.path), 0o700); err != nil {
		return err
	}
	tmp := g.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, g.path); err != nil {
		return err
	}
	if g.hub != nil {
		g.hub.Publish(g.copyLocked())
	}
	return nil
}

func cleanName(name string) (string, error) {
	name = strings.Join(strings.Fields(name), " ")
	if name == "" || len([]rune(name)) > 40 {
		return "", errors.New("a group name needs 1 to 40 characters")
	}
	// The app works these out from each chat's address, so a stored one would only be confusing.
	for _, builtIn := range []string{"All", "People", "Groups", "Unread", "Unsorted"} {
		if strings.EqualFold(name, builtIn) {
			return "", fmt.Errorf("%s is built in, so choose another name", builtIn)
		}
	}
	return name, nil
}

func (g *Groups) findLocked(id string) *Group {
	for i := range g.doc.Groups {
		if g.doc.Groups[i].ID == id {
			return &g.doc.Groups[i]
		}
	}
	return nil
}

// Create adds a group.
func (g *Groups) Create(name, color string) error {
	name, err := cleanName(name)
	if err != nil {
		return err
	}
	if color != "" && !colorRe.MatchString(color) {
		return errors.New("bad color")
	}
	if color == "" {
		color = "#8a6fd1"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.doc.Groups) >= maxGroups {
		return errors.New("too many groups")
	}
	for _, existing := range g.doc.Groups {
		if strings.EqualFold(existing.Name, name) {
			return errors.New("that group already exists")
		}
	}
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	order := 0
	for _, existing := range g.doc.Groups {
		if existing.Order >= order {
			order = existing.Order + 1
		}
	}
	g.doc.Groups = append(g.doc.Groups, Group{ID: hex.EncodeToString(buf), Name: name, Color: color, Order: order})
	return g.saveLocked()
}

// Update changes a group's name, color or position.
func (g *Groups) Update(id string, name, color *string, order *int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	group := g.findLocked(id)
	if group == nil {
		return errors.New("no such group")
	}
	if name != nil {
		clean, err := cleanName(*name)
		if err != nil {
			return err
		}
		group.Name = clean
	}
	if color != nil {
		if !colorRe.MatchString(*color) {
			return errors.New("bad color")
		}
		group.Color = *color
	}
	if order != nil {
		group.Order = *order
	}
	return g.saveLocked()
}

// Delete removes a group. Its chats go back to unsorted; no chat is deleted.
func (g *Groups) Delete(id string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.findLocked(id) == nil {
		return errors.New("no such group")
	}
	kept := g.doc.Groups[:0]
	for _, group := range g.doc.Groups {
		if group.ID != id {
			kept = append(kept, group)
		}
	}
	g.doc.Groups = kept
	for jid, assigned := range g.doc.Assign {
		if assigned == id {
			delete(g.doc.Assign, jid)
		}
	}
	return g.saveLocked()
}

// Assign puts chats or contacts in a group. An empty group means unsorted. All or nothing: one bad
// address rejects the whole request, and nothing is saved.
func (g *Groups) Assign(jids []string, group string) error {
	if len(jids) == 0 || len(jids) > maxAssignment {
		return fmt.Errorf("between 1 and %d addresses are needed", maxAssignment)
	}
	for _, jid := range jids {
		if !jidRe.MatchString(jid) {
			return errors.New("bad address")
		}
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if group != "" && g.findLocked(group) == nil {
		return errors.New("no such group")
	}
	for _, jid := range jids {
		if group == "" {
			delete(g.doc.Assign, jid)
		} else {
			g.doc.Assign[jid] = group
		}
	}
	return g.saveLocked()
}
