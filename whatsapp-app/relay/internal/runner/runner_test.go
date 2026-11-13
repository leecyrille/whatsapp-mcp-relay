package runner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests run a fake Claude: this same test binary, re-executed. It behaves like the real CLI's
// stream-json mode, including asking the approval endpoint before an action.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_RUNNER") != "" {
		fakeRunner()
		return
	}
	if os.Getenv("FAKE_CLAUDE") != "" {
		fakeClaude()
		return
	}
	os.Exit(m.Run())
}

func fakeClaude() {
	args := os.Args[1:]
	if len(args) >= 2 && args[0] == "auth" {
		fmt.Printf(`{"loggedIn":%v}`, os.Getenv("FAKE_SIGNED_OUT") == "")
		return
	}
	prompt, _ := io.ReadAll(os.Stdin)
	if rec := os.Getenv("FAKE_ARGS_FILE"); rec != "" {
		raw, _ := json.Marshal(map[string]any{"args": args, "prompt": string(prompt), "cwd": mustCwd(), "env_timeout": os.Getenv("MCP_TOOL_TIMEOUT"), "nested": os.Getenv("CLAUDECODE")})
		os.WriteFile(rec, raw, 0o600)
	}
	mode := os.Getenv("FAKE_CLAUDE")
	if mode == "sort" {
		fmt.Println(os.Getenv("FAKE_SORT_OUTPUT"))
		return
	}
	emit := func(v any) {
		raw, _ := json.Marshal(v)
		fmt.Println(string(raw))
	}
	emit(map[string]any{"type": "system", "subtype": "init", "session_id": "sess-1"})
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "Hel"}}})
	emit(map[string]any{"type": "stream_event", "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "lo"}}})
	emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "Hello"}}}})
	switch mode {
	case "approve":
		answer := askApprover(args)
		emit(map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": "approver said: " + answer}}}})
	case "hang":
		if pidFile := os.Getenv("FAKE_PID_FILE"); pidFile != "" {
			// A grandchild, as a shell command Claude started would be.
			child := exec.Command("ping", "-n", "600", "127.0.0.1")
			child.Start()
			os.WriteFile(pidFile, []byte(fmt.Sprintf("%d %d", os.Getpid(), child.Process.Pid)), 0o600)
		}
		time.Sleep(time.Hour)
	case "crash":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	}
	emit(map[string]any{"type": "result", "subtype": "success", "session_id": "sess-1", "result": "done", "total_cost_usd": 0.01})
}

// fakeRunner is a whole runner process, so a test can kill it the hard way, as mcpctl does.
func fakeRunner() {
	os.Unsetenv("FAKE_RUNNER")
	var cfg Config
	json.Unmarshal([]byte(os.Getenv("FAKE_RUNNER_CFG")), &cfg)
	r, err := New(cfg, "t")
	if err != nil {
		os.Exit(2)
	}
	th, err := r.CreateThread(cfg.ProjectAllowlist[0], "t")
	if err != nil {
		os.Exit(3)
	}
	if err := r.StartMessage(th.ID, "x"); err != nil {
		os.Exit(4)
	}
	time.Sleep(time.Hour)
}

