package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func nameRequest(t *testing.T, app *App, jid, name string) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"jid": jid, "name": name})
	w := httptest.NewRecorder()
	app.handleNameSet(w, httptest.NewRequest(http.MethodPost, "/api/app/name", bytes.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func contactsOf(t *testing.T, app *App) map[string]string {
	t.Helper()
	w := httptest.NewRecorder()
	app.handleContacts(w, httptest.NewRequest(http.MethodGet, "/api/app/contacts", nil))
	var out struct {
		Contacts []contactRow `json:"contacts"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, row := range out.Contacts {
		names[row.JID] = row.Full
	}
	return names
}

func TestNameShowsAtBothAddresses(t *testing.T) {
	h := newHarness(t)
	// testLID and testDM are one person. Naming by either covers both.
	if code, out := nameRequest(t, h.app, testLID, "Sam Smith (ACME)"); code != 200 || out["jid"] != testDM {
		t.Fatalf("%d %v", code, out)
	}
	got := contactsOf(t, h.app)
	if got[testDM] != "Sam Smith (ACME)" || got[testLID] != "Sam Smith (ACME)" {
		t.Fatalf("got %v", got)
	}
}

func TestNameBeatsSavedNameAndRemovalRestoresIt(t *testing.T) {
	h := newHarness(t)
	const saved = "15550000001@s.whatsapp.net"
	if contactsOf(t, h.app)[saved] != "Sample Person" {
		t.Fatal("fake contact changed")
	}
	nameRequest(t, h.app, saved, "Renamed")
	if contactsOf(t, h.app)[saved] != "Renamed" {
		t.Fatal("name didn't apply")
	}
	w := httptest.NewRecorder()
	h.app.handleNameRemove(w, httptest.NewRequest(http.MethodDelete, "/api/app/name?jid="+saved, nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"changed":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if contactsOf(t, h.app)[saved] != "Sample Person" {
		t.Fatal("saved name didn't come back")
	}
}

func TestRemoveByOtherAddress(t *testing.T) {
	h := newHarness(t)
	nameRequest(t, h.app, testDM, "Sam")
	w := httptest.NewRecorder()
	h.app.handleNameRemove(w, httptest.NewRequest(http.MethodDelete, "/api/app/name?jid="+testLID, nil))
	if !strings.Contains(w.Body.String(), `"changed":true`) {
		t.Fatal(w.Body.String())
	}
	if contactsOf(t, h.app)[testDM] == "Sam" {
		t.Fatal("name stayed")
	}
}

func TestUnknownHiddenIDCanBeNamed(t *testing.T) {
	h := newHarness(t)
	const stranger = "424242424242@lid"
	if code, out := nameRequest(t, h.app, stranger, "Group Member"); code != 200 || out["jid"] != stranger {
		t.Fatalf("%d %v", code, out)
	}
	if contactsOf(t, h.app)[stranger] != "Group Member" {
		t.Fatal("row wasn't added")
	}
	if !h.app.knownChat(context.Background(), stranger) {
		t.Fatal("a named person should count for pictures")
	}
}

func TestPhoneNumberNameWinsOverHiddenID(t *testing.T) {
	rows := withNames(nil, map[string]string{testLID: "Older", testDM: "Newer"}, [][2]string{{testLID, testDM}})
	for _, row := range rows {
		if row.Full != "Newer" {
			t.Fatalf("%+v", rows)
		}
	}
}

func TestNameRefused(t *testing.T) {
	h := newHarness(t)
	for label, c := range map[string][2]string{
		"group":     {testGroup, "Nope"},
		"status":    {"status@broadcast", "Nope"},
		"broadcast": {"123@broadcast", "Nope"},
		"bad":       {"not an address", "Nope"},
		"empty":     {testDM, "   "},
		"invisible": {testDM, "​‮"},
		"too long":  {testDM, strings.Repeat("x", maxNameRunes+1)},
	} {
		if code, _ := nameRequest(t, h.app, c[0], c[1]); code != 400 {
			t.Errorf("%s: code %d", label, code)
		}
	}
	if len(h.app.names.All()) != 0 {
		t.Fatalf("something was saved: %v", h.app.names.All())
	}
}

func TestNameCleaned(t *testing.T) {
	got, err := cleanName("  Sam\t\n‮Smith​  (ACME) \x07")
	if err != nil || got != "Sam Smith (ACME)" {
		t.Fatalf("%q %v", got, err)
	}
	family := "\U0001F468‍\U0001F469"
	if got, err := cleanName(family); err != nil || got != family {
		t.Fatalf("emoji broken: %q %v", got, err)
	}
}

func TestNamesSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-names.json")
	n, err := NewNames(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Set(testDM, "Sam"); err != nil {
		t.Fatal(err)
	}
	again, err := NewNames(path)
	if err != nil || again.All()[testDM] != "Sam" {
		t.Fatalf("%v %v", again.All(), err)
	}
}

func TestDamagedFileSetAside(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app-names.json")
	if err := os.WriteFile(path, []byte("{damaged"), 0o600); err != nil {
		t.Fatal(err)
	}
	n, err := NewNames(path)
	if err != nil || len(n.All()) != 0 {
		t.Fatalf("%v %v", n.All(), err)
	}
	if _, err := os.Stat(path + ".bad"); err != nil {
		t.Fatal("damaged file wasn't kept")
	}
}

// A name is only a label. It must not reach WhatsApp.
func TestNamingSendsNothing(t *testing.T) {
	h := newHarness(t)
	nameRequest(t, h.app, testDM, "Sam")
	h.wa.mu.Lock()
	defer h.wa.mu.Unlock()
	if len(h.wa.sent) != 0 {
		t.Fatalf("sent %d messages", len(h.wa.sent))
	}
}
