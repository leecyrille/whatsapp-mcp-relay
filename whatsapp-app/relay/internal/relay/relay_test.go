package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"wa-relay/internal/access"
)

var testKey, _ = rsa.GenerateKey(rand.Reader, 2048)

func testKeys(string) (*rsa.PublicKey, error) { return &testKey.PublicKey, nil }

type seenRequest struct {
	Method, Path string
	Header       http.Header
	Body         string
}

type fixture struct {
	t                    *testing.T
	srv                  *Server
	front                *httptest.Server
	mu                   sync.Mutex
	bridge               []seenRequest
	runner               []seenRequest
	cfg                  Config
	bridgeSrv, runnerSrv *httptest.Server
	bridgeEvents         chan string
}

func (f *fixture) record(list *[]seenRequest, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	*list = append(*list, seenRequest{r.Method, r.URL.Path, r.Header.Clone(), string(body)})
	f.mu.Unlock()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, bridgeEvents: make(chan string, 8)}
	f.bridgeSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/app/events" {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprintf(w, "data: {\"type\":\"status\",\"connected\":true,\"logged_in\":true}\n\n")
			w.(http.Flusher).Flush()
			for {
				select {
				case <-r.Context().Done():
					return
				case ev := <-f.bridgeEvents:
					fmt.Fprintf(w, "data: %s\n\n", ev)
					w.(http.Flusher).Flush()
				}
			}
		}
		f.record(&f.bridge, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	f.runnerSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/events" {
			w.WriteHeader(503)
			return
		}
		f.record(&f.runner, r)
		fmt.Fprint(w, `{"ok":true}`)
	}))
	t.Cleanup(func() { f.bridgeSrv.CloseClientConnections(); f.bridgeSrv.Close(); f.runnerSrv.Close() })
	f.cfg = Config{
		TeamDomain: "myteam", AUD: "aud-1", AllowedEmails: []string{"user@example.com"}, ListenPort: 8704,
		BridgeURL: strings.Replace(f.bridgeSrv.URL, "127.0.0.1", "127.0.0.1", 1), RunnerURL: f.runnerSrv.URL,
		DataDir: dir, TokenFile: filepath.Join(dir, "runner.token"), DownloadsDir: filepath.Join(dir, "dl"),
		CallsLog: filepath.Join(dir, "calls.log"),
	}
	srv, err := New(f.cfg, func(string) (*rsa.PublicKey, error) { return &testKey.PublicKey, nil })
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	t.Cleanup(srv.Close)
	f.front = httptest.NewServer(srv.Handler())
	t.Cleanup(f.front.Close)
	return f
}

func goodToken(t *testing.T) string {
	t.Helper()
	now := time.Now().Unix()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://myteam.cloudflareaccess.com", "aud": []string{"aud-1"}, "email": "user@example.com", "iat": now, "exp": now + 300,
	})
	s, err := tok.SignedString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fixture) do(method, path string, token string, headers map[string]string, body string) *http.Response {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.front.URL+path, strings.NewReader(body))
	if token != "" {
		req.Header.Set(access.HeaderName, token)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp
}

func app() map[string]string { return map[string]string{ClientHeader: "1"} }

func TestEveryPathNeedsLoginBeforeAnythingElse(t *testing.T) {
	f := newFixture(t)
	paths := []string{"/", "/api/send", "/api/sync", "/api/events", "/api/claude/threads", "/api/claude/approvals/x",
		"/download", "/login-ok", "/api/app/send", "/mcp", "/.env", "/nope"}
	for _, p := range paths {
		for _, m := range []string{"GET", "POST"} {
			resp := f.do(m, p, "", app(), "{}")
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%s %s without login: %d", m, p, resp.StatusCode)
			}
			resp = f.do(m, p, "forged", app(), "{}")
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%s %s with forged token: %d", m, p, resp.StatusCode)
			}
		}
	}
	if len(f.bridge) != 0 || len(f.runner) != 0 {
		t.Fatalf("reached a backend: %d %d", len(f.bridge), len(f.runner))
	}
}

