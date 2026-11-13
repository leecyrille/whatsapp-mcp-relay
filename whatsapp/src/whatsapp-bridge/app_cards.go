package main

// Contact cards: the user's own details for people, with the fields a phone keeps, kept on the home PC only. Nothing
// here is sent to WhatsApp or written to whatsmeow's store, and cards don't sync to the phone. A card links to one
// or more WhatsApp addresses, and its name becomes the user's name for them through the same names file the app and the
// connector already read. Every change is appended to a log with the card before and after, so any change can be
// undone, a delete included.

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"
)

const (
	maxCards        = 20000
	maxCardItems    = 20
	maxFieldRunes   = 300
	maxNoteRunes    = 5000
	maxImportCards  = 5000
	cardsFile       = "app-cards.json"
	cardsLogFile    = "app-cards-log.jsonl"
	whoHeader       = "X-Verified-Email"
	defaultCountry  = "1" // Numbers without a country code are taken as North American. Change this for elsewhere.
	cardsMaxRequest = 16 << 20
)

type Labeled struct {
	Label string `json:"label,omitempty"`
	Value string `json:"value"`
}

type CardAddress struct {
	Label    string `json:"label,omitempty"`
	Street   string `json:"street,omitempty"`
	City     string `json:"city,omitempty"`
	Region   string `json:"region,omitempty"`
	Postcode string `json:"postcode,omitempty"`
	Country  string `json:"country,omitempty"`
}

type CardName struct {
	Prefix string `json:"prefix,omitempty"`
	Given  string `json:"given,omitempty"`
	Middle string `json:"middle,omitempty"`
	Family string `json:"family,omitempty"`
	Suffix string `json:"suffix,omitempty"`
}

// Card holds what a phone's contact keeps, following vCard's fields.
type Card struct {
	ID         string        `json:"id"`
	Version    int           `json:"version"`
	CreatedMs  int64         `json:"created_ms"`
	UpdatedMs  int64         `json:"updated_ms"`
	Display    string        `json:"display"`
	Name       CardName      `json:"name"`
	Nickname   string        `json:"nickname,omitempty"`
	Phones     []Labeled     `json:"phones,omitempty"`
	Emails     []Labeled     `json:"emails,omitempty"`
	Company    string        `json:"company,omitempty"`
	Department string        `json:"department,omitempty"`
	Title      string        `json:"title,omitempty"`
	Addresses  []CardAddress `json:"addresses,omitempty"`
	URLs       []Labeled     `json:"urls,omitempty"`
	Dates      []Labeled     `json:"dates,omitempty"`
	Related    []Labeled     `json:"related,omitempty"`
	Handles    []Labeled     `json:"handles,omitempty"`
	Notes      string        `json:"notes,omitempty"`
	// Links are the WhatsApp addresses this card is for, in the form Canon gives.
	Links []string `json:"links,omitempty"`
}

type cardsDoc struct {
	Rev   int     `json:"rev"`
	Cards []*Card `json:"cards"`
}

type cardLogEntry struct {
	Seq    int    `json:"seq"`
	AtMs   int64  `json:"at_ms"`
	Who    string `json:"who"`
	Action string `json:"action"`
	ID     string `json:"id"`
	Before *Card  `json:"before,omitempty"`
	After  *Card  `json:"after,omitempty"`
}

var (
	errCardChanged  = errors.New("This card was changed somewhere else since you opened it. Close it and open it again to see the latest.")
	errCardNotFound = errors.New("That card doesn't exist any more.")
	dateShape       = regexp.MustCompile(`^(\d{4}|--)-?\d{2}-?\d{2}$|^\d{4}$`)
)

// Cards is the card file, its log, and the names it gives people.
type Cards struct {
	dir     string
	names   *Names
	canon   func(string) string
	changed func()
	mu      sync.Mutex
	doc     cardsDoc
	logSeq  int
}