func processAlive(pid int) bool {
	out, err := exec.Command("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid), "/NH", "/FO", "CSV").Output()
	return err == nil && strings.Contains(string(out), fmt.Sprintf(`"%d"`, pid))
}

// Stopping the runner has to stop what it started, or the kill switch would leave Claude running.
func TestKillingTheRunnerEndsItsJobsAndTheirChildren(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	os.MkdirAll(project, 0o700)
	self, _ := os.Executable()
	cfg, _ := json.Marshal(Config{
		ClaudeExe: self, ProjectAllowlist: []string{project}, MaxRunningJobs: 1,
		JobTimeoutMinutes: 5, ApprovalTimeoutMin: 1, ListenPort: 18999, DataDir: filepath.Join(dir, "data"),
		TokenFile: filepath.Join(dir, "t"), ActionsLog: filepath.Join(dir, "a.log"),
	})
	pidFile := filepath.Join(dir, "pids")
	runner := exec.Command(self)
	runner.Env = append(os.Environ(), "FAKE_RUNNER=1", "FAKE_CLAUDE=hang", "FAKE_RUNNER_CFG="+string(cfg), "FAKE_PID_FILE="+pidFile)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	var job, grandchild int
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if n, _ := fmt.Sscan(string(raw), &job, &grandchild); n == 2 {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if job == 0 || grandchild == 0 {
		runner.Process.Kill()
		t.Fatal("the fake job never started")
	}
	if !processAlive(job) || !processAlive(grandchild) {
		t.Fatal("the check for a live process isn't working")
	}
	runner.Process.Kill()
	runner.Wait()
	deadline = time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && (processAlive(job) || processAlive(grandchild)) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(job) {
		t.Error("the Claude job outlived the runner")
	}
	if processAlive(grandchild) {
		t.Error("a command the job started outlived the runner")
	}
}

func mustCwd() string {
	d, _ := os.Getwd()
	return d
}

func askApprover(args []string) string {
	var cfg struct {
		McpServers map[string]struct {
			URL string `json:"url"`
		} `json:"mcpServers"`
	}
	for i, a := range args {
		if a == "--mcp-config" {
			raw, _ := os.ReadFile(args[i+1])
			json.Unmarshal(raw, &cfg)
		}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{
		"name": "ask", "arguments": map[string]any{"tool_name": "Bash", "input": map[string]any{"command": "echo hi"}},
	}})
	req, _ := http.NewRequest("POST", cfg.McpServers["approver"].URL, bytes.NewReader(body))
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "error: " + err.Error()
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}

type rig struct {
	t       *testing.T
	r       *Runner
	base    string
	project string
	token   string
	dir     string
}

func newRig(t *testing.T, mode string, tweak func(*Config)) *rig {
	t.Helper()
	t.Setenv("FAKE_CLAUDE", mode)
	dir := t.TempDir()
	project := filepath.Join(dir, "project")
	os.MkdirAll(project, 0o700)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	self, _ := os.Executable()
	cfg := Config{
		ClaudeExe: self, DeniedTools: []string{"Read(//c/mcp/**)"},
		ProjectAllowlist: []string{project}, MaxRunningJobs: 2, JobTimeoutMinutes: 1, ApprovalTimeoutMin: 1,
		ListenPort: port, DataDir: filepath.Join(dir, "data"), TokenFile: filepath.Join(dir, "t"), ActionsLog: filepath.Join(dir, "actions.log"),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	r, err := New(cfg, "relay-token")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: r.Handler()}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return &rig{t: t, r: r, base: fmt.Sprintf("http://127.0.0.1:%d", port), project: project, token: "relay-token", dir: dir}
}

func (g *rig) call(method, path string, body any, into any) int {
	g.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, g.base+path, reader)
	req.Header.Set(TokenHeader, g.token)
	req.Header.Set(EmailHeader, "user@example.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		json.NewDecoder(resp.Body).Decode(into)
	}
	return resp.StatusCode
}

func (g *rig) thread() string {
	g.t.Helper()
	var th Thread
	if code := g.call("POST", "/threads", map[string]string{"project": g.project, "title": "t"}, &th); code != 200 {
		g.t.Fatalf("create thread: %d", code)
	}
	return th.ID
}

func (g *rig) waitItems(id string, pred func(*Thread) bool) *Thread {
	g.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var th Thread
		g.call("GET", "/threads/"+id, nil, &th)
		if pred(&th) {
			return &th
		}
		time.Sleep(25 * time.Millisecond)
	}
	var th Thread
	g.call("GET", "/threads/"+id, nil, &th)
	g.t.Fatalf("condition never met; thread: %+v", th)
	return nil
}