func TestWrongEmailIsForbidden(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Unix()
	tok, _ := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{
		"iss": "https://myteam.cloudflareaccess.com", "aud": []string{"aud-1"}, "email": "someone@else.com", "iat": now, "exp": now + 300,
	}).SignedString(testKey)
	resp := f.do("POST", "/api/send", tok, app(), `{}`)
	resp.Body.Close()
	if resp.StatusCode != 403 || len(f.bridge) != 0 {
		t.Fatalf("%d, reached backend %d times", resp.StatusCode, len(f.bridge))
	}
}

func TestBrowserRequestsToAPIAreRefused(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	cases := []map[string]string{
		{}, // A plain navigation or <img> tag: no client header.
		{ClientHeader: "1", "Origin": "https://evil.example"}, // Cross-site fetch, even if the header got through.
		{ClientHeader: "1", "Origin": "null"},
	}
	for _, headers := range cases {
		for _, target := range []string{"/api/send", "/api/claude/approvals/abc", "/api/assign"} {
			resp := f.do("POST", target, tok, headers, `{}`)
			resp.Body.Close()
			if resp.StatusCode != 403 {
				t.Errorf("%s %v: %d", target, headers, resp.StatusCode)
			}
		}
	}
	if len(f.bridge)+len(f.runner) != 0 {
		t.Fatal("reached a backend")
	}
}

func TestOnlyListedRoutesReachBackends(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	for _, route := range [][2]string{
		{"POST", "/api/app/send"}, {"GET", "/api/app/sync"}, {"POST", "/api/download"}, {"GET", "/api/health"},
		{"GET", "/api/send"}, {"DELETE", "/api/send"}, {"POST", "/api/sync"},
		{"GET", "/api/../api/app/send"}, {"POST", "/api/claude/mcp/secret"}, {"GET", "/api/claude/events"},
		{"PUT", "/api/claude/threads"}, {"POST", "/api/claude/status"},
	} {
		resp := f.do(route[0], route[1], tok, app(), "{}")
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%v was reachable", route)
		}
	}
	if len(f.bridge)+len(f.runner) != 0 {
		t.Fatalf("reached a backend: %+v %+v", f.bridge, f.runner)
	}
}

func TestBridgePathsAreMappedAndCredentialsStripped(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	resp := f.do("POST", "/api/send", tok, map[string]string{
		ClientHeader: "1", "Cookie": "CF_Authorization=x", "Authorization": "Bearer y", "X-Keep": "yes",
		RunnerTokenHeader: "spoofed", EmailHeader: "spoofed@evil.com",
	}, `{"chat":"x"}`)
	resp.Body.Close()
	if resp.StatusCode != 200 || len(f.bridge) != 1 {
		t.Fatalf("%d %d", resp.StatusCode, len(f.bridge))
	}
	got := f.bridge[0]
	if got.Path != "/api/app/send" || got.Body != `{"chat":"x"}` {
		t.Fatalf("%+v", got)
	}
	for _, h := range []string{"Cookie", "Authorization", access.HeaderName, RunnerTokenHeader, ClientHeader} {
		if got.Header.Get(h) != "" {
			t.Errorf("%s reached the bridge", h)
		}
	}
	// The bridge gets the verified login, for its contact card log, and never the one the client claimed.
	if got.Header.Get(EmailHeader) != "user@example.com" {
		t.Errorf("email %q", got.Header.Get(EmailHeader))
	}
	if got.Header.Get("X-Keep") != "yes" {
		t.Error("ordinary header dropped")
	}
}

