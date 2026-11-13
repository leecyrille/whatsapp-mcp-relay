package relay

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"wa-relay/internal/access"
)

// Every chat name here is made up.

func jobRule(t *testing.T) *NameRule {
	t.Helper()
	rule, err := NewNameRule("Jobs", "job-number")
	if err != nil {
		t.Fatal(err)
	}
	return rule
}

func TestJobNumberMatching(t *testing.T) {
	rule := jobRule(t)
	yes := []string{
		"AB-1234", "ABC-1234", "ABCD-1234", "abc-1234", "aBcD-1234", "ABC-1234 Site Work", "ABCD-1234 Site Work",
		"ABC-1234-Taylor", "ABC-1234/5678 Pump", "ABC-1234 - Name", "  ABC-1234", "ABC-1234 " + strings.Repeat("x", 300),
		"ABCDE-1234", "Site ABC-1234", "(ABC-1234)", "WF-2601-Preventive Maintenance",
		"ABCO-A-2602-North Pump Replacement", "QRST-SC-2601-Titan Upgrade", "MNOPQ-2501 - Datalogger",
		"Model 900 Pump Upgrade (XYZ-2602) Crew Only", "Lakeview - DEF-2601", "Pine Hill-GHI-2603 & JKL-2601",
	}
	no := []string{
		"", "A-1234", "ABCDEF-1234", "ABC-12345", "ABC-123", "AB-123", "ABC 1234", "ABC1234", "ABC_1234", "ABC–1234", "ABC—1234",
		"A1C-1234", "12-1234", "ABC-12a4", "ABC--1234", "ABC-1234567", "АВС-1234", "Group 120363000000000001",
		"ABC-١٢٣٤", "CREW-Field", "CR-QUOTES", "CREW- Scheduling", "Pine Hill -", "2026 Chess Club Finals",
		"VOLUNTEERS - 2026 NORTH-EAST Fun Run", "Lake Weekend Oct 2-4",
	}
	for _, name := range yes {
		if !rule.matches(name) {
			t.Errorf("%q should match", name)
		}
	}
	for _, name := range no {
		if rule.matches(name) {
			t.Errorf("%q shouldn't match", name)
		}
	}
}

func TestNameRuleConfig(t *testing.T) {
	for _, bad := range []NameRuleConfig{{"", "job-number"}, {"Jobs", ""}, {"Jobs", "("}, {" ", " "}} {
		if _, err := bad.compile(); err == nil {
			t.Errorf("%+v should be refused", bad)
		}
	}
	var none *NameRuleConfig
	if rule, err := none.compile(); rule != nil || err != nil {
		t.Fatal("no name_rule should mean no rule")
	}
	custom, err := NewNameRule("Clients", `^Client: `)
	if err != nil || !custom.matches("Client: Made-up Ltd") || custom.matches("A client") {
		t.Fatalf("a custom pattern: %v", err)
	}
	cfg := Config{ListenPort: 8704, BridgeURL: "http://127.0.0.1:1", RunnerURL: "http://127.0.0.1:2", DataDir: "d", TokenFile: "t", CallsLog: "c",
		NameRule: &NameRuleConfig{Group: "Jobs", Pattern: "["}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("a broken pattern should stop the relay starting")
	}
}

// ruleGroups starts from the default groups, plus a group called Jobs for the rule.
func ruleGroups(t *testing.T) (*Groups, string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "groups.json")
	g, err := LoadGroups(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Create("Jobs", ""); err != nil {
		t.Fatal(err)
	}
	return g, path, groupID(t, g, "Jobs")
}

func groupID(t *testing.T, g *Groups, name string) string {
	t.Helper()
	for _, group := range g.Snapshot().Groups {
		if group.Name == name {
			return group.ID
		}
	}
	t.Fatalf("no group %q", name)
	return ""
}