func hasKind(th *Thread, kind string) *Item {
	for i := range th.Items {
		if th.Items[i].Kind == kind {
			return &th.Items[i]
		}
	}
	return nil
}

func TestAllRoutesNeedTheRelayToken(t *testing.T) {
	g := newRig(t, "plain", nil)
	for _, p := range [][2]string{{"GET", "/threads"}, {"POST", "/threads"}, {"POST", "/approvals/x"}, {"GET", "/events"}, {"GET", "/status"}, {"POST", "/sort"}} {
		for _, token := range []string{"", "wrong"} {
			req, _ := http.NewRequest(p[0], g.base+p[1], strings.NewReader("{}"))
			if token != "" {
				req.Header.Set(TokenHeader, token)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 401 {
				t.Errorf("%v with token %q: %d", p, token, resp.StatusCode)
			}
		}
	}
}

func TestBrowserOriginRefused(t *testing.T) {
	g := newRig(t, "plain", nil)
	req, _ := http.NewRequest("POST", g.base+"/approvals/x", strings.NewReader(`{"approve":true}`))
	req.Header.Set(TokenHeader, g.token)
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 403 {
		t.Fatal(resp.StatusCode)
	}
}

func TestCommandLineIsTheSafeOne(t *testing.T) {
	g := newRig(t, "plain", nil)
	rec := filepath.Join(g.dir, "args.json")
	t.Setenv("FAKE_ARGS_FILE", rec)
	t.Setenv("CLAUDECODE", "1")
	id := g.thread()
	if code := g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "hello there"}, nil); code != 202 {
		t.Fatal(code)
	}
	g.waitItems(id, func(th *Thread) bool { return !th.Running && hasKind(th, "result") != nil })
	var got struct {
		Args       []string `json:"args"`
		Prompt     string   `json:"prompt"`
		Cwd        string   `json:"cwd"`
		EnvTimeout string   `json:"env_timeout"`
		Nested     string   `json:"nested"`
	}
	raw, _ := os.ReadFile(rec)
	json.Unmarshal(raw, &got)
	joined := strings.Join(got.Args, " ")
	for _, must := range []string{"--permission-mode default", "--disallowedTools Agent,Task,Read(//c/mcp/**)",
		"--permission-prompt-tool mcp__approver__ask", "--strict-mcp-config", "--setting-sources  --settings "} {
		if !strings.Contains(joined, must) {
			t.Errorf("missing %q in: %s", must, joined)
		}
	}
	for _, never := range []string{"bypass", "dangerously", "skip-permissions", "acceptEdits", "--allowedTools"} {
		if strings.Contains(joined, never) {
			t.Errorf("found %q in: %s", never, joined)
		}
	}
	if got.Prompt != "hello there" {
		t.Errorf("prompt: %q", got.Prompt)
	}
	if !strings.EqualFold(got.Cwd, g.project) {
		t.Errorf("cwd: %q, want %q", got.Cwd, g.project)
	}
	if got.Nested != "" {
		t.Error("nested-Claude marker leaked to the child")
	}
	if got.EnvTimeout == "" {
		t.Error("no tool timeout set; a long approval could be cut off")
	}
}

func TestStreamBecomesThreadItemsAndResumes(t *testing.T) {
	g := newRig(t, "plain", nil)
	rec := filepath.Join(g.dir, "args.json")
	t.Setenv("FAKE_ARGS_FILE", rec)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "one"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { return !th.Running && hasKind(th, "result") != nil })
	assistant := hasKind(th, "assistant")
	if assistant == nil || assistant.Text != "Hello" {
		t.Fatalf("%+v", th.Items)
	}
	if th.SessionID != "sess-1" {
		t.Fatalf("session: %q", th.SessionID)
	}
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "two"}, nil)
	g.waitItems(id, func(th *Thread) bool {
		n := 0
		for _, it := range th.Items {
			if it.Kind == "result" {
				n++
			}
		}
		return !th.Running && n == 2
	})
	raw, _ := os.ReadFile(rec)
	if !strings.Contains(string(raw), `"--resume","sess-1"`) {
		t.Fatalf("second message didn't resume: %s", raw)
	}
}