func NewCards(dir string, names *Names, canon func(string) string, changed func()) (*Cards, error) {
	c := &Cards{dir: dir, names: names, canon: canon, changed: changed}
	raw, err := os.ReadFile(filepath.Join(dir, cardsFile))
	if err == nil {
		if err := json.Unmarshal(raw, &c.doc); err != nil {
			// Set aside rather than started over, so the next save can't overwrite what was there.
			fmt.Printf("Cards file is damaged, setting it aside: %v\n", os.Rename(filepath.Join(dir, cardsFile), filepath.Join(dir, cardsFile+".bad")))
			c.doc = cardsDoc{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if f, err := os.Open(filepath.Join(dir, cardsLogFile)); err == nil {
		scan := bufio.NewScanner(f)
		scan.Buffer(make([]byte, 1<<20), 16<<20)
		for scan.Scan() {
			var e cardLogEntry
			if json.Unmarshal(scan.Bytes(), &e) == nil && e.Seq > c.logSeq {
				c.logSeq = e.Seq
			}
		}
		f.Close()
	}
	return c, nil
}

func newCardID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func cloneCard(c *Card) *Card {
	if c == nil {
		return nil
	}
	raw, _ := json.Marshal(c)
	var out Card
	_ = json.Unmarshal(raw, &out)
	return &out
}

// All returns copies, sorted by name.
func (c *Cards) All() ([]*Card, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Card, 0, len(c.doc.Cards))
	for _, card := range c.doc.Cards {
		out = append(out, cloneCard(card))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Display) < strings.ToLower(out[j].Display) })
	return out, c.doc.Rev
}

func (c *Cards) find(id string) (int, *Card) {
	for i, card := range c.doc.Cards {
		if card.ID == id {
			return i, card
		}
	}
	return -1, nil
}

// ForLink is the card linked to an address, if any.
func (c *Cards) ForLink(jid string) *Card {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneCard(c.forLink(c.canon(jid)))
}

func (c *Cards) forLink(key string) *Card {
	for _, card := range c.doc.Cards {
		for _, l := range card.Links {
			if l == key {
				return card
			}
		}
	}
	return nil
}

// --- Cleaning ---