func TestRunnerGetsTokenAndVerifiedEmailNotCallersValues(t *testing.T) {
	f := newFixture(t)
	resp := f.do("POST", "/api/claude/approvals/abc", goodToken(t), map[string]string{
		ClientHeader: "1", RunnerTokenHeader: "spoofed", EmailHeader: "spoofed@evil.com",
	}, `{"approve":true}`)
	resp.Body.Close()
	if len(f.runner) != 1 {
		t.Fatalf("runner calls: %d", len(f.runner))
	}
	got := f.runner[0]
	if got.Path != "/approvals/abc" {
		t.Fatal(got.Path)
	}
	if got.Header.Get(EmailHeader) != "user@example.com" {
		t.Fatalf("email %q", got.Header.Get(EmailHeader))
	}
	if got.Header.Get(RunnerTokenHeader) != f.srv.token || f.srv.token == "spoofed" {
		t.Fatal("wrong runner token")
	}
}

func TestLogHasNoTokensQueriesOrBodies(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	f.do("POST", "/api/send?chat=secret-chat", tok, app(), `{"text":"secret-text"}`).Body.Close()
	f.do("GET", "/api/sync", "", app(), "").Body.Close()
	f.srv.Close()
	raw, _ := os.ReadFile(f.cfg.CallsLog)
	text := string(raw)
	for _, secret := range []string{tok, "secret-chat", "secret-text"} {
		if strings.Contains(text, secret) {
			t.Errorf("log contains %q", secret[:6])
		}
	}
	if !strings.Contains(text, `"result":401`) || !strings.Contains(text, "user@example.com") {
		t.Fatalf("missing audit entries: %s", text)
	}
}

func TestGroupsPersistAndAssign(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	var doc GroupDoc
	resp := f.do("GET", "/api/groups", tok, app(), "")
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if len(doc.Groups) != 2 || doc.Groups[0].Name != "Personal" || doc.Groups[1].Name != "Work" {
		t.Fatalf("%+v", doc)
	}
	resp = f.do("POST", "/api/assign", tok, app(), `{"jids":["1@s.whatsapp.net","2@g.us"],"group":"work"}`)
	json.NewDecoder(resp.Body).Decode(&doc)
	resp.Body.Close()
	if doc.Assign["2@g.us"] != "work" {
		t.Fatalf("%+v", doc)
	}
	reloaded, err := LoadGroups(filepath.Join(f.cfg.DataDir, "groups.json"), nil)
	if err != nil || reloaded.Snapshot().Assign["1@s.whatsapp.net"] != "work" {
		t.Fatalf("not persisted: %v", err)
	}
}

func TestGroupRules(t *testing.T) {
	g, _ := LoadGroups(filepath.Join(t.TempDir(), "g.json"), nil)
	if err := g.Assign([]string{"1@s.whatsapp.net", "not a jid"}, "work"); err == nil {
		t.Fatal("bad address accepted")
	}
	if len(g.Snapshot().Assign) != 0 {
		t.Fatal("partial assign saved")
	}
	if err := g.Assign([]string{"1@s.whatsapp.net"}, "nope"); err == nil {
		t.Fatal("unknown group accepted")
	}
	if err := g.Assign([]string{"../../x@g.us"}, "work"); err == nil {
		t.Fatal("path-like address accepted")
	}
	if err := g.Create("Family", ""); err != nil {
		t.Fatal(err)
	}
	if err := g.Create("family", ""); err == nil {
		t.Fatal("duplicate accepted")
	}
	if err := g.Create("  ", ""); err == nil {
		t.Fatal("blank accepted")
	}
	if err := g.Create("x", "red; background:url(evil)"); err == nil {
		t.Fatal("bad color accepted")
	}
	g.Assign([]string{"1@s.whatsapp.net"}, "work")
	g.Delete("work")
	if _, ok := g.Snapshot().Assign["1@s.whatsapp.net"]; ok {
		t.Fatal("deleting a group must unsort its chats")
	}
	g.Assign([]string{"2@g.us"}, "personal")
	g.Assign([]string{"2@g.us"}, "")
	if len(g.Snapshot().Assign) != 0 {
		t.Fatal("empty group must unsort")
	}
}

func TestDamagedGroupsFileRefusesToStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "g.json")
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := LoadGroups(path, nil); err == nil {
		t.Fatal("would silently replace the user's sorting with defaults")
	}
}

func TestEventsCarryBridgeStatusThenLiveEvents(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.srv.pump(ctx, f.cfg.BridgeURL+"/api/app/events", "", f.srv.onBridgeEvent, func() {})
	deadline := time.Now().Add(3 * time.Second)
	for {
		f.srv.mu.Lock()
		ready := f.srv.bridgeStatus != nil
		f.srv.mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("bridge status never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", f.front.URL+"/api/events", nil)
	req.Header.Set(access.HeaderName, goodToken(t))
	req.Header.Set(ClientHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(resp.Body)
	want := []string{`"type":"status"`, `"type":"claude_status"`, `"type":"delta"`}
	f.bridgeEvents <- `{"type":"delta","seq":5}`
	for _, marker := range want {
		found := false
		for i := 0; i < 8 && !found; i++ {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			found = strings.Contains(line, marker)
		}
		if !found {
			t.Fatalf("never saw %s", marker)
		}
	}
}

func TestDownloadsOnlyServeInstallers(t *testing.T) {
	f := newFixture(t)
	os.MkdirAll(f.cfg.DownloadsDir, 0o700)
	os.WriteFile(filepath.Join(f.cfg.DownloadsDir, "WA Relay_1.0.0_x64-setup.exe"), []byte("MZ"), 0o600)
	os.WriteFile(filepath.Join(f.cfg.DownloadsDir, "secret.db"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(f.cfg.DataDir, "groups.json"), []byte("{}"), 0o600)
	tok := goodToken(t)
	resp := f.do("GET", "/download/WA%20Relay_1.0.0_x64-setup.exe", tok, nil, "")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || string(body) != "MZ" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
	for _, p := range []string{"/download/secret.db", "/download/..%2fgroups.json", "/download/..%5cgroups.json", "/download/nope.exe"} {
		resp := f.do("GET", p, tok, nil, "")
		resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Errorf("%s served", p)
		}
	}
}

func TestBadConfigFailsClosed(t *testing.T) {
	base := func() Config {
		d := t.TempDir()
		return Config{TeamDomain: "t", AUD: "a", AllowedEmails: []string{"a@b.c"}, ListenPort: 8704,
			BridgeURL: "http://127.0.0.1:1", RunnerURL: "http://127.0.0.1:2", DataDir: d,
			TokenFile: filepath.Join(d, "t"), CallsLog: filepath.Join(d, "l")}
	}
	keys := func(string) (*rsa.PublicKey, error) { return nil, nil }
	mutations := []func(*Config){
		func(c *Config) { c.AUD = "" }, func(c *Config) { c.AUD = "TODO" }, func(c *Config) { c.TeamDomain = "" },
		func(c *Config) { c.AllowedEmails = nil }, func(c *Config) { c.AllowedEmails = []string{"*"} },
		func(c *Config) { c.BridgeURL = "http://10.0.0.5:80" }, func(c *Config) { c.RunnerURL = "https://example.com" },
		func(c *Config) { c.ListenPort = 80 }, func(c *Config) { c.DataDir = "" }, func(c *Config) { c.CallsLog = "" },
	}
	for i, mutate := range mutations {
		c := base()
		mutate(&c)
		if _, err := New(c, keys); err == nil {
			t.Errorf("mutation %d accepted", i)
		}
	}
}

func TestEnsureTokenIsStableAndSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "t")
	var wg sync.WaitGroup
	tokens := make([]string, 8)
	for i := range tokens {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var err error
			tokens[i], err = EnsureToken(path)
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	for _, tok := range tokens {
		if tok != tokens[0] || len(tok) != 64 {
			t.Fatalf("tokens differ: %v", tokens)
		}
	}
}

func TestBuiltInNamesCantBeUsed(t *testing.T) {
	g, err := LoadGroups(filepath.Join(t.TempDir(), "groups.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"People", "groups", " ALL ", "unsorted", "Unread", "UNREAD"} {
		if err := g.Create(name, ""); err == nil {
			t.Errorf("created a group called %q", name)
		}
		renamed := name
		if err := g.Update("work", &renamed, nil, nil); err == nil {
			t.Errorf("renamed a group to %q", name)
		}
	}
	if err := g.Create("People I Know", ""); err != nil {
		t.Errorf("a longer name should be fine: %v", err)
	}
}

func TestPictureRoutesReachTheBridgeAndNeedTheApp(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	for _, tc := range []struct{ method, path, want string }{
		{"GET", "/api/pictures", "/api/app/pictures"},
		{"GET", "/api/picture?jid=1@g.us", "/api/app/picture"},
		{"POST", "/api/picture/own?jid=1@g.us", "/api/app/picture/own"},
		{"DELETE", "/api/picture/own?jid=1@g.us", "/api/app/picture/own"},
		{"GET", "/api/picture/group-access?jid=1@g.us", "/api/app/picture/group-access"},
		{"GET", "/api/group/members?jid=1@g.us", "/api/app/group/members"},
		{"POST", "/api/picture/group?jid=1@g.us", "/api/app/picture/group"},
	} {
		if resp := f.do(tc.method, tc.path, "", app(), ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without a login: %d", tc.method, tc.path, resp.StatusCode)
		}
		if resp := f.do(tc.method, tc.path, tok, nil, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s from a browser: %d", tc.method, tc.path, resp.StatusCode)
		}
		if resp := f.do(tc.method, tc.path, tok, app(), "x"); resp.StatusCode != 200 {
			t.Errorf("%s %s from the app: %d", tc.method, tc.path, resp.StatusCode)
		}
		f.mu.Lock()
		last := f.bridge[len(f.bridge)-1]
		f.mu.Unlock()
		if last.Path != tc.want || last.Method != tc.method {
			t.Errorf("%s %s reached %s %s", tc.method, tc.path, last.Method, last.Path)
		}
		if last.Header.Get("Cookie") != "" || last.Header.Get("Cf-Access-Jwt-Assertion") != "" {
			t.Errorf("%s %s passed credentials on", tc.method, tc.path)
		}
	}
}

func TestAGroupPhotoChangeIsLoggedWithItsChatAndResult(t *testing.T) {
	f := newFixture(t)
	tok := goodToken(t)
	if resp := f.do("POST", "/api/picture/group?jid=120363000000000001@g.us", tok, app(), "photo"); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if resp := f.do("POST", "/api/picture/group?jid=../../x", tok, app(), "photo"); resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	raw, _ := os.ReadFile(f.cfg.CallsLog)
	log := string(raw)
	if !strings.Contains(log, `"chat":"120363000000000001@g.us"`) || !strings.Contains(log, `"event":"group_photo"`) || !strings.Contains(log, `"result":200`) {
		t.Fatalf("log: %s", log)
	}
	if strings.Contains(log, "../../x") || !strings.Contains(log, "not a group address") {
		t.Fatalf("logged a bad address: %s", log)
	}
	if resp := f.do("POST", "/api/picture/group?jid=120363000000000002@g.us", "", app(), "photo"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no login: %d", resp.StatusCode)
	}
	raw, _ = os.ReadFile(f.cfg.CallsLog)
	if strings.Contains(string(raw), "120363000000000002") {
		t.Fatalf("logged a chat with no login: %s", raw)
	}
}

func TestCreatingAGroupIsLoggedWithNoMemberDetails(t *testing.T) {
	f := newFixture(t)
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/app/events" {
			w.WriteHeader(503)
			return
		}
		if r.URL.Path != "/api/app/group/create" {
			t.Errorf("reached %s", r.URL.Path)
		}
		w.Header().Set("X-Group-Create-Result", "created")
		w.Header().Set("X-Group-Create-Chat", "120363000000000077@g.us")
		w.Header().Set("X-Group-Create-Count", "added=1 not_added=1")
		fmt.Fprint(w, `{"ok":true,"added":["15550000011@s.whatsapp.net"]}`)
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
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	req, _ := http.NewRequest("POST", front.URL+"/api/group/create", strings.NewReader(`{"name":"Made up","members":["15550000011@s.whatsapp.net"]}`))
	req.Header.Set(ClientHeader, "1")
	req.Header.Set(access.HeaderName, goodToken(t))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	// The answer can reach the caller just before the handler logs it.
	log := ""
	for i := 0; i < 100 && !strings.Contains(log, "group_create"); i++ {
		raw, _ := os.ReadFile(cfg.CallsLog)
		log = string(raw)
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{`"event":"group_create"`, `"outcome":"created"`, `"chat":"120363000000000077@g.us"`, `"count":"added=1 not_added=1"`} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %s: %s", want, log)
		}
	}
	if strings.Contains(log, "15550000011") || strings.Contains(log, "Made up") {
		t.Fatalf("logged a member or name: %s", log)
	}
}