func approvalItem(th *Thread) *Item { return hasKind(th, "approval") }

func TestApprovalAllowReturnsToClaude(t *testing.T) {
	g := newRig(t, "approve", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "do it"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { a := approvalItem(th); return a != nil && a.State == "pending" })
	a := approvalItem(th)
	if a.Tool != "Bash" || !strings.Contains(string(a.Input), "echo hi") {
		t.Fatalf("The user must see what they're approving: %+v", a)
	}
	if code := g.call("POST", "/approvals/"+a.ID, map[string]any{"approve": true}, nil); code != 200 {
		t.Fatal(code)
	}
	th = g.waitItems(id, func(th *Thread) bool { return !th.Running && hasKind(th, "result") != nil })
	if a := approvalItem(th); a.State != "approved" || a.By != "user@example.com" {
		t.Fatalf("%+v", a)
	}
	found := false
	for _, it := range th.Items {
		if it.Kind == "assistant" && strings.Contains(it.Text, `behavior`) && strings.Contains(it.Text, `allow`) && strings.Contains(it.Text, `echo hi`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("Claude never got the allow: %+v", th.Items)
	}
	log, _ := os.ReadFile(g.r.cfg.ActionsLog)
	if !strings.Contains(string(log), "approval_approved") || !strings.Contains(string(log), "user@example.com") {
		t.Fatalf("audit log: %s", log)
	}
}

func TestApprovalDenyReturnsDeny(t *testing.T) {
	g := newRig(t, "approve", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "do it"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { a := approvalItem(th); return a != nil && a.State == "pending" })
	g.call("POST", "/approvals/"+approvalItem(th).ID, map[string]any{"approve": false}, nil)
	th = g.waitItems(id, func(th *Thread) bool { return !th.Running && hasKind(th, "result") != nil })
	if approvalItem(th).State != "denied" {
		t.Fatalf("%+v", approvalItem(th))
	}
	for _, it := range th.Items {
		if it.Kind == "assistant" && strings.Contains(it.Text, `\"allow\"`) {
			t.Fatalf("a deny was reported as allow: %s", it.Text)
		}
	}
}

func TestAnswerOnlyCountsOnce(t *testing.T) {
	g := newRig(t, "approve", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "do it"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { a := approvalItem(th); return a != nil && a.State == "pending" })
	aid := approvalItem(th).ID
	g.call("POST", "/approvals/"+aid, map[string]any{"approve": false}, nil)
	if code := g.call("POST", "/approvals/"+aid, map[string]any{"approve": true}, nil); code != 409 {
		t.Fatalf("a second answer must be refused: %d", code)
	}
	th = g.waitItems(id, func(th *Thread) bool { return !th.Running })
	if approvalItem(th).State != "denied" {
		t.Fatal("the later approve overrode the deny")
	}
}

func TestApprovalMustSayYesOrNo(t *testing.T) {
	g := newRig(t, "approve", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "do it"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { a := approvalItem(th); return a != nil && a.State == "pending" })
	aid := approvalItem(th).ID
	if code := g.call("POST", "/approvals/"+aid, map[string]any{}, nil); code != 400 {
		t.Fatalf("a missing answer: %d", code)
	}
	if err := g.r.Decide(aid, true, ""); err == nil {
		t.Fatal("an approval with no verified user was accepted")
	}
	g.call("POST", "/approvals/"+aid, map[string]any{"approve": false}, nil)
	g.waitItems(id, func(th *Thread) bool { return !th.Running })
}

func TestNoAnswerMeansDeny(t *testing.T) {
	g := newRig(t, "approve", func(c *Config) { c.ApprovalTimeoutMin = 1 })
	id := g.thread()
	g.r.cfg.ApprovalTimeoutMin = 0 // Zero minutes: expires at once.
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "do it"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { a := approvalItem(th); return a != nil && a.State == "expired" && !th.Running })
	for _, it := range th.Items {
		if it.Kind == "assistant" && strings.Contains(it.Text, `\"allow\"`) {
			t.Fatal("timeout was treated as allow")
		}
	}
}