func TestDefaultGroupsAreGenericAndRuleNeedsItsGroup(t *testing.T) {
	g, err := LoadGroups(filepath.Join(t.TempDir(), "groups.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, group := range g.Snapshot().Groups {
		names = append(names, group.Name)
	}
	if strings.Join(names, ",") != "Personal,Work" {
		t.Fatalf("defaults: %v", names)
	}
	if added, err := g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "ABC-1234"}}); added != 0 || err != nil {
		t.Fatalf("added %d with no Jobs group, %v", added, err)
	}
	if added, _ := g.ApplyNameRule(nil, []NamedChat{{"1@g.us", "ABC-1234"}}); added != 0 {
		t.Fatal("no rule added something")
	}
}

func TestRuleAddsOnlyMatchingGroupChats(t *testing.T) {
	g, _, jobs := ruleGroups(t)
	added, err := g.ApplyNameRule(jobRule(t), []NamedChat{
		{"1@g.us", "ABC-1234 Site"},
		{"2@g.us", "Family"},
		{"15550000001@s.whatsapp.net", "ABC-1234 A person"},
		{"99@lid", "ABC-1234 Another person"},
		{"3@broadcast", "ABC-1234"},
		{"../../x@g.us", "ABC-1234"},
	})
	if err != nil || added != 1 {
		t.Fatalf("added %d, %v", added, err)
	}
	assign := g.Snapshot().Assign
	if len(assign) != 1 || assign["1@g.us"] != jobs {
		t.Fatalf("assign: %v", assign)
	}
}

func TestRuleLeavesOtherGroupsAlone(t *testing.T) {
	g, _, _ := ruleGroups(t)
	g.Assign([]string{"1@g.us"}, "work")
	g.Assign([]string{"2@g.us"}, "personal")
	added, err := g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "ABC-1234"}, {"2@g.us", "AB-9999"}})
	if err != nil || added != 0 {
		t.Fatalf("added %d, %v", added, err)
	}
	assign := g.Snapshot().Assign
	if assign["1@g.us"] != "work" || assign["2@g.us"] != "personal" {
		t.Fatalf("moved a hand-placed chat: %v", assign)
	}
	// And it stays decided, so taking it out of Work later doesn't pull it into Jobs.
	g.Assign([]string{"1@g.us"}, "")
	g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "ABC-1234"}})
	if got := g.Snapshot().Assign["1@g.us"]; got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTakingAChatOutOfJobsSticks(t *testing.T) {
	g, path, _ := ruleGroups(t)
	chats := []NamedChat{{"1@g.us", "ABC-1234"}, {"2@g.us", "ABD-1234"}}
	g.ApplyNameRule(jobRule(t), chats)
	g.Assign([]string{"1@g.us"}, "")
	g.Assign([]string{"2@g.us"}, "personal")
	for i := 0; i < 3; i++ {
		if added, err := g.ApplyNameRule(jobRule(t), chats); err != nil || added != 0 {
			t.Fatalf("added %d, %v", added, err)
		}
	}
	// Including after the relay restarts, and if the group is renamed and comes back.
	reloaded, err := LoadGroups(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloaded.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "ABC-5678 Renamed"}, {"2@g.us", "ABC-1234"}})
	assign := reloaded.Snapshot().Assign
	if assign["1@g.us"] != "" || assign["2@g.us"] != "personal" {
		t.Fatalf("put back: %v", assign)
	}
}

func TestRuleOnlyAddsOnce(t *testing.T) {
	g, _, _ := ruleGroups(t)
	chats := []NamedChat{{"1@g.us", "ABC-1234"}}
	if added, _ := g.ApplyNameRule(jobRule(t), chats); added != 1 {
		t.Fatal("first time")
	}
	if added, _ := g.ApplyNameRule(jobRule(t), chats); added != 0 {
		t.Fatal("second time")
	}
}

