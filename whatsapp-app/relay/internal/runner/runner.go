// Package runner runs Claude jobs for the app, in the user's logged-in Windows session so it uses their
// Claude login. It follows C:\mcp\claude\core.py: a project allowlist, read-only tools by default,
// and no way to skip permissions.
//
// Anything outside the read-only tools goes through the user: Claude's permission prompt is routed to a
// small MCP endpoint here, which blocks until the user taps Approve or Deny in the app. No answer within
// the time limit counts as Deny.
package runner

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// TokenHeader authenticates the relay. EmailHeader says who the relay verified.
	TokenHeader       = "X-Runner-Token"
	EmailHeader       = "X-Verified-Email"
	maxPrompt         = 200_000
	actionLogMaxBytes = 10 << 20
)

// Config is runner.json. There is deliberately no setting for the permission mode.
type Config struct {
	ClaudeExe          string   `json:"claude_exe"`
	AllowedTools       []string `json:"allowed_tools"`
	DeniedTools        []string `json:"denied_tools"`
	ProjectAllowlist   []string `json:"project_allowlist"`
	MaxRunningJobs     int      `json:"max_running_jobs"`
	JobTimeoutMinutes  int      `json:"job_timeout_minutes"`
	ApprovalTimeoutMin int      `json:"approval_timeout_minutes"`
	Model              string   `json:"model"`
	ListenPort         int      `json:"listen_port"`
	DataDir            string   `json:"data_dir"`
	TokenFile          string   `json:"runner_token_file"`
	ActionsLog         string   `json:"actions_log"`
}

// Validate refuses unsafe or incomplete config, and any tool list that would remove the approval
// step. Only tools that can't change anything may run without asking.
func (c *Config) Validate() error {
	if c.ClaudeExe == "" || c.DataDir == "" || c.TokenFile == "" || c.ActionsLog == "" {
		return errors.New("claude_exe, data_dir, runner_token_file and actions_log are required")
	}
	if c.ListenPort < 1024 || c.ListenPort > 65535 {
		return errors.New("listen_port out of range")
	}
	if len(c.ProjectAllowlist) == 0 {
		return errors.New("project_allowlist is empty")
	}
	if len(c.AllowedTools) > 0 {
		return errors.New("allowed_tools must stay empty: an allow rule covers the whole disk, not just the project, and everything else goes through approval")
	}
	if c.MaxRunningJobs < 1 {
		c.MaxRunningJobs = 2
	}
	if c.JobTimeoutMinutes < 1 {
		c.JobTimeoutMinutes = 30
	}
	if c.ApprovalTimeoutMin < 1 {
		c.ApprovalTimeoutMin = 10
	}
	return nil
}