func TestApprovalEndpointNeedsTheJobsOwnToken(t *testing.T) {
	g := newRig(t, "plain", nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask","arguments":{"tool_name":"Bash","input":{}}}}`
	resp, err := http.Post(g.base+"/mcp/not-a-real-token", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatal(resp.StatusCode)
	}
	var th Thread
	g.call("GET", "/threads", nil, &th)
	if len(g.r.pending) != 0 {
		t.Fatal("a fake request created an approval")
	}
}

func TestMCPHandshake(t *testing.T) {
	g := newRig(t, "hang", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "x"}, nil)
	g.waitItems(id, func(th *Thread) bool { return th.Running })
	g.r.mu.Lock()
	token := g.r.jobs[id].mcpToken
	g.r.mu.Unlock()
	post := func(body string) (int, string) {
		resp, err := http.Post(g.base+"/mcp/"+token, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(raw)
	}
	if code, out := post(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`); code != 200 || !strings.Contains(out, "2025-06-18") || !strings.Contains(out, `"tools"`) {
		t.Fatalf("%d %s", code, out)
	}
	if code, _ := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != 202 {
		t.Fatal(code)
	}
	if code, out := post(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`); code != 200 || !strings.Contains(out, `"ask"`) {
		t.Fatalf("%d %s", code, out)
	}
	resp, _ := http.Get(g.base + "/mcp/" + token)
	resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatal(resp.StatusCode)
	}
	g.call("POST", "/threads/"+id+"/cancel", nil, nil)
	g.waitItems(id, func(th *Thread) bool { return !th.Running })
}

func TestProjectAllowlist(t *testing.T) {
	g := newRig(t, "plain", nil)
	sub := filepath.Join(g.project, "sub")
	os.MkdirAll(sub, 0o700)
	if _, err := g.r.AllowedProject(sub); err != nil {
		t.Errorf("subfolder refused: %v", err)
	}
	if _, err := g.r.AllowedProject(strings.ToUpper(g.project)); err != nil {
		t.Errorf("Windows paths are case-insensitive: %v", err)
	}
	outside := filepath.Join(g.dir, "project-evil")
	os.MkdirAll(outside, 0o700)
	for _, bad := range []string{outside, g.dir, filepath.Join(g.project, ".."), "", filepath.Join(g.project, "missing"), `C:\Windows`} {
		if _, err := g.r.AllowedProject(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if code := g.call("POST", "/threads", map[string]string{"project": outside}, nil); code != 400 {
		t.Errorf("thread outside the allowlist: %d", code)
	}
}

func TestProjectRemovedFromAllowlistCantRun(t *testing.T) {
	g := newRig(t, "plain", nil)
	id := g.thread()
	g.r.cfg.ProjectAllowlist = []string{filepath.Join(g.dir, "other")}
	if code := g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "x"}, nil); code != 400 {
		t.Fatalf("got %d", code)
	}
}

func TestConfigRefusesWritingToolsWithoutApproval(t *testing.T) {
	base := Config{ClaudeExe: "x", DataDir: "d", TokenFile: "t", ActionsLog: "a", ListenPort: 18705, ProjectAllowlist: []string{"p"}}
	for _, tool := range []string{"Read", "Glob", "Grep", "Bash", "Edit", "Write", "WebFetch", "Bash(*)", "mcp__x__y", "*"} {
		c := base
		c.AllowedTools = []string{tool}
		if c.Validate() == nil {
			t.Errorf("%s was allowed without approval", tool)
		}
	}
	if err := base.Validate(); err != nil {
		t.Errorf("the empty list must be accepted: %v", err)
	}
	if !strings.Contains(jobSettings, `"Bash"`) || !strings.Contains(jobSettings, `"ask"`) {
		t.Error("commands must always ask")
	}
	c := base
	c.ProjectAllowlist = nil
	if c.Validate() == nil {
		t.Error("empty allowlist accepted")
	}
}

