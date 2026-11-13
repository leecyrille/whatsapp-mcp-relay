package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Made-up people only.

func (h *harness) send(method, path string, body any, who string, into any) int {
	h.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, h.srv.URL+path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		req.Header.Set(whoHeader, who)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

type cardReply struct {
	OK    bool   `json:"ok"`
	Card  *Card  `json:"card"`
	Error string `json:"error"`
}

func (h *harness) saveCard(card Card, expect int, who string) (int, cardReply) {
	h.t.Helper()
	var out cardReply
	code := h.send("POST", "/api/app/cards/save", map[string]any{"card": card, "expect_version": expect}, who, &out)
	return code, out
}

const lee = "lee@example.com"

func TestPhoneNumbersAreReadAnyUsualWay(t *testing.T) {
	cases := map[string]string{
		"+1 (780) 555-0101": "17805550101", "780-555-0101": "17805550101", "(780) 555 0101": "17805550101",
		"0044 20 7946 0000": "442079460000", "+44 20 7946 0000": "442079460000", "1 780 555 0101": "17805550101",
	}
	for in, want := range cases {
		if got, ok := phoneDigits(in); !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"call me", "12", "+1 780 555 0101 ext 4"} {
		if _, ok := phoneDigits(bad); ok {
			t.Errorf("%q should be refused", bad)
		}
	}
}

func TestACardIsSavedCheckedAndVersioned(t *testing.T) {
	h := newHarness(t)
	code, out := h.saveCard(Card{Name: CardName{Given: "Ann", Family: "Example"}, Emails: []Labeled{{"work", "Ann@Example.COM"}},
		Phones: []Labeled{{"mobile", "+1 780 555 0101"}}, Dates: []Labeled{{"birthday", "--05-31"}}, Company: "Made-up Co"}, 0, lee)
	if code != 200 || out.Card.Display != "Ann Example" || out.Card.Version != 1 || out.Card.Emails[0].Value != "Ann@example.com" {
		t.Fatalf("create: %d %+v", code, out)
	}
	first := out.Card
	first.Title = "Engineer"
	if code, out = h.saveCard(*first, 1, lee); code != 200 || out.Card.Version != 2 {
		t.Fatalf("update: %d %+v", code, out)
	}
	first.Title = "Stale"
	if code, out = h.saveCard(*first, 1, "connector"); code != 409 || !strings.Contains(out.Error, "changed somewhere else") {
		t.Fatalf("a stale edit must be refused: %d %+v", code, out)
	}
	for name, bad := range map[string]Card{
		"empty":    {},
		"email":    {Display: "X", Emails: []Labeled{{"", "not an email"}}},
		"date":     {Display: "X", Dates: []Labeled{{"birthday", "31/05/2024"}}},
		"phone":    {Display: "X", Phones: []Labeled{{"", "call me"}}},
		"link":     {Display: "X", Links: []string{testGroup}},
		"too many": {Display: "X", URLs: make([]Labeled, 21)},
	} {
		if name == "too many" {
			for i := range bad.URLs {
				bad.URLs[i].Value = "https://example.com/" + string(rune('a'+i))
			}
		}
		if code, _ := h.saveCard(bad, 0, lee); code != 400 {
			t.Errorf("%s: got %d", name, code)
		}
	}
	all, _ := h.app.cards.All()
	if len(all) != 1 || all[0].Title != "Engineer" {
		t.Fatalf("only the good card, at its latest: %+v", all)
	}
}

func TestCardsSurviveARestartAndADamagedFileIsSetAside(t *testing.T) {
	dir := t.TempDir()
	names, _ := NewNames(filepath.Join(dir, "app-names.json"))
	same := func(s string) string { return s }
	c, _ := NewCards(dir, names, same, nil)
	if _, err := c.Save(&Card{Display: "Kept"}, 0, lee); err != nil {
		t.Fatal(err)
	}
	again, _ := NewCards(dir, names, same, nil)
	if all, _ := again.All(); len(all) != 1 || all[0].Display != "Kept" {
		t.Fatalf("not kept: %+v", all)
	}
	if _, err := again.Save(&Card{Display: "Second"}, 0, lee); err != nil {
		t.Fatal(err)
	}
	if again.logSeq != 2 {
		t.Fatalf("log numbering must carry on after a restart: %d", again.logSeq)
	}
	os.WriteFile(filepath.Join(dir, cardsFile), []byte("{broken"), 0o600)
	damaged, err := NewCards(dir, names, same, nil)
	if err != nil {
		t.Fatal(err)
	}
	if all, _ := damaged.All(); len(all) != 0 {
		t.Fatal("a damaged file starts empty")
	}
	if _, err := os.Stat(filepath.Join(dir, cardsFile+".bad")); err != nil {
		t.Fatal("and is kept aside, not overwritten")
	}
}

func TestALinkedCardNamesThePersonEverywhere(t *testing.T) {
	h := newHarness(t)
	_, out := h.saveCard(Card{Display: "Card Name", Links: []string{testDM}}, 0, lee)
	if h.app.names.All()[testDM] != "Card Name" {
		t.Fatalf("names: %v", h.app.names.All())
	}
	// Renaming the person changes the card, so they can't disagree.
	if code := h.send("POST", "/api/app/name", map[string]any{"jid": testDM, "name": "Renamed"}, lee, nil); code != 200 {
		t.Fatalf("rename: %d", code)
	}
	card := h.app.cards.ForLink(testDM)
	if card.Display != "Renamed" || h.app.names.All()[testDM] != "Renamed" {
		t.Fatalf("card %q, name %q", card.Display, h.app.names.All()[testDM])
	}
	if code := h.send("DELETE", "/api/app/name?jid="+testDM, nil, lee, nil); code != 409 {
		t.Fatalf("removing the name of someone with a card: %d", code)
	}
	// The hidden address finds the same card.
	if h.app.cards.ForLink(testLID) == nil {
		t.Fatal("either address must find the card")
	}
	// A second card can't take the same person.
	if code, _ := h.saveCard(Card{Display: "Other", Links: []string{testLID}}, 0, lee); code != 400 {
		t.Fatalf("one person, one card: %d", code)
	}
	// Unlinking takes the card's name back off.
	card.Links = nil
	if code, _ := h.saveCard(*card, card.Version, lee); code != 200 {
		t.Fatal("unlink")
	}
	if _, ok := h.app.names.All()[testDM]; ok {
		t.Fatalf("name left behind: %v", h.app.names.All())
	}
	_ = out
}

func TestOnlyTheAppDeletesAndEveryChangeCanBeUndone(t *testing.T) {
	h := newHarness(t)
	_, out := h.saveCard(Card{Display: "Undo Me", Links: []string{testDM}, Notes: "first"}, 0, lee)
	id := out.Card.ID
	edited := *out.Card
	edited.Notes = "a bad edit"
	h.saveCard(edited, 1, "")
	if code := h.send("POST", "/api/app/cards/delete", map[string]any{"id": id, "expect_version": 2}, "", nil); code != 409 {
		t.Fatalf("a delete without the app's login: %d", code)
	}
	var hist struct {
		Entries []cardLogEntry `json:"entries"`
	}
	h.getJSON("/api/app/cards/history?id="+id, &hist)
	if len(hist.Entries) != 2 || hist.Entries[0].Action != "update" || hist.Entries[0].Who != "connector" || hist.Entries[1].Who != "app:"+lee {
		t.Fatalf("history %+v", hist.Entries)
	}
	if code := h.send("POST", "/api/app/cards/undo", map[string]any{"seq": hist.Entries[0].Seq}, lee, nil); code != 200 {
		t.Fatalf("undo: %d", code)
	}
	if c := h.app.cards.ForLink(testDM); c.Notes != "first" || c.Version != 3 {
		t.Fatalf("the bad edit wasn't undone: %+v", c)
	}
	if code := h.send("POST", "/api/app/cards/delete", map[string]any{"id": id, "expect_version": 3}, lee, nil); code != 200 {
		t.Fatalf("delete: %d", code)
	}
	if h.app.cards.ForLink(testDM) != nil || h.app.names.Has(testDM) {
		t.Fatal("delete must remove the card and its name")
	}
	var gone struct {
		Entries []cardLogEntry `json:"entries"`
	}
	h.getJSON("/api/app/cards/history?deleted=1", &gone)
	if len(gone.Entries) != 1 || gone.Entries[0].ID != id || gone.Entries[0].Before.Display != "Undo Me" {
		t.Fatalf("a deleted card must be listed, to bring back: %+v", gone.Entries)
	}
	h.getJSON("/api/app/cards/history?id="+id, &hist)
	if code := h.send("POST", "/api/app/cards/undo", map[string]any{"seq": hist.Entries[0].Seq}, lee, nil); code != 200 {
		t.Fatalf("undo delete: %d", code)
	}
	h.getJSON("/api/app/cards/history?deleted=1", &gone)
	if len(gone.Entries) != 0 {
		t.Fatalf("and not once it's back: %+v", gone.Entries)
	}
	back := h.app.cards.ForLink(testDM)
	if back == nil || back.ID != id || h.app.names.All()[testDM] != "Undo Me" {
		t.Fatalf("not brought back: %+v %v", back, h.app.names.All())
	}
	if code := h.send("POST", "/api/app/cards/undo", map[string]any{"seq": 1}, "", nil); code != 409 {
		t.Fatalf("undo without the app's login: %d", code)
	}
}

func TestSearchFindsAnyField(t *testing.T) {
	h := newHarness(t)
	h.saveCard(Card{Display: "Ann", Company: "Made-up Co", Phones: []Labeled{{"", "+1 (780) 555-0101"}},
		Addresses: []CardAddress{{City: "Sampletown"}}, Emails: []Labeled{{"", "ann@example.com"}}}, 0, lee)
	h.saveCard(Card{Display: "Bob", Notes: "met at the made-up fair"}, 0, lee)
	for q, want := range map[string]string{"made-up co": "Ann", "sampletown": "Ann", "780.555.0101": "Ann", "5550101": "Ann", "EXAMPLE.com": "Ann", "fair": "Bob"} {
		var out struct {
			Cards []Card `json:"cards"`
		}
		h.getJSON("/api/app/cards?q="+strings.ReplaceAll(q, " ", "%20"), &out)
		if len(out.Cards) != 1 || out.Cards[0].Display != want {
			t.Errorf("%q: %+v", q, out.Cards)
		}
	}
}

func TestLinkSuggestionsNeverRestOnANameAlone(t *testing.T) {
	h := newHarness(t)
	var out struct {
		Suggestions []linkSuggestion `json:"suggestions"`
	}
	h.send("POST", "/api/app/cards/suggest", Card{Display: "Nobody Here", Phones: []Labeled{{"", "555-000-0009"}}}, lee, &out)
	if len(out.Suggestions) != 0 {
		t.Fatalf("a number nobody has: %+v", out.Suggestions)
	}
	// Written without a country code, it's still the same number.
	h.send("POST", "/api/app/cards/suggest", Card{Display: "Nobody Here", Phones: []Labeled{{"", "555-000-0001"}}}, lee, &out)
	if len(out.Suggestions) != 1 || out.Suggestions[0].Why != "same number" {
		t.Fatalf("a local number: %+v", out.Suggestions)
	}
	h.send("POST", "/api/app/cards/suggest", Card{Display: "Someone Else", Phones: []Labeled{{"", "+1 555 000 0001"}}}, lee, &out)
	if len(out.Suggestions) != 1 || out.Suggestions[0].JID != testDM || out.Suggestions[0].Why != "same number" {
		t.Fatalf("exact number: %+v", out.Suggestions)
	}
	h.send("POST", "/api/app/cards/suggest", Card{Display: "sample person"}, lee, &out)
	if len(out.Suggestions) != 1 || out.Suggestions[0].Why != "same name only" {
		t.Fatalf("a name is only a possibility: %+v", out.Suggestions)
	}
}

func TestImportShowsWhatItWillDoBeforeSaving(t *testing.T) {
	h := newHarness(t)
	_, a := h.saveCard(Card{Display: "Ann Example", Company: "Old Co", Emails: []Labeled{{"", "ann@example.com"}}}, 0, lee)
	h.saveCard(Card{Display: "Twin One", Phones: []Labeled{{"", "+1 780 555 0199"}}}, 0, lee)
	h.saveCard(Card{Display: "Twin Two", Emails: []Labeled{{"", "twin@example.com"}}}, 0, lee)
	incoming := []Card{
		{Display: "Ann E.", Company: "New Co", Emails: []Labeled{{"", "ANN@example.com"}}, Phones: []Labeled{{"", "+1 780 555 0101"}}},
		{Display: "Brand New", Phones: []Labeled{{"", "+1 555 000 0001"}}},
		{Display: "Twins?", Phones: []Labeled{{"", "780-555-0199"}}, Emails: []Labeled{{"", "twin@example.com"}}},
		{Display: "Broken", Emails: []Labeled{{"", "nope"}}},
	}
	var preview struct {
		Items []importItem `json:"items"`
	}
	h.send("POST", "/api/app/cards/import", map[string]any{"cards": incoming, "link_exact": true}, lee, &preview)
	kinds := []string{}
	for _, it := range preview.Items {
		kinds = append(kinds, it.Kind)
	}
	if strings.Join(kinds, ",") != "merge,new,ambiguous,invalid" {
		t.Fatalf("kinds %v", kinds)
	}
	merge := preview.Items[0]
	if merge.MatchID != a.Card.ID || len(merge.Conflicts) != 2 || !strings.Contains(strings.Join(merge.Adds, ","), "phones") {
		t.Fatalf("merge %+v", merge)
	}
	if len(preview.Items[1].Links) != 1 || preview.Items[1].Links[0] != testDM {
		t.Fatalf("an exact number links: %+v", preview.Items[1])
	}
	if all, _ := h.app.cards.All(); len(all) != 3 {
		t.Fatal("a preview must change nothing")
	}
	if code := h.send("POST", "/api/app/cards/import", map[string]any{"cards": incoming, "apply": true}, "", nil); code != 409 {
		t.Fatalf("only the app imports: %d", code)
	}
	var done struct {
		Added, Updated int
	}
	choices := []importChoice{{Index: 0, Action: "merge", Take: []string{"company"}}, {Index: 1, Action: "add"}, {Index: 2, Action: "skip"}}
	if code := h.send("POST", "/api/app/cards/import", map[string]any{"cards": incoming, "apply": true, "choices": choices, "link_exact": true}, lee, &done); code != 200 || done.Added != 1 || done.Updated != 1 {
		t.Fatalf("apply: %d %+v", code, done)
	}
	ann := h.app.cards.Search("ann@example.com", 5)[0]
	if ann.Company != "New Co" || ann.Display != "Ann Example" || len(ann.Phones) != 1 {
		t.Fatalf("only the chosen conflict is taken, and new details added: %+v", ann)
	}
	if h.app.names.All()[testDM] != "Brand New" {
		t.Fatal("the linked new card names the person")
	}
	if all, _ := h.app.cards.All(); len(all) != 4 {
		t.Fatalf("the ambiguous one was skipped: %d", len(all))
	}
}