// Item is one entry in a thread.
type Item struct {
	ID    string          `json:"id"`
	Kind  string          `json:"kind"` // user, assistant, tool, approval, error, result
	Text  string          `json:"text,omitempty"`
	Tool  string          `json:"tool,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	State string          `json:"state,omitempty"` // approval: pending, approved, denied, expired
	By    string          `json:"by,omitempty"`
	TS    int64           `json:"ts"`
	Cost  float64         `json:"cost,omitempty"`
}

// Thread is one conversation with Claude.
type Thread struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Project   string `json:"project"`
	SessionID string `json:"session_id,omitempty"`
	Created   int64  `json:"created"`
	Updated   int64  `json:"updated"`
	Running   bool   `json:"running"`
	Items     []Item `json:"items,omitempty"`
}

// Runner holds all state. Threads are saved as one JSON file each.
type Runner struct {
	cfg     Config
	token   string
	root    string
	mu      sync.Mutex
	threads map[string]*Thread
	jobs    map[string]*job // by thread
	pending map[string]*approval
	sorts   map[string]*SortJob
	subs    map[chan []byte]struct{}
	authMu  sync.Mutex
	auth    authState
	killer  *procGroup
	logMu   sync.Mutex
}

type job struct {
	threadID string
	mcpToken string
	cancel   context.CancelFunc
	current  *Item
}

type approval struct {
	id       string
	threadID string
	decision chan decision
}

type decision struct {
	allow bool
	by    string
}

type authState struct {
	at       time.Time
	loggedIn bool
}

// New loads saved threads. A thread marked running from before a restart gets an error item, since
// its process is gone.
func New(cfg Config, token string) (*Runner, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	r := &Runner{
		cfg: cfg, token: token, root: filepath.Join(cfg.DataDir, "claude"),
		threads: map[string]*Thread{}, jobs: map[string]*job{}, pending: map[string]*approval{},
		sorts: map[string]*SortJob{}, subs: map[chan []byte]struct{}{}, killer: newProcGroup(),
	}
	if err := os.MkdirAll(filepath.Join(r.root, "threads"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(r.root, "jobs"), 0o700); err != nil {
		return nil, err
	}
	files, err := filepath.Glob(filepath.Join(r.root, "threads", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var t Thread
		if json.Unmarshal(raw, &t) != nil || t.ID == "" {
			continue
		}
		if t.Running {
			t.Running = false
			for i := range t.Items {
				if t.Items[i].Kind == "approval" && t.Items[i].State == "pending" {
					t.Items[i].State = "expired"
				}
			}
			t.Items = append(t.Items, Item{ID: newID(), Kind: "error", Text: "The PC or runner restarted while this was running, so it stopped.", TS: now()})
			r.saveThread(&t)
		}
		r.threads[t.ID] = &t
	}
	return r, nil
}

func now() int64 { return time.Now().UnixMilli() }

func newID() string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

var idRe = regexp.MustCompile(`^[0-9a-f]{24}$`)

func (r *Runner) saveThread(t *Thread) {
	raw, err := json.Marshal(t)
	if err != nil {
		return
	}
	path := filepath.Join(r.root, "threads", t.ID+".json")
	if err := os.WriteFile(path+".tmp", raw, 0o600); err == nil {
		_ = os.Rename(path+".tmp", path)
	}
}

func (r *Runner) publish(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- data:
		default:
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func (r *Runner) publishItem(threadID string, item Item) {
	r.publish(map[string]any{"type": "claude", "thread": threadID, "kind": "item", "item": item})
}

// actionLog records every approval decision and every job start. It is the audit trail for what
// Claude was allowed to do on this PC.
func (r *Runner) actionLog(fields map[string]any) {
	fields["t"] = time.Now().Format(time.RFC3339)
	line, _ := json.Marshal(fields)
	r.logMu.Lock()
	defer r.logMu.Unlock()
	_ = os.MkdirAll(filepath.Dir(r.cfg.ActionsLog), 0o700)
	if info, err := os.Stat(r.cfg.ActionsLog); err == nil && info.Size() > actionLogMaxBytes {
		_ = os.Rename(r.cfg.ActionsLog, r.cfg.ActionsLog+".1")
	}
	f, err := os.OpenFile(r.cfg.ActionsLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		log.Printf("can't write the actions log: %v", err)
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}

// AllowedProject resolves a folder and requires it to be on the allowlist, following symlinks so a
// link inside an allowed folder can't lead outside it.
func (r *Runner) AllowedProject(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("a project folder is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("that folder doesn't exist")
	}
	info, err := os.Stat(real)
	if err != nil || !info.IsDir() {
		return "", errors.New("that isn't a folder")
	}
	for _, entry := range r.cfg.ProjectAllowlist {
		root, err := filepath.EvalSymlinks(entry)
		if err != nil {
			continue
		}
		if real == root || strings.HasPrefix(strings.ToLower(real)+`\`, strings.ToLower(root)+`\`) {
			return real, nil
		}
	}
	return "", errors.New("that folder isn't on the project allowlist")
}

// alwaysDenied can't be turned on by config. Subagents are off because their tool calls might not
// come through the approval step.
var alwaysDenied = []string{"Agent", "Task"}

// jobSettings is the policy for every job. Commands always ask: Claude Code treats some shell
// commands as read-only and runs them unprompted, and those can read anything on the disk.
const jobSettings = `{"permissions":{"ask":["Bash","PowerShell"]},"disableAllHooks":true}`

// buildArgs makes the Claude command line. The permission mode is fixed. There is no path to
// bypassPermissions or --dangerously-skip-permissions in this program.
//
// No tools are allowed outright, so Claude can read inside the job's folder without asking and has
// to ask for anything else. No settings files load, so a file in a project, or a settings change
// from an earlier job, can't add allow rules or hooks to this one.
func (r *Runner) buildArgs(mcpConfig, settings, sessionID string) []string {
	denied := append(append([]string{}, alwaysDenied...), r.cfg.DeniedTools...)
	args := []string{
		"-p", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
		"--permission-mode", "default",
		"--setting-sources", "", "--settings", settings,
		"--strict-mcp-config", "--mcp-config", mcpConfig,
		"--permission-prompt-tool", "mcp__approver__ask",
		"--disallowedTools", strings.Join(denied, ","),
	}
	if r.cfg.Model != "" {
		args = append(args, "--model", r.cfg.Model)
	}
	if sessionID != "" {
		args = append(args, "--resume", sessionID)
	}
	return args
}

// childEnv keeps the environment, minus markers that would make Claude think it's nested.
func childEnv(toolTimeoutMs int) []string {
	var env []string
	for _, kv := range os.Environ() {
		key, _, _ := strings.Cut(kv, "=")
		upper := strings.ToUpper(key)
		if upper == "CLAUDECODE" || strings.HasPrefix(upper, "CLAUDE_CODE_") || upper == "MCP_TOOL_TIMEOUT" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, fmt.Sprintf("MCP_TOOL_TIMEOUT=%d", toolTimeoutMs))
}

// StartMessage adds a user message to a thread and starts Claude on it.
func (r *Runner) StartMessage(threadID, text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("the message is empty")
	}
	if len(text) > maxPrompt {
		return errors.New("the message is too long")
	}
	r.mu.Lock()
	t := r.threads[threadID]
	if t == nil {
		r.mu.Unlock()
		return errors.New("no such thread")
	}
	if r.jobs[threadID] != nil {
		r.mu.Unlock()
		return errors.New("this thread is already running")
	}
	if len(r.jobs) >= r.cfg.MaxRunningJobs {
		r.mu.Unlock()
		return fmt.Errorf("%d jobs are already running; try again when one finishes", len(r.jobs))
	}
	project, err := r.AllowedProject(t.Project)
	if err != nil {
		r.mu.Unlock()
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.cfg.JobTimeoutMinutes)*time.Minute)
	j := &job{threadID: threadID, mcpToken: newID() + newID(), cancel: cancel}
	r.jobs[threadID] = j
	item := Item{ID: newID(), Kind: "user", Text: text, TS: now()}
	t.Items = append(t.Items, item)
	t.Running = true
	t.Updated = now()
	sessionID := t.SessionID
	r.saveThread(t)
	r.mu.Unlock()

	r.publishItem(threadID, item)
	r.publish(map[string]any{"type": "claude", "thread": threadID, "kind": "state", "running": true})
	r.actionLog(map[string]any{"action": "job_start", "thread": threadID, "project": project, "resume": sessionID != ""})
	go r.runJob(ctx, cancel, j, project, sessionID, text)
	return nil
}

func (r *Runner) addItem(threadID string, item Item) Item {
	r.mu.Lock()
	if t := r.threads[threadID]; t != nil {
		t.Items = append(t.Items, item)
		t.Updated = now()
		r.saveThread(t)
	}
	r.mu.Unlock()
	r.publishItem(threadID, item)
	return item
}

func (r *Runner) runJob(ctx context.Context, cancel context.CancelFunc, j *job, project, sessionID, prompt string) {
	defer cancel()
	exitNote := ""
	defer func() {
		r.mu.Lock()
		delete(r.jobs, j.threadID)
		if t := r.threads[j.threadID]; t != nil {
			t.Running = false
			t.Updated = now()
			r.saveThread(t)
		}
		r.mu.Unlock()
		if exitNote != "" {
			r.addItem(j.threadID, Item{ID: newID(), Kind: "error", Text: exitNote, TS: now()})
		}
		r.publish(map[string]any{"type": "claude", "thread": j.threadID, "kind": "state", "running": false})
	}()

	folder := filepath.Join(r.root, "jobs", newID())
	if err := os.MkdirAll(folder, 0o700); err != nil {
		exitNote = "Couldn't prepare the job folder."
		return
	}
	defer os.RemoveAll(folder)
	mcp, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"approver": map[string]any{
		"type": "http", "url": fmt.Sprintf("http://127.0.0.1:%d/mcp/%s", r.cfg.ListenPort, j.mcpToken),
	}}})
	mcpPath := filepath.Join(folder, "mcp.json")
	if err := os.WriteFile(mcpPath, mcp, 0o600); err != nil {
		exitNote = "Couldn't prepare the job."
		return
	}
	settingsPath := filepath.Join(folder, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(jobSettings), 0o600); err != nil {
		exitNote = "Couldn't prepare the job."
		return
	}
	cmd := exec.Command(r.cfg.ClaudeExe, r.buildArgs(mcpPath, settingsPath, sessionID)...)
	cmd.Dir = project
	cmd.Env = childEnv(int(time.Duration(r.cfg.ApprovalTimeoutMin+1) * time.Minute / time.Millisecond))
	cmd.Stdin = strings.NewReader(prompt)
	hideWindow(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		exitNote = "Couldn't start Claude."
		return
	}
	var stderr tailBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		exitNote = "Couldn't start Claude: " + err.Error()
		return
	}
	r.killer.add(cmd.Process)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			killTree(cmd.Process.Pid)
		case <-stopped:
		}
	}()
	sawResult := r.readStream(j, stdout)
	waitErr := cmd.Wait()
	close(stopped)
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		exitNote = "Stopped: it ran past the time limit."
	case errors.Is(ctx.Err(), context.Canceled):
		exitNote = "Stopped."
	case !sawResult && waitErr != nil:
		exitNote = "Claude stopped unexpectedly. " + clip(stderr.String(), 400)
	case !sawResult:
		exitNote = "Claude finished without giving a result. " + clip(stderr.String(), 400)
	}
}

// readStream turns Claude's stream-json output into thread items. Returns whether a result arrived.
func (r *Runner) readStream(j *job, stdout io.Reader) bool {
	sawResult := false
	reader := bufio.NewReaderSize(stdout, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if r.handleLine(j, line) {
				sawResult = true
			}
		}
		if err != nil {
			return sawResult
		}
	}
}

type streamEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	Event     struct {
		Type  string `json:"type"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	} `json:"event"`
	Message struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	} `json:"message"`
	Result  string  `json:"result"`
	IsError bool    `json:"is_error"`
	Cost    float64 `json:"total_cost_usd"`
}