func cleanText(raw string, limit int) string {
	var b strings.Builder
	for _, r := range raw {
		if r == '\n' && limit == maxNoteRunes {
			b.WriteRune(r)
		} else if unicode.IsSpace(r) {
			b.WriteRune(' ')
		} else if r == '‍' || (!unicode.IsControl(r) && !unicode.Is(unicode.Cf, r)) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if limit != maxNoteRunes {
		out = strings.Join(strings.Fields(out), " ")
	} else {
		out = strings.TrimSpace(out)
	}
	if rs := []rune(out); len(rs) > limit {
		out = string(rs[:limit])
	}
	return out
}

func cleanList(items []Labeled, check func(string) (string, error)) ([]Labeled, error) {
	var out []Labeled
	seen := map[string]bool{}
	for _, it := range items {
		value := cleanText(it.Value, maxFieldRunes)
		if value == "" {
			continue
		}
		if check != nil {
			var err error
			if value, err = check(value); err != nil {
				return nil, err
			}
		}
		if seen[strings.ToLower(value)] {
			continue
		}
		seen[strings.ToLower(value)] = true
		out = append(out, Labeled{Label: cleanText(it.Label, 40), Value: value})
	}
	if len(out) > maxCardItems {
		return nil, fmt.Errorf("a card can hold up to %d of each kind of detail", maxCardItems)
	}
	return out, nil
}

func checkEmail(v string) (string, error) {
	addr, err := mail.ParseAddress(v)
	if err != nil || addr.Address != v || !strings.Contains(v, ".") {
		return "", fmt.Errorf("%q doesn't look like an email address", v)
	}
	at := strings.LastIndex(v, "@")
	return v[:at] + strings.ToLower(v[at:]), nil
}

func checkPhone(v string) (string, error) {
	if _, ok := phoneDigits(v); !ok {
		return "", fmt.Errorf("%q doesn't look like a phone number", v)
	}
	return v, nil
}

func checkDate(v string) (string, error) {
	if !dateShape.MatchString(v) {
		return "", fmt.Errorf("%q isn't a date. Use 2024-05-31, or --05-31 without a year", v)
	}
	return v, nil
}

// phoneDigits is a number in full international form, digits only. One with no country code is taken as North
// American (defaultCountry).
func phoneDigits(raw string) (string, bool) {
	trimmed := strings.TrimSpace(raw)
	var b strings.Builder
	for _, r := range trimmed {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if !strings.ContainsRune(" +-().", r) {
			return "", false
		}
	}
	d := b.String()
	switch {
	case strings.HasPrefix(trimmed, "+"):
	case strings.HasPrefix(d, "00"):
		d = d[2:]
	case len(d) == 10:
		d = defaultCountry + d
	}
	if len(d) < 8 || len(d) > 15 {
		return "", false
	}
	return d, true
}

func (c *Cards) cleanCard(in *Card) (*Card, error) {
	out := &Card{ID: in.ID, Version: in.Version, CreatedMs: in.CreatedMs, UpdatedMs: in.UpdatedMs}
	out.Name = CardName{
		Prefix: cleanText(in.Name.Prefix, 40), Given: cleanText(in.Name.Given, maxFieldRunes), Middle: cleanText(in.Name.Middle, maxFieldRunes),
		Family: cleanText(in.Name.Family, maxFieldRunes), Suffix: cleanText(in.Name.Suffix, 40),
	}
	out.Nickname = cleanText(in.Nickname, maxFieldRunes)
	out.Company, out.Department, out.Title = cleanText(in.Company, maxFieldRunes), cleanText(in.Department, maxFieldRunes), cleanText(in.Title, maxFieldRunes)
	out.Notes = cleanText(in.Notes, maxNoteRunes)
	var err error
	if out.Phones, err = cleanList(in.Phones, checkPhone); err != nil {
		return nil, err
	}
	if out.Emails, err = cleanList(in.Emails, checkEmail); err != nil {
		return nil, err
	}
	if out.URLs, err = cleanList(in.URLs, nil); err != nil {
		return nil, err
	}
	if out.Dates, err = cleanList(in.Dates, checkDate); err != nil {
		return nil, err
	}
	if out.Related, err = cleanList(in.Related, nil); err != nil {
		return nil, err
	}
	if out.Handles, err = cleanList(in.Handles, nil); err != nil {
		return nil, err
	}
	for _, a := range in.Addresses {
		clean := CardAddress{Label: cleanText(a.Label, 40), Street: cleanText(a.Street, maxFieldRunes), City: cleanText(a.City, maxFieldRunes),
			Region: cleanText(a.Region, maxFieldRunes), Postcode: cleanText(a.Postcode, 40), Country: cleanText(a.Country, maxFieldRunes)}
		if clean.Street+clean.City+clean.Region+clean.Postcode+clean.Country != "" {
			out.Addresses = append(out.Addresses, clean)
		}
	}
	if len(out.Addresses) > maxCardItems {
		return nil, fmt.Errorf("a card can hold up to %d addresses", maxCardItems)
	}
	seen := map[string]bool{}
	for _, l := range in.Links {
		jid, err := personJID(l)
		if err != nil || (jid.Server != types.DefaultUserServer && jid.Server != types.HiddenUserServer) {
			return nil, fmt.Errorf("%q isn't a WhatsApp person's address", l)
		}
		key := c.canon(jid.ToNonAD().String())
		if !seen[key] {
			seen[key] = true
			out.Links = append(out.Links, key)
		}
	}
	display, _ := cleanName(in.Display)
	if display == "" {
		display = strings.TrimSpace(strings.Join(strings.Fields(strings.Join([]string{out.Name.Prefix, out.Name.Given, out.Name.Middle, out.Name.Family, out.Name.Suffix}, " ")), " "))
	}
	for _, fallback := range []string{out.Nickname, out.Company} {
		if display == "" {
			display = fallback
		}
	}
	if display == "" && len(out.Emails) > 0 {
		display = out.Emails[0].Value
	}
	if display == "" && len(out.Phones) > 0 {
		display = out.Phones[0].Value
	}
	if display == "" {
		return nil, errors.New("a card needs a name, or at least a company, email or phone number")
	}
	if rs := []rune(display); len(rs) > maxNameRunes {
		display = string(rs[:maxNameRunes])
	}
	out.Display = display
	return out, nil
}

// --- Saving ---

func (c *Cards) persist(doc cardsDoc) error {
	raw, err := json.MarshalIndent(doc, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, "app-cards-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(c.dir, cardsFile))
}

func (c *Cards) appendLog(entries []cardLogEntry) error {
	f, err := os.OpenFile(filepath.Join(c.dir, cardsLogFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, e := range entries {
		raw, _ := json.Marshal(e)
		if _, err := f.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	return f.Sync()
}

type cardChange struct {
	before, after *Card
	action        string
}

// commit writes the changes, logs them, and moves names to match. The log is written first: a change that isn't
// logged can't be undone, so it doesn't happen.
func (c *Cards) commit(who string, changes []cardChange) error {
	next := cardsDoc{Rev: c.doc.Rev + 1, Cards: make([]*Card, 0, len(c.doc.Cards)+len(changes))}
	gone := map[string]bool{}
	replaced := map[string]*Card{}
	for _, ch := range changes {
		if ch.after == nil {
			gone[ch.before.ID] = true
		} else {
			replaced[ch.after.ID] = ch.after
		}
	}
	for _, card := range c.doc.Cards {
		if gone[card.ID] {
			continue
		}
		if r, ok := replaced[card.ID]; ok {
			next.Cards = append(next.Cards, r)
			delete(replaced, card.ID)
		} else {
			next.Cards = append(next.Cards, card)
		}
	}
	for _, ch := range changes {
		if ch.after != nil {
			if _, still := replaced[ch.after.ID]; still {
				next.Cards = append(next.Cards, ch.after)
			}
		}
	}
	if len(next.Cards) > maxCards {
		return fmt.Errorf("that's more than %d cards", maxCards)
	}
	now := time.Now().UnixMilli()
	entries := make([]cardLogEntry, 0, len(changes))
	seq := c.logSeq
	for _, ch := range changes {
		seq++
		id := ""
		if ch.after != nil {
			id = ch.after.ID
		} else {
			id = ch.before.ID
		}
		entries = append(entries, cardLogEntry{Seq: seq, AtMs: now, Who: who, Action: ch.action, ID: id, Before: ch.before, After: ch.after})
	}
	if err := c.appendLog(entries); err != nil {
		return fmt.Errorf("couldn't log the change, so nothing was saved: %w", err)
	}
	c.logSeq = seq
	if err := c.persist(next); err != nil {
		return fmt.Errorf("couldn't save: %w", err)
	}
	c.doc = next
	c.projectNames(changes)
	if c.changed != nil {
		c.changed()
	}
	return nil
}

// projectNames makes a linked card's name the user's name for its addresses, and takes it back off an address that's
// no longer linked, as long as it's still the card's own name there.
func (c *Cards) projectNames(changes []cardChange) {
	if c.names == nil {
		return
	}
	for _, ch := range changes {
		if ch.before != nil {
			keep := map[string]bool{}
			if ch.after != nil {
				for _, l := range ch.after.Links {
					keep[l] = true
				}
			}
			for _, l := range ch.before.Links {
				if !keep[l] {
					if current, ok := c.names.All()[l]; ok && current == ch.before.Display {
						_, _ = c.names.Remove(l)
					}
				}
			}
		}
		if ch.after != nil {
			for _, l := range ch.after.Links {
				if current := c.names.All()[l]; current != ch.after.Display {
					_ = c.names.Set(l, ch.after.Display)
				}
			}
		}
	}
}

func (c *Cards) checkLinks(card *Card) error {
	for _, l := range card.Links {
		if other := c.forLink(l); other != nil && other.ID != card.ID {
			return fmt.Errorf("%s is already linked to %s. Unlink it there first", l, other.Display)
		}
	}
	return nil
}

// Save creates a card (no ID) or replaces one, if nobody has changed it since expect.
func (c *Cards) Save(in *Card, expect int, who string) (*Card, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	clean, err := c.cleanCard(in)
	if err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	var before *Card
	if in.ID == "" {
		clean.ID, clean.Version, clean.CreatedMs = newCardID(), 1, now
	} else {
		_, existing := c.find(in.ID)
		if existing == nil {
			return nil, errCardNotFound
		}
		if existing.Version != expect {
			return nil, errCardChanged
		}
		before = cloneCard(existing)
		clean.Version, clean.CreatedMs = existing.Version+1, existing.CreatedMs
	}
	clean.UpdatedMs = now
	if err := c.checkLinks(clean); err != nil {
		return nil, err
	}
	action := "create"
	if before != nil {
		action = "update"
	}
	if err := c.commit(who, []cardChange{{before, clean, action}}); err != nil {
		return nil, err
	}
	return cloneCard(clean), nil
}

// Delete removes a card. It stays in the log, so it can be brought back.
func (c *Cards) Delete(id string, expect int, who string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, existing := c.find(id)
	if existing == nil {
		return errCardNotFound
	}
	if existing.Version != expect {
		return errCardChanged
	}
	return c.commit(who, []cardChange{{cloneCard(existing), nil, "delete"}})
}

func (c *Cards) readLog() []cardLogEntry {
	f, err := os.Open(filepath.Join(c.dir, cardsLogFile))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []cardLogEntry
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 1<<20), 16<<20)
	for scan.Scan() {
		var e cardLogEntry
		if json.Unmarshal(scan.Bytes(), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}

// History is one card's changes, newest first.
func (c *Cards) History(id string) []cardLogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []cardLogEntry
	for _, e := range c.readLog() {
		if e.ID == id {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	return out
}

// Deleted lists cards that are gone now, each by its latest change, newest first, so a delete can be undone.
func (c *Cards) Deleted() []cardLogEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	latest := map[string]cardLogEntry{}
	for _, e := range c.readLog() {
		latest[e.ID] = e
	}
	var out []cardLogEntry
	for id, e := range latest {
		if _, current := c.find(id); current == nil && e.After == nil && e.Before != nil {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if len(out) > 100 {
		out = out[:100]
	}
	return out
}

// Undo puts a card back as it was before one logged change. Undoing a create removes the card; undoing a delete
// brings it back. It's a new change of its own, so it can be undone too.
func (c *Cards) Undo(seq int, who string) (*Card, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var entry *cardLogEntry
	for _, e := range c.readLog() {
		if e.Seq == seq {
			e := e
			entry = &e
		}
	}
	if entry == nil {
		return nil, errors.New("that change isn't in the log")
	}
	_, current := c.find(entry.ID)
	if entry.Before == nil {
		if current == nil {
			return nil, nil
		}
		return nil, c.commit(who, []cardChange{{cloneCard(current), nil, "undo"}})
	}
	restored := cloneCard(entry.Before)
	restored.UpdatedMs = time.Now().UnixMilli()
	if current != nil {
		restored.Version = current.Version + 1
	} else {
		restored.Version++
	}
	// An address linked elsewhere since then stays where it is.
	kept := restored.Links[:0]
	for _, l := range restored.Links {
		if other := c.forLink(l); other == nil || other.ID == restored.ID {
			kept = append(kept, l)
		}
	}
	restored.Links = kept
	if err := c.commit(who, []cardChange{{cloneCard(current), restored, "undo"}}); err != nil {
		return nil, err
	}
	return cloneCard(restored), nil
}

// --- Finding ---

func cardText(card *Card) string {
	parts := []string{card.Display, card.Name.Given, card.Name.Middle, card.Name.Family, card.Nickname, card.Company, card.Department, card.Title, card.Notes}
	for _, list := range [][]Labeled{card.Phones, card.Emails, card.URLs, card.Related, card.Handles, card.Dates} {
		for _, it := range list {
			parts = append(parts, it.Value, it.Label)
		}
	}
	for _, a := range card.Addresses {
		parts = append(parts, a.Street, a.City, a.Region, a.Postcode, a.Country)
	}
	parts = append(parts, card.Links...)
	return strings.ToLower(strings.Join(parts, "\n"))
}

// Search matches any field, and a number by its digits however it's written.
func (c *Cards) Search(query string, limit int) []*Card {
	c.mu.Lock()
	defer c.mu.Unlock()
	q := strings.ToLower(strings.TrimSpace(query))
	digits := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, q)
	var out []*Card
	for _, card := range c.doc.Cards {
		hit := q == "" || strings.Contains(cardText(card), q)
		if !hit && len(digits) >= 4 {
			for _, p := range card.Phones {
				if d, ok := phoneDigits(p.Value); ok && strings.Contains(d, digits) {
					hit = true
				}
			}
			for _, l := range card.Links {
				if strings.Contains(l, digits) {
					hit = true
				}
			}
		}
		if hit {
			out = append(out, cloneCard(card))
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Display) < strings.ToLower(out[j].Display) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func (c *Cards) matchExisting(in *Card) []*Card {
	phones, emails := map[string]bool{}, map[string]bool{}
	for _, p := range in.Phones {
		if d, ok := phoneDigits(p.Value); ok {
			phones[d] = true
		}
	}
	for _, e := range in.Emails {
		emails[strings.ToLower(e.Value)] = true
	}
	var out []*Card
	for _, card := range c.doc.Cards {
		match := false
		for _, p := range card.Phones {
			if d, ok := phoneDigits(p.Value); ok && phones[d] {
				match = true
			}
		}
		for _, e := range card.Emails {
			if emails[strings.ToLower(e.Value)] {
				match = true
			}
		}
		if match {
			out = append(out, card)
		}
	}
	return out
}

// --- HTTP ---

func whoCalled(r *http.Request) string {
	if email := strings.TrimSpace(r.Header.Get(whoHeader)); email != "" {
		return "app:" + email
	}
	return "connector"
}

func (a *App) cardsChanged() {
	a.hub.Publish(map[string]any{"type": "cards"})
	a.contactsChanged()
}

func (a *App) handleCards(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("q"); q != "" || r.URL.Query().Has("q") {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 200 {
			limit = 20
		}
		writeJSON(w, 200, map[string]any{"cards": a.cards.Search(q, limit)})
		return
	}
	if jid := r.URL.Query().Get("jid"); jid != "" {
		writeJSON(w, 200, map[string]any{"card": a.cards.ForLink(jid)})
		return
	}
	cards, rev := a.cards.All()
	writeJSON(w, 200, map[string]any{"cards": cards, "rev": rev})
}

func cardError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errCardChanged):
		fail(w, 409, err.Error())
	case errors.Is(err, errCardNotFound):
		fail(w, 404, err.Error())
	default:
		fail(w, 400, err.Error())
	}
}

func (a *App) handleCardSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Card   Card `json:"card"`
		Expect int  `json:"expect_version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	saved, err := a.cards.Save(&req.Card, req.Expect, whoCalled(r))
	if err != nil {
		cardError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "card": saved})
}

func (a *App) handleCardDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Expect int    `json:"expect_version"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	// Only the app deletes cards. The connector has no delete tool, and a request without a verified login isn't one.
	if r.Header.Get(whoHeader) == "" {
		fail(w, 409, "Cards can only be deleted in the WA Relay app.")
		return
	}
	if err := a.cards.Delete(req.ID, req.Expect, whoCalled(r)); err != nil {
		cardError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleCardHistory(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("deleted") {
		writeJSON(w, 200, map[string]any{"entries": a.cards.Deleted()})
		return
	}
	writeJSON(w, 200, map[string]any{"entries": a.cards.History(r.URL.Query().Get("id"))})
}

func (a *App) handleCardUndo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seq int `json:"seq"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		fail(w, 400, "bad request")
		return
	}
	if r.Header.Get(whoHeader) == "" {
		fail(w, 409, "Changes can only be undone in the WA Relay app.")
		return
	}
	card, err := a.cards.Undo(req.Seq, whoCalled(r))
	if err != nil {
		cardError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "card": card})
}

type linkSuggestion struct {
	JID      string `json:"jid"`
	Name     string `json:"name"`
	Why      string `json:"why"`
	LinkedTo string `json:"linked_to,omitempty"`
}

// knownPeople is everyone WhatsApp knows of here, by address, with their best name.
func (a *App) knownPeople(ctx context.Context) map[string]string {
	out := map[string]string{}
	if contacts, err := a.wa.Contacts(ctx); err == nil {
		for jid, c := range contacts {
			name := c.FullName
			for _, n := range []string{c.FirstName, c.PushName, c.BusinessName} {
				if name == "" {
					name = n
				}
			}
			out[a.wa.Canon(jid.ToNonAD().String())] = name
		}
	}
	rows, err := a.store.db.QueryContext(ctx, `SELECT jid, COALESCE(name, '') FROM chats WHERE jid LIKE '%@s.whatsapp.net' OR jid LIKE '%@lid'`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var jid, name string
			if rows.Scan(&jid, &name) == nil {
				key := a.wa.Canon(jid)
				if out[key] == "" {
					out[key] = name
				}
			}
		}
	}
	for jid, name := range a.names.All() {
		out[a.wa.Canon(jid)] = name
	}
	return out
}

// suggest finds WhatsApp people for a card: exactly the same number is a match, the same name only a possibility.
func (a *App) suggest(ctx context.Context, card *Card) []linkSuggestion {
	people := a.knownPeople(ctx)
	linked := map[string]bool{}
	for _, l := range card.Links {
		linked[l] = true
	}
	var out []linkSuggestion
	seen := map[string]bool{}
	for _, p := range card.Phones {
		d, ok := phoneDigits(p.Value)
		if !ok {
			continue
		}
		key := a.wa.Canon(d + "@" + types.DefaultUserServer)
		if _, known := people[key]; !known || linked[key] || seen[key] {
			continue
		}
		seen[key] = true
		s := linkSuggestion{JID: key, Name: people[key], Why: "same number"}
		if other := a.cards.ForLink(key); other != nil && other.ID != card.ID {
			s.LinkedTo = other.Display
		}
		out = append(out, s)
	}
	want := strings.ToLower(card.Display)
	for key, name := range people {
		if name == "" || linked[key] || seen[key] || strings.ToLower(name) != want {
			continue
		}
		seen[key] = true
		s := linkSuggestion{JID: key, Name: name, Why: "same name only"}
		if other := a.cards.ForLink(key); other != nil && other.ID != card.ID {
			s.LinkedTo = other.Display
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Why != out[j].Why {
			return out[i].Why == "same number"
		}
		return out[i].JID < out[j].JID
	})
	return out
}

func (a *App) handleCardSuggest(w http.ResponseWriter, r *http.Request) {
	var card Card
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&card); err != nil {
		fail(w, 400, "bad request")
		return
	}
	writeJSON(w, 200, map[string]any{"suggestions": a.suggest(r.Context(), &card)})
}

// --- Import ---

type importConflict struct {
	Field    string `json:"field"`
	Existing string `json:"existing"`
	Incoming string `json:"incoming"`
}

type importItem struct {
	Index     int              `json:"index"`
	Display   string           `json:"display"`
	Kind      string           `json:"kind"` // new, merge, ambiguous, invalid
	MatchID   string           `json:"match_id,omitempty"`
	MatchName string           `json:"match_name,omitempty"`
	Matches   []string         `json:"matches,omitempty"`
	Adds      []string         `json:"adds,omitempty"`
	Conflicts []importConflict `json:"conflicts,omitempty"`
	Links     []string         `json:"links,omitempty"`
	Error     string           `json:"error,omitempty"`
}

type importChoice struct {
	Index  int      `json:"index"`
	Action string   `json:"action"` // add, merge, skip
	Take   []string `json:"take,omitempty"`
}

var singleFields = []string{"display", "prefix", "given", "middle", "family", "suffix", "nickname", "company", "department", "title", "notes"}

func singleField(c *Card, field string) *string {
	switch field {
	case "display":
		return &c.Display
	case "prefix":
		return &c.Name.Prefix
	case "given":
		return &c.Name.Given
	case "middle":
		return &c.Name.Middle
	case "family":
		return &c.Name.Family
	case "suffix":
		return &c.Name.Suffix
	case "nickname":
		return &c.Nickname
	case "company":
		return &c.Company
	case "department":
		return &c.Department
	case "title":
		return &c.Title
	case "notes":
		return &c.Notes
	}
	return nil
}

func listFields(c *Card) map[string]*[]Labeled {
	return map[string]*[]Labeled{"phones": &c.Phones, "emails": &c.Emails, "urls": &c.URLs, "dates": &c.Dates, "related": &c.Related, "handles": &c.Handles}
}

func labeledKey(field, value string) string {
	if field == "phones" {
		if d, ok := phoneDigits(value); ok {
			return d
		}
	}
	return strings.ToLower(value)
}

// merge adds what's new from incoming to existing. A single field that differs is only replaced when the user chose it.
func merge(existing, incoming *Card, take map[string]bool) (*Card, []string, []importConflict) {
	out := cloneCard(existing)
	var adds []string
	var conflicts []importConflict
	for _, f := range singleFields {
		have, got := singleField(out, f), singleField(incoming, f)
		if *got == "" || *got == *have {
			continue
		}
		if *have == "" {
			*have = *got
			adds = append(adds, f)
			continue
		}
		conflicts = append(conflicts, importConflict{Field: f, Existing: *have, Incoming: *got})
		if take[f] {
			*have = *got
		}
	}
	inLists := listFields(incoming)
	for f, list := range listFields(out) {
		seen := map[string]bool{}
		for _, it := range *list {
			seen[labeledKey(f, it.Value)] = true
		}
		for _, it := range *inLists[f] {
			if !seen[labeledKey(f, it.Value)] {
				*list = append(*list, it)
				seen[labeledKey(f, it.Value)] = true
				adds = append(adds, f)
			}
		}
	}
	for _, addr := range incoming.Addresses {
		dup := false
		for _, have := range out.Addresses {
			if strings.EqualFold(have.Street+have.City+have.Postcode, addr.Street+addr.City+addr.Postcode) {
				dup = true
			}
		}
		if !dup {
			out.Addresses = append(out.Addresses, addr)
			adds = append(adds, "addresses")
		}
	}
	sort.Strings(adds)
	uniq := adds[:0]
	for i, f := range adds {
		if i == 0 || adds[i-1] != f {
			uniq = append(uniq, f)
		}
	}
	return out, uniq, conflicts
}

func (a *App) handleCardImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cards     []Card         `json:"cards"`
		Apply     bool           `json:"apply"`
		Choices   []importChoice `json:"choices"`
		LinkExact bool           `json:"link_exact"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, cardsMaxRequest)).Decode(&req); err != nil {
		fail(w, 400, "That file is too big or isn't readable.")
		return
	}
	if len(req.Cards) > maxImportCards {
		fail(w, 400, fmt.Sprintf("Up to %d cards can be imported at once.", maxImportCards))
		return
	}
	if req.Apply && r.Header.Get(whoHeader) == "" {
		fail(w, 409, "Cards can only be imported in the WA Relay app.")
		return
	}
	people := a.knownPeople(r.Context())
	choices := map[int]importChoice{}
	for _, ch := range req.Choices {
		choices[ch.Index] = ch
	}
	a.cards.mu.Lock()
	defer a.cards.mu.Unlock()
	var items []importItem
	var changes []cardChange
	claimed := map[string]bool{}
	now := time.Now().UnixMilli()
	for i := range req.Cards {
		in := req.Cards[i]
		in.ID, in.Links = "", nil
		clean, err := a.cards.cleanCard(&in)
		if err != nil {
			items = append(items, importItem{Index: i, Kind: "invalid", Error: err.Error(), Display: in.Display})
			continue
		}
		item := importItem{Index: i, Display: clean.Display}
		matches := a.cards.matchExisting(clean)
		take := map[string]bool{}
		for _, f := range choices[i].Take {
			take[f] = true
		}
		switch len(matches) {
		case 0:
			item.Kind = "new"
			if req.LinkExact {
				for _, l := range a.exactLinksLocked(clean, people) {
					if !claimed[l] {
						claimed[l] = true
						item.Links = append(item.Links, l)
					}
				}
			}
			if req.Apply && choices[i].Action != "skip" {
				clean.ID, clean.Version, clean.CreatedMs, clean.UpdatedMs = newCardID(), 1, now, now
				clean.Links = item.Links
				changes = append(changes, cardChange{nil, clean, "import"})
			}
		case 1:
			item.Kind, item.MatchID, item.MatchName = "merge", matches[0].ID, matches[0].Display
			merged, adds, conflicts := merge(matches[0], clean, take)
			item.Adds, item.Conflicts = adds, conflicts
			if req.Apply && choices[i].Action != "skip" && (len(adds) > 0 || len(take) > 0) {
				merged.Version++
				merged.UpdatedMs = now
				changes = append(changes, cardChange{cloneCard(matches[0]), merged, "import"})
			}
		default:
			item.Kind = "ambiguous"
			for _, m := range matches {
				item.Matches = append(item.Matches, m.Display)
			}
			if req.Apply && choices[i].Action == "add" {
				clean.ID, clean.Version, clean.CreatedMs, clean.UpdatedMs = newCardID(), 1, now, now
				changes = append(changes, cardChange{nil, clean, "import"})
			}
		}
		items = append(items, item)
	}
	if !req.Apply {
		writeJSON(w, 200, map[string]any{"ok": true, "items": items})
		return
	}
	// Two incoming cards that merge into the same card are applied one after the other.
	byID := map[string]int{}
	var merged []cardChange
	for _, ch := range changes {
		if ch.before != nil {
			if at, ok := byID[ch.before.ID]; ok {
				again, _, _ := merge(merged[at].after, ch.after, nil)
				merged[at].after = again
				continue
			}
			byID[ch.before.ID] = len(merged)
		}
		merged = append(merged, ch)
	}
	if len(merged) > 0 {
		if err := a.cards.commit(whoCalled(r), merged); err != nil {
			fail(w, 500, err.Error())
			return
		}
	}
	added, updated := 0, 0
	for _, ch := range merged {
		if ch.before == nil {
			added++
		} else {
			updated++
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "updated": updated, "items": items})
}

// exactLinksLocked is exactLinks for when the cards lock is already held.
func (a *App) exactLinksLocked(card *Card, people map[string]string) []string {
	var out []string
	for _, p := range card.Phones {
		if d, ok := phoneDigits(p.Value); ok {
			key := a.wa.Canon(d + "@" + types.DefaultUserServer)
			if _, known := people[key]; known && a.cards.forLink(key) == nil {
				out = append(out, key)
			}
		}
	}
	return out
}