func TestOnePerThreadAndGlobalLimit(t *testing.T) {
	g := newRig(t, "hang", func(c *Config) { c.MaxRunningJobs = 1 })
	a, b := g.thread(), g.thread()
	if code := g.call("POST", "/threads/"+a+"/messages", map[string]string{"text": "x"}, nil); code != 202 {
		t.Fatal(code)
	}
	if code := g.call("POST", "/threads/"+a+"/messages", map[string]string{"text": "y"}, nil); code != 400 {
		t.Errorf("second job on one thread: %d", code)
	}
	if code := g.call("POST", "/threads/"+b+"/messages", map[string]string{"text": "z"}, nil); code != 400 {
		t.Errorf("over the limit: %d", code)
	}
	g.call("POST", "/threads/"+a+"/cancel", nil, nil)
	g.waitItems(a, func(th *Thread) bool { return !th.Running })
}

func TestCancelStopsTheProcess(t *testing.T) {
	g := newRig(t, "hang", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "x"}, nil)
	g.waitItems(id, func(th *Thread) bool { return th.Running && hasKind(th, "assistant") != nil })
	if code := g.call("POST", "/threads/"+id+"/cancel", nil, nil); code != 200 {
		t.Fatal(code)
	}
	th := g.waitItems(id, func(th *Thread) bool { return !th.Running })
	e := hasKind(th, "error")
	if e == nil || !strings.Contains(e.Text, "Stopped") {
		t.Fatalf("%+v", th.Items)
	}
}

func TestCrashIsReported(t *testing.T) {
	g := newRig(t, "crash", nil)
	id := g.thread()
	g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "x"}, nil)
	th := g.waitItems(id, func(th *Thread) bool { return !th.Running && hasKind(th, "error") != nil })
	if !strings.Contains(hasKind(th, "error").Text, "boom") {
		t.Fatalf("%+v", th.Items)
	}
}

func TestSignedOutIsExplained(t *testing.T) {
	g := newRig(t, "plain", nil)
	t.Setenv("FAKE_SIGNED_OUT", "1")
	id := g.thread()
	var out map[string]any
	if code := g.call("POST", "/threads/"+id+"/messages", map[string]string{"text": "x"}, &out); code != 409 {
		t.Fatal(code)
	}
	if !strings.Contains(fmt.Sprint(out["error"]), "claude auth login") {
		t.Fatalf("%v", out)
	}
}

func TestRestartMarksInterruptedThreads(t *testing.T) {
	g := newRig(t, "plain", nil)
	th := &Thread{ID: newID(), Title: "x", Project: g.project, Running: true, Updated: now(),
		Items: []Item{{ID: newID(), Kind: "approval", State: "pending", Tool: "Bash", TS: now()}}}
	g.r.saveThread(th)
	again, err := New(g.r.cfg, "relay-token")
	if err != nil {
		t.Fatal(err)
	}
	loaded := again.threads[th.ID]
	if loaded.Running || loaded.Items[0].State != "expired" || hasKind(loaded, "error") == nil {
		t.Fatalf("%+v", loaded)
	}
}