func (r *Runner) setSession(threadID, sessionID string) {
	if sessionID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if t := r.threads[threadID]; t != nil && t.SessionID != sessionID {
		t.SessionID = sessionID
		r.saveThread(t)
	}
}

func (r *Runner) handleLine(j *job, line []byte) bool {
	var ev streamEvent
	if json.Unmarshal(line, &ev) != nil {
		return false
	}
	switch ev.Type {
	case "system":
		r.setSession(j.threadID, ev.SessionID)
	case "stream_event":
		if ev.Event.Type == "content_block_delta" && ev.Event.Delta.Type == "text_delta" {
			if j.current == nil {
				j.current = &Item{ID: newID(), Kind: "assistant", TS: now()}
			}
			j.current.Text += ev.Event.Delta.Text
			r.publish(map[string]any{"type": "claude", "thread": j.threadID, "kind": "delta", "item_id": j.current.ID, "text": ev.Event.Delta.Text})
		}
	case "assistant":
		for _, block := range ev.Message.Content {
			switch block.Type {
			case "text":
				if strings.TrimSpace(block.Text) == "" {
					continue
				}
				item := Item{ID: newID(), Kind: "assistant", TS: now()}
				if j.current != nil {
					item = *j.current
				}
				item.Text = block.Text
				j.current = nil
				r.addItem(j.threadID, item)
			case "tool_use":
				r.addItem(j.threadID, Item{ID: newID(), Kind: "tool", Tool: block.Name, Input: clipInput(block.Input), TS: now()})
			}
		}
	case "result":
		r.setSession(j.threadID, ev.SessionID)
		if j.current != nil {
			item := *j.current
			j.current = nil
			r.addItem(j.threadID, item)
		}
		item := Item{ID: newID(), Kind: "result", TS: now(), Cost: ev.Cost}
		if ev.IsError {
			item.Kind = "error"
			item.Text = clip(ev.Result, 2000)
			if item.Text == "" {
				item.Text = "Claude reported an error (" + ev.Subtype + ")."
			}
		}
		r.addItem(j.threadID, item)
		return true
	}
	return false
}

