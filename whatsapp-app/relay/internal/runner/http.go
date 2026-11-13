package runner

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"ok": false, "error": msg})
}

// Handler is the runner's API. Its routes need the relay's token. The approval endpoint for Claude
// has its own per-job token in the path, and the relay has no route to it.
func (r *Runner) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /events", r.handleEvents)
	api.HandleFunc("GET /status", r.handleStatus)
	api.HandleFunc("GET /projects", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"projects": r.Projects()})
	})
	api.HandleFunc("GET /threads", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 200, map[string]any{"threads": r.ThreadList()})
	})
	api.HandleFunc("POST /threads", r.handleCreateThread)
	api.HandleFunc("GET /threads/{id}", r.handleGetThread)
	api.HandleFunc("DELETE /threads/{id}", r.handleDeleteThread)
	api.HandleFunc("POST /threads/{id}/messages", r.handleMessage)
	api.HandleFunc("POST /threads/{id}/cancel", r.handleCancel)
	api.HandleFunc("POST /approvals/{id}", r.handleDecide)
	api.HandleFunc("POST /sort", r.handleSort)
	api.HandleFunc("GET /sort/{id}", r.handleSortGet)
	api.HandleFunc("POST /sort/{id}/dismiss", r.handleSortDismiss)
	mcp := http.HandlerFunc(r.handleMCP)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Origin") != "" {
			fail(w, 403, "forbidden")
			return
		}
		if strings.HasPrefix(req.URL.Path, "/mcp/") {
			mcp.ServeHTTP(w, req)
			return
		}
		given := req.Header.Get(TokenHeader)
		if subtle.ConstantTimeCompare([]byte(given), []byte(r.token)) != 1 {
			fail(w, 401, "unauthorized")
			return
		}
		api.ServeHTTP(w, req)
	})
}

func (r *Runner) handleEvents(w http.ResponseWriter, req *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, "no streaming")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan []byte, 1024)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if _, open := r.subs[ch]; open {
			delete(r.subs, ch)
			close(ch)
		}
		r.mu.Unlock()
	}()
	fmt.Fprint(w, "data: {\"type\":\"hello\"}\n\n")
	flusher.Flush()
	beat := time.NewTicker(20 * time.Second)
	defer beat.Stop()
	for {
		select {
		case <-req.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": hb\n\n")
			flusher.Flush()
		case data, open := <-ch:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

// signedIn asks the CLI whether it is logged in, at most once a minute.
func (r *Runner) signedIn() bool {
	r.authMu.Lock()
	defer r.authMu.Unlock()
	if time.Since(r.auth.at) < time.Minute {
		return r.auth.loggedIn
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.cfg.ClaudeExe, "auth", "status")
	cmd.Env = childEnv(60000)
	hideWindow(cmd)
	out, err := cmd.Output()
	var parsed struct {
		LoggedIn bool `json:"loggedIn"`
	}
	r.auth = authState{at: time.Now(), loggedIn: err == nil && json.Unmarshal(out, &parsed) == nil && parsed.LoggedIn}
	return r.auth.loggedIn
}

func (r *Runner) handleStatus(w http.ResponseWriter, _ *http.Request) {
	r.mu.Lock()
	running := len(r.jobs)
	r.mu.Unlock()
	writeJSON(w, 200, map[string]any{
		"signed_in": r.signedIn(), "running_jobs": running, "max_running_jobs": r.cfg.MaxRunningJobs,
		"approval_timeout_minutes": r.cfg.ApprovalTimeoutMin,
	})
}

func (r *Runner) handleCreateThread(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Project string `json:"project"`
		Title   string `json:"title"`
	}
	if json.NewDecoder(req.Body).Decode(&body) != nil {
		fail(w, 400, "bad request")
		return
	}
	t, err := r.CreateThread(body.Project, body.Title)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, t)
}

func (r *Runner) handleGetThread(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("id")
	r.mu.Lock()
	defer r.mu.Unlock()
	t := r.threads[id]
	if t == nil {
		fail(w, 404, "no such thread")
		return
	}
	writeJSON(w, 200, t)
}

func (r *Runner) handleDeleteThread(w http.ResponseWriter, req *http.Request) {
	if err := r.DeleteThread(req.PathValue("id")); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (r *Runner) handleMessage(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Text string `json:"text"`
	}
	if json.NewDecoder(req.Body).Decode(&body) != nil {
		fail(w, 400, "bad request")
		return
	}
	if !r.signedIn() {
		fail(w, 409, "Claude isn't signed in on the home PC. Run: claude auth login")
		return
	}
	if err := r.StartMessage(req.PathValue("id"), body.Text); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 202, map[string]any{"ok": true})
}