func TestSortHasNoToolsAndChecksTheAnswer(t *testing.T) {
	items := []SortItem{{JID: "1@s.whatsapp.net", Name: "Boss"}, {JID: "2@s.whatsapp.net", Name: "Mum"}}
	groups := []SortGroup{{ID: "work", Name: "Work"}, {ID: "personal", Name: "Personal"}}
	output := `{"result":"{\"assignments\":[` +
		`{\"jid\":\"1@s.whatsapp.net\",\"group\":\"work\",\"confidence\":0.9,\"reason\":\"boss\"},` +
		`{\"jid\":\"1@s.whatsapp.net\",\"group\":\"personal\",\"confidence\":0.1,\"reason\":\"duplicate\"},` +
		`{\"jid\":\"invented@s.whatsapp.net\",\"group\":\"work\",\"confidence\":1,\"reason\":\"not in the list\"},` +
		`{\"jid\":\"2@s.whatsapp.net\",\"group\":\"hacked\",\"confidence\":1,\"reason\":\"not a group\"}` +
		`]}","total_cost_usd":0.02}`
	g := newRig(t, "sort", nil)
	rec := filepath.Join(g.dir, "args.json")
	t.Setenv("FAKE_ARGS_FILE", rec)
	t.Setenv("FAKE_SORT_OUTPUT", output)
	var job SortJob
	if code := g.call("POST", "/sort", map[string]any{"items": items, "groups": groups}, &job); code != 202 {
		t.Fatal(code)
	}
	deadline := time.Now().Add(10 * time.Second)
	for job.State != "done" && job.State != "failed" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
		g.call("GET", "/sort/"+job.ID, nil, &job)
	}
	if job.State != "done" || len(job.Proposals) != 1 || job.Proposals[0].JID != "1@s.whatsapp.net" || job.Proposals[0].Group != "work" {
		t.Fatalf("invented answers must be dropped: %+v", job)
	}
	var got struct {
		Args   []string `json:"args"`
		Prompt string   `json:"prompt"`
	}
	raw, _ := os.ReadFile(rec)
	json.Unmarshal(raw, &got)
	joined := strings.Join(got.Args, " ")
	if !strings.Contains(joined, "--tools ") || strings.Contains(joined, "--allowedTools") || strings.Contains(joined, "--mcp-config") || strings.Contains(joined, "--resume") {
		t.Fatalf("sort must have no tools: %v", got.Args)
	}
	idx := -1
	for i, a := range got.Args {
		if a == "--tools" {
			idx = i
		}
	}
	if idx < 0 || got.Args[idx+1] != "" {
		t.Fatalf("--tools must be empty: %v", got.Args)
	}
	if !strings.Contains(got.Prompt, "<items>") || !strings.Contains(got.Prompt, "DATA") {
		t.Fatalf("prompt must mark names as data: %s", got.Prompt)
	}
}

func TestSortPromptClipsAndFallsBack(t *testing.T) {
	long := strings.Repeat("x", 5000)
	prompt, err := buildSortPrompt([]SortItem{{JID: "1@g.us", Name: long, Snippets: []string{long, long, long, long, long, long, long}}}, []SortGroup{{ID: "w", Name: "W"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(prompt) > 3000 {
		t.Fatalf("not clipped: %d", len(prompt))
	}
	var many []SortItem
	for i := 0; i < 3000; i++ {
		many = append(many, SortItem{JID: fmt.Sprintf("%d@g.us", i), Name: "n", Snippets: []string{strings.Repeat("y", 160)}})
	}
	prompt, err = buildSortPrompt(many, []SortGroup{{ID: "w", Name: "W"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(prompt, "yyyy") || len(prompt) > maxSortBytes+2000 {
		t.Fatalf("snippets should be dropped when too big: %d", len(prompt))
	}
}

func TestParseSortRefusesGarbage(t *testing.T) {
	groups := []SortGroup{{ID: "w", Name: "W"}}
	items := []SortItem{{JID: "1@g.us"}}
	for _, bad := range []string{"", "no json here", "{not json}", "}{"} {
		if _, err := parseSortResult(bad, items, groups); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	out, err := parseSortResult("Here you go:\n```json\n{\"assignments\":[{\"jid\":\"1@g.us\",\"group\":\"w\",\"confidence\":7,\"reason\":\"a\"}]}\n```", items, groups)
	if err != nil || len(out) != 1 || out[0].Confidence != 1 {
		t.Fatalf("%+v %v", out, err)
	}
}