func clip(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "…"
}

// clipInput keeps very large tool inputs out of the thread file. Approvals keep theirs whole,
// because the user must see exactly what they are approving.
func clipInput(raw json.RawMessage) json.RawMessage {
	if len(raw) <= 4000 {
		return raw
	}
	out, _ := json.Marshal(map[string]string{"note": "input too large to show", "preview": clip(string(raw), 3000)})
	return out
}

type tailBuffer struct {
	mu   sync.Mutex
	data []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data = append(t.data, p...)
	if len(t.data) > 8192 {
		t.data = t.data[len(t.data)-8192:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.data)
}

// Decide applies the user's answer to a pending approval. A second answer is refused, so a stale tap on
// another device can't change a decision that has already been acted on.
func (r *Runner) Decide(id string, allow bool, by string) error {
	if by == "" {
		return errors.New("no verified user")
	}
	r.mu.Lock()
	a := r.pending[id]
	if a == nil {
		r.mu.Unlock()
		return errors.New("this request has already been answered or has expired")
	}
	delete(r.pending, id)
	r.mu.Unlock()
	a.decision <- decision{allow: allow, by: by}
	return nil
}

// askApproval creates a pending item and blocks until an answer, a timeout, or the job ending.
func (r *Runner) askApproval(ctx context.Context, threadID, tool string, input json.RawMessage) (bool, string) {
	a := &approval{id: newID(), threadID: threadID, decision: make(chan decision, 1)}
	item := Item{ID: a.id, Kind: "approval", Tool: tool, Input: input, State: "pending", TS: now()}
	r.mu.Lock()
	r.pending[a.id] = a
	r.mu.Unlock()
	r.addItem(threadID, item)
	// Enough to see what was asked and to check it later, without keeping whole files in the log.
	sum := sha256.Sum256(input)
	r.actionLog(map[string]any{"action": "approval_asked", "thread": threadID, "id": a.id, "tool": tool,
		"input_preview": clip(string(input), 500), "input_bytes": len(input), "input_sha256": hex.EncodeToString(sum[:])})

	finish := func(state, by, why string, allow bool) (bool, string) {
		r.mu.Lock()
		delete(r.pending, a.id)
		if t := r.threads[threadID]; t != nil {
			for i := range t.Items {
				if t.Items[i].ID == a.id {
					t.Items[i].State, t.Items[i].By = state, by
					item = t.Items[i]
				}
			}
			r.saveThread(t)
		}
		r.mu.Unlock()
		r.publishItem(threadID, item)
		r.actionLog(map[string]any{"action": "approval_" + state, "thread": threadID, "id": a.id, "tool": tool, "by": by})
		return allow, why
	}
	timer := time.NewTimer(time.Duration(r.cfg.ApprovalTimeoutMin) * time.Minute)
	defer timer.Stop()
	select {
	case d := <-a.decision:
		if d.allow {
			return finish("approved", d.by, "", true)
		}
		return finish("denied", d.by, "The user denied this action.", false)
	case <-timer.C:
		return finish("expired", "", "No answer from the user in time, so this was denied.", false)
	case <-ctx.Done():
		return finish("expired", "", "The job ended before an answer.", false)
	}
}

// Cancel stops a running job.
func (r *Runner) Cancel(threadID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	j := r.jobs[threadID]
	if j == nil {
		return errors.New("that thread isn't running")
	}
	j.cancel()
	return nil
}

// Projects lists the allowlist's folders that exist on this PC.
func (r *Runner) Projects() []string {
	out := []string{}
	for _, p := range r.cfg.ProjectAllowlist {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			out = append(out, p)
		}
	}
	return out
}

// ThreadList returns all threads, newest first, without their items.
func (r *Runner) ThreadList() []Thread {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Thread, 0, len(r.threads))
	for _, t := range r.threads {
		copied := *t
		copied.Items = nil
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	return out
}

// CreateThread starts a new conversation in an allowed folder.
func (r *Runner) CreateThread(project, title string) (*Thread, error) {
	real, err := r.AllowedProject(project)
	if err != nil {
		return nil, err
	}
	title = clip(strings.Join(strings.Fields(title), " "), 80)
	if title == "" {
		title = "New conversation"
	}
	t := &Thread{ID: newID(), Title: title, Project: real, Created: now(), Updated: now()}
	r.mu.Lock()
	r.threads[t.ID] = t
	r.saveThread(t)
	r.mu.Unlock()
	return t, nil
}

// DeleteThread removes a stopped thread's history from the app. The Claude session itself stays in
// ~/.claude.
func (r *Runner) DeleteThread(id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.jobs[id] != nil {
		return errors.New("stop it first")
	}
	if r.threads[id] == nil {
		return errors.New("no such thread")
	}
	delete(r.threads, id)
	_ = os.Remove(filepath.Join(r.root, "threads", id+".json"))
	return nil
}