func (r *Runner) handleCancel(w http.ResponseWriter, req *http.Request) {
	if err := r.Cancel(req.PathValue("id")); err != nil {
		fail(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (r *Runner) handleDecide(w http.ResponseWriter, req *http.Request) {
	var body struct {
		Approve *bool `json:"approve"`
	}
	if json.NewDecoder(req.Body).Decode(&body) != nil || body.Approve == nil {
		fail(w, 400, "approve must be true or false")
		return
	}
	if err := r.Decide(req.PathValue("id"), *body.Approve, req.Header.Get(EmailHeader)); err != nil {
		fail(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleMCP is the one tool Claude uses to ask permission. Streamable HTTP with a JSON or SSE reply;
// the wait can run for minutes, so the reply is SSE with pings to keep the connection alive.
func (r *Runner) handleMCP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	token := strings.TrimPrefix(req.URL.Path, "/mcp/")
	var j *job
	r.mu.Lock()
	for _, candidate := range r.jobs {
		if subtle.ConstantTimeCompare([]byte(candidate.mcpToken), []byte(token)) == 1 {
			j = candidate
		}
	}
	r.mu.Unlock()
	if j == nil {
		fail(w, 404, "unknown job")
		return
	}
	var call struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Name            string `json:"name"`
			ProtocolVersion string `json:"protocolVersion"`
			Arguments       struct {
				ToolName string          `json:"tool_name"`
				Input    json.RawMessage `json:"input"`
			} `json:"arguments"`
		} `json:"params"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, req.Body, 8<<20)).Decode(&call) != nil {
		fail(w, 400, "bad request")
		return
	}
	reply := func(result any) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result}
	}
	switch call.Method {
	case "initialize":
		version := call.Params.ProtocolVersion
		if version == "" {
			version = "2025-03-26"
		}
		writeJSON(w, 200, reply(map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "approver", "version": "1.0.0"},
		}))
	case "ping":
		writeJSON(w, 200, reply(map[string]any{}))
	case "tools/list":
		writeJSON(w, 200, reply(map[string]any{"tools": []any{map[string]any{
			"name":        "ask",
			"description": "Ask the user to approve a tool call.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"tool_name":   map[string]any{"type": "string"},
					"input":       map[string]any{"type": "object"},
					"tool_use_id": map[string]any{"type": "string"},
				},
				"required": []string{"tool_name", "input"},
			},
		}}}))
	case "tools/call":
		if call.Params.Name != "ask" || call.Params.Arguments.ToolName == "" {
			writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32602, "message": "unknown tool"}})
			return
		}
		r.answerApproval(w, req, j, call.ID, call.Params.Arguments.ToolName, call.Params.Arguments.Input)
	default:
		if strings.HasPrefix(call.Method, "notifications/") || call.ID == nil {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32601, "message": "unknown method"}})
	}
}

func (r *Runner) answerApproval(w http.ResponseWriter, req *http.Request, j *job, id json.RawMessage, tool string, input json.RawMessage) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	flusher, canStream := w.(http.Flusher)
	stream := canStream && strings.Contains(req.Header.Get("Accept"), "text/event-stream")
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		flusher.Flush()
	}
	result := make(chan []byte, 1)
	go func() {
		// If this connection drops, no answer can reach Claude, so the request can't stay open.
		allow, why := r.askApproval(req.Context(), j.threadID, tool, input)
		payload := map[string]any{"behavior": "deny", "message": why}
		if allow {
			payload = map[string]any{"behavior": "allow", "updatedInput": json.RawMessage(input)}
		}
		text, _ := json.Marshal(payload)
		out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": string(text)}},
		}})
		result <- out
	}()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case out := <-result:
			if stream {
				fmt.Fprintf(w, "event: message\ndata: %s\n\n", out)
				flusher.Flush()
			} else {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(out)
			}
			return
		case <-ping.C:
			if stream {
				fmt.Fprint(w, ": ping\n\n")
				flusher.Flush()
			}
		case <-req.Context().Done():
			return
		}
	}
}

// Run serves until the listener fails. Bound to localhost; the address isn't configurable.
func (r *Runner) Run(ctx context.Context) error {
	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(r.cfg.ListenPort)),
		Handler:           r.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}