func TestANewGroupAndARenameBothCount(t *testing.T) {
	g, _, _ := ruleGroups(t)
	g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "Not a project yet"}})
	if len(g.Snapshot().Assign) != 0 {
		t.Fatal("matched too early")
	}
	added, _ := g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "DEF-2699 Now a project"}, {"2@g.us", "NEW-2601 Just made"}})
	if added != 2 {
		t.Fatalf("added %d", added)
	}
}

func TestRuleFindsItsGroupByIDOrNameAndSkipsWhenGone(t *testing.T) {
	g, _, jobs := ruleGroups(t)
	byID, _ := NewNameRule(jobs, "job-number")
	name := "Projects"
	g.Update(jobs, &name, nil, nil)
	g.ApplyNameRule(byID, []NamedChat{{"1@g.us", "ABC-1234"}})
	if g.Snapshot().Assign["1@g.us"] != jobs {
		t.Fatal("lost track of a renamed group named by its ID")
	}
	g.Delete(jobs)
	if added, err := g.ApplyNameRule(jobRule(t), []NamedChat{{"2@g.us", "ABC-1235"}}); added != 0 || err != nil {
		t.Fatalf("added %d, %v", added, err)
	}
	if _, decided := g.doc.ProjectRule["2@g.us"]; decided {
		t.Fatal("decided a chat with no group to put it in")
	}
	// A new group with the rule's name picks the rule back up.
	if err := g.Create("jobs", ""); err != nil {
		t.Fatal(err)
	}
	if added, _ := g.ApplyNameRule(jobRule(t), []NamedChat{{"2@g.us", "ABC-1235"}}); added != 1 {
		t.Fatal("didn't use the new group")
	}
}

func TestGeneralChatsStayOutOfJobs(t *testing.T) {
	g, _, _ := ruleGroups(t)
	g.Assign([]string{"1@g.us"}, "work")
	added, _ := g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "CREW Management"}, {"2@g.us", "CREW-Field"}})
	if added != 0 || g.Snapshot().Assign["1@g.us"] != "work" || g.Snapshot().Assign["2@g.us"] != "" {
		t.Fatalf("added %d: %v", added, g.Snapshot().Assign)
	}
}

func TestClientsNeverSeeTheRulesNotes(t *testing.T) {
	g, _, _ := ruleGroups(t)
	g.ApplyNameRule(jobRule(t), []NamedChat{{"1@g.us", "ABC-1234"}})
	raw, _ := json.Marshal(g.Snapshot())
	if strings.Contains(string(raw), "project_rule") {
		t.Fatalf("sent to clients: %s", raw)
	}
}

func TestRuleTellsRunningAppsAndUsesTheBridgesNames(t *testing.T) {
	f := newFixture(t)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/app/group-names" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"chats": []NamedChat{{"1@g.us", "ABC-1234 Made-up site"}}})
	}))
	defer bridge.Close()
	cfg := f.cfg
	cfg.BridgeURL = bridge.URL
	cfg.DataDir = t.TempDir()
	cfg.TokenFile = filepath.Join(cfg.DataDir, "t")
	cfg.CallsLog = filepath.Join(cfg.DataDir, "c.log")
	srv, err := New(cfg, access.KeyFunc(testKeys))
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if err := srv.groups.Create("Jobs", ""); err != nil {
		t.Fatal(err)
	}
	jobs := groupID(t, srv.groups, "Jobs")
	sub := srv.Hub().Subscribe()
	defer srv.Hub().Unsubscribe(sub)
	if err := srv.applyNameRule(context.Background(), jobRule(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(<-sub), `"1@g.us":"`+jobs+`"`) {
		t.Fatal("running apps weren't told")
	}
	bridge.Close()
	if err := srv.applyNameRule(context.Background(), jobRule(t)); err == nil {
		t.Fatal("a stopped bridge should report an error and change nothing")
	}
	if len(srv.groups.Snapshot().Assign) != 1 {
		t.Fatal("a stopped bridge changed something")
	}
}