func TestGroupActionsAreLoggedWithTheirChatAndNotTheName(t *testing.T) {
	f := newFixture(t)
	reached := map[string]string{}
	var mu sync.Mutex
	bridge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/app/events" {
			w.WriteHeader(503)
			return
		}
		mu.Lock()
		reached[r.Method+" "+r.URL.Path] = r.URL.RawQuery
		mu.Unlock()
		w.Header().Set("X-Group-Action-Result", "made-up-result")
		fmt.Fprint(w, `{"ok":true}`)
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
	front := httptest.NewServer(srv.Handler())
	defer front.Close()
	call := func(method, path, body string) int {
		t.Helper()
		req, _ := http.NewRequest(method, front.URL+path, strings.NewReader(body))
		req.Header.Set(ClientHeader, "1")
		req.Header.Set(access.HeaderName, goodToken(t))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	const group = "120363000000000077@g.us"
	for _, action := range []string{"rename", "leave"} {
		if got := call("POST", "/api/group/"+action+"?jid="+group, `{"name":"Secret made-up name"}`); got != 200 {
			t.Fatalf("%s: %d", action, got)
		}
		mu.Lock()
		query := reached["POST /api/app/group/"+action]
		mu.Unlock()
		if query != "jid="+group {
			t.Fatalf("%s: %q", action, query)
		}
	}
	if got := call("POST", "/api/name", `{"jid":"15550000001@s.whatsapp.net","name":"Made up"}`); got != 200 {
		t.Fatalf("name: %d", got)
	}
	if got := call("DELETE", "/api/name?jid=15550000001@s.whatsapp.net", ""); got != 200 {
		t.Fatalf("name removal: %d", got)
	}
	mu.Lock()
	_, setName := reached["POST /api/app/name"]
	_, removedName := reached["DELETE /api/app/name"]
	mu.Unlock()
	if !setName || !removedName {
		t.Fatalf("%v", reached)
	}
	// Only these. The bridge's other group routes stay out of reach.
	if got := call("GET", "/api/group/rename?jid="+group, ""); got == 200 {
		t.Fatalf("GET reached it: %d", got)
	}
	if got := call("POST", "/api/group/delete?jid="+group, ""); got == 200 {
		t.Fatalf("delete reached it: %d", got)
	}
	log := ""
	for i := 0; i < 100 && !strings.Contains(log, "group_leave"); i++ {
		raw, _ := os.ReadFile(cfg.CallsLog)
		log = string(raw)
		time.Sleep(10 * time.Millisecond)
	}
	for _, want := range []string{`"event":"group_rename"`, `"event":"group_leave"`, `"chat":"` + group + `"`, `"outcome":"made-up-result"`} {
		if !strings.Contains(log, want) {
			t.Errorf("missing %s: %s", want, log)
		}
	}
	if strings.Contains(log, "Secret made-up name") {
		t.Fatalf("logged the name: %s", log)
	}
}
