// Package relay is the app's public entry point. Everything arriving from Cloudflare must pass the
// Access check before any route runs. It holds no WhatsApp or Claude logic of its own: it forwards
// to the bridge and the runner, both on localhost, and stores the Personal/Work groups.
package relay

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"wa-relay/internal/access"
)

const (
	// ClientHeader marks a request as coming from the app. A web page can't add a custom header to
	// a cross-site request without a preflight, which Access answers with a login redirect.
	ClientHeader = "X-WA-Client"
	maxChunk     = 70 << 20
	maxPicture   = 11 << 20
	maxJSONBody  = 4 << 20
	// Contact cards parsed from a vCard file, sent in one go.
	maxCardImport = 16 << 20
	logMaxBytes  = 10 << 20
	// Clients treat 25 seconds of silence as disconnected, so this leaves room for one to go missing.
	heartbeatEvery = 10 * time.Second
	Version        = "1.2.13"
)

// Config is relay.json.
type Config struct {
	TeamDomain    string   `json:"team_domain"`
	AUD           string   `json:"aud"`
	AllowedEmails []string `json:"allowed_emails"`
	ListenPort    int      `json:"listen_port"`
	BridgeURL     string   `json:"bridge_url"`
	RunnerURL     string   `json:"runner_url"`
	DataDir       string   `json:"data_dir"`
	TokenFile     string   `json:"runner_token_file"`
	DownloadsDir  string   `json:"downloads_dir"`
	CallsLog      string   `json:"calls_log"`
	// NameRule, if set, puts group chats whose name matches into one custom group (rule.go). Off when absent.
	NameRule *NameRuleConfig `json:"name_rule,omitempty"`
}

// Validate refuses config that could leave anything unprotected.
func (c *Config) Validate() error {
	if c.ListenPort < 1024 || c.ListenPort > 65535 {
		return errors.New("listen_port out of range")
	}
	for name, raw := range map[string]string{"bridge_url": c.BridgeURL, "runner_url": c.RunnerURL} {
		if !strings.HasPrefix(raw, "http://127.0.0.1:") && !strings.HasPrefix(raw, "http://localhost:") {
			return fmt.Errorf("%s must be on localhost", name)
		}
	}
	if c.DataDir == "" || c.TokenFile == "" || c.CallsLog == "" {
		return errors.New("data_dir, runner_token_file and calls_log are required")
	}
	if c.NameRule != nil {
		if _, err := c.NameRule.compile(); err != nil {
			return err
		}
	}
	return nil
}

// Server is the relay.
type Server struct {
	cfg     Config
	verify  *access.Verifier
	hub     *Hub
	groups  *Groups
	bridge  *httputil.ReverseProxy
	runner  *httputil.ReverseProxy
	token   string
	client  *http.Client
	logMu   sync.Mutex
	logFile *os.File
	logSize int64

	silenceLimit time.Duration // Set by tests. Otherwise a minute.

	mu           sync.Mutex
	bridgeStatus json.RawMessage
	runnerUp     bool
}

// New builds a server. keys is injectable for tests.
func New(cfg Config, keys access.KeyFunc) (*Server, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if keys == nil {
		keys = access.NewJWKS(access.CertsURL(cfg.TeamDomain)).Key
	}
	verify, err := access.New(cfg.TeamDomain, cfg.AUD, cfg.AllowedEmails, keys)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return nil, err
	}
	token, err := EnsureToken(cfg.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("runner token: %w", err)
	}
	hub := NewHub()
	groups, err := LoadGroups(filepath.Join(cfg.DataDir, "groups.json"), hub)
	if err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, verify: verify, hub: hub, groups: groups, token: token, client: &http.Client{}}
	s.bridge, err = s.proxy(cfg.BridgeURL, "")
	if err != nil {
		return nil, err
	}
	s.runner, err = s.proxy(cfg.RunnerURL, token)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Hub is the event hub.
func (s *Server) Hub() *Hub { return s.hub }

// Close releases the log file.
func (s *Server) Close() {
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile != nil {
		s.logFile.Close()
		s.logFile = nil
	}
}

// stripHeaders are edge credentials and browser headers that never go further than the relay.
var stripHeaders = []string{
	"Authorization", "Cookie", "Cf-Access-Jwt-Assertion", "Cf-Access-Token", "Cf-Access-Client-Id",
	"Cf-Access-Client-Secret", "Cf-Authorization", "Origin", "Referer", ClientHeader,
	RunnerTokenHeader, EmailHeader,
}

const (
	// RunnerTokenHeader authenticates the relay to the runner.
	RunnerTokenHeader = "X-Runner-Token"
	// EmailHeader tells the runner who approved an action.
	EmailHeader = "X-Verified-Email"
)

type emailKey struct{}

func (s *Server) proxy(target, token string) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(target)
	if err != nil {
		return nil, err
	}
	return &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(u)
			pr.Out.Host = u.Host
			for _, h := range stripHeaders {
				pr.Out.Header.Del(h)
			}
			if token != "" {
				pr.Out.Header.Set(RunnerTokenHeader, token)
			}
			// Who's asking, from the verified login. The bridge logs it with each contact card change, and only
			// lets a request carrying it delete or import cards. Anything a client sent under this name was stripped above.
			if email, ok := pr.In.Context().Value(emailKey{}).(string); ok && email != "" {
				pr.Out.Header.Set(EmailHeader, email)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "That service isn't running on your home PC."})
		},
	}, nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// bridgeRoutes maps public paths to the bridge's. Only these are reachable, so the bridge's other
// routes (including the MCP server's /api/send) can't be called through the relay.
var bridgeRoutes = map[string]string{
	"GET /api/sync":                 "/api/app/sync",
	"GET /api/contacts":             "/api/app/contacts",
	"POST /api/send":                "/api/app/send",
	"POST /api/name":                "/api/app/name",
	"DELETE /api/name":              "/api/app/name",
	"POST /api/react":               "/api/app/react",
	"POST /api/edit":                "/api/app/edit",
	"POST /api/revoke":              "/api/app/revoke",
	"POST /api/hide":                "/api/app/hide",
	"POST /api/forward":             "/api/app/forward",
	"GET /api/cards":                "/api/app/cards",
	"POST /api/cards/save":          "/api/app/cards/save",
	"POST /api/cards/delete":        "/api/app/cards/delete",
	"GET /api/cards/history":        "/api/app/cards/history",
	"POST /api/cards/undo":          "/api/app/cards/undo",
	"POST /api/cards/suggest":       "/api/app/cards/suggest",
	"POST /api/read":                "/api/app/read",
	"POST /api/history":             "/api/app/history",
	"GET /api/media":                "/api/app/media",
	"GET /api/pictures":             "/api/app/pictures",
	"GET /api/picture":              "/api/app/picture",
	"GET /api/picture/group-access": "/api/app/picture/group-access",
	"GET /api/group/members":        "/api/app/group/members",
	"DELETE /api/picture/own":       "/api/app/picture/own",
}

// runnerRoutes are the Claude endpoints, each with a fixed method.
var runnerRoutes = map[string]bool{
	"GET /status":                 true,
	"GET /projects":               true,
	"GET /threads":                true,
	"POST /threads":               true,
	"GET /threads/{id}":           true,
	"DELETE /threads/{id}":        true,
	"POST /threads/{id}/messages": true,
	"POST /threads/{id}/cancel":   true,
	"POST /approvals/{id}":        true,
	"POST /sort":                  true,
	"GET /sort/{id}":              true,
	"POST /sort/{id}/dismiss":     true,
}

// Handler is the whole public surface. Auth runs first, so an unauthenticated caller learns nothing
// about which paths exist.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /login-ok", s.handleLoginOK)
	mux.HandleFunc("GET /download", s.handleDownloads)
	mux.HandleFunc("GET /download/{name}", s.handleDownloadFile)
	api := http.NewServeMux()
	api.HandleFunc("GET /api/whoami", s.handleWhoami)
	api.HandleFunc("GET /api/status", s.handleStatus)
	api.HandleFunc("GET /api/events", s.handleEvents)
	api.HandleFunc("GET /api/groups", s.handleGroupsGet)
	api.HandleFunc("POST /api/groups", s.handleGroupCreate)
	api.HandleFunc("PATCH /api/groups/{id}", s.handleGroupUpdate)
	api.HandleFunc("DELETE /api/groups/{id}", s.handleGroupDelete)
	api.HandleFunc("POST /api/assign", s.handleAssign)
	api.HandleFunc("GET /api/update/latest", s.handleUpdateLatest)
	api.HandleFunc("GET /api/update/installer/{name}", s.handleUpdateInstaller)
	for route, to := range bridgeRoutes {
		to := to
		api.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path, r.URL.RawPath = to, ""
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
			s.bridge.ServeHTTP(w, r)
		})
	}
	// A whole address book's worth of cards, so it gets more room than other requests.
	api.HandleFunc("POST /api/cards/import", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path, r.URL.RawPath = "/api/app/cards/import", ""
		r.Body = http.MaxBytesReader(w, r.Body, maxCardImport)
		s.bridge.ServeHTTP(w, r)
	})
	uploadProxy := func(suffix string, limit int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path, r.URL.RawPath = "/api/app/upload/"+r.PathValue("uid")+suffix, ""
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			s.bridge.ServeHTTP(w, r)
		}
	}
	// The bridge checks and re-encodes the image. This only limits how much can arrive.
	api.HandleFunc("POST /api/picture/own", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path, r.URL.RawPath = "/api/app/picture/own", ""
		r.Body = http.MaxBytesReader(w, r.Body, maxPicture)
		s.bridge.ServeHTTP(w, r)
	})
	// This changes what everyone in a group sees, so each try is logged with its chat and result.
	api.HandleFunc("POST /api/picture/group", func(w http.ResponseWriter, r *http.Request) {
		jid := r.URL.Query().Get("jid")
		r.URL.Path, r.URL.RawPath = "/api/app/picture/group", ""
		r.Body = http.MaxBytesReader(w, r.Body, maxPicture)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		s.bridge.ServeHTTP(rec, r)
		email, _ := r.Context().Value(emailKey{}).(string)
		s.logGroupChange("group_photo", email, jid, rec.status, w.Header().Get("X-Group-Photo-Result"))
	})
	// This adds and notifies other people, so each try is logged with its result. The log holds counts and
	// the new group's address, and no member's name or number.
	api.HandleFunc("POST /api/group/create", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path, r.URL.RawPath = "/api/app/group/create", ""
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		s.bridge.ServeHTTP(rec, r)
		email, _ := r.Context().Value(emailKey{}).(string)
		s.writeLog(map[string]any{
			"t": time.Now().Format(time.RFC3339), "email": email, "event": "group_create", "result": rec.status,
			"outcome": w.Header().Get("X-Group-Create-Result"), "chat": w.Header().Get("X-Group-Create-Chat"),
			"count": w.Header().Get("X-Group-Create-Count"),
		})
	})
	// Others see a rename and an exit, so each try is logged with its chat and result. The log never holds the
	// name.
	for _, action := range []string{"rename", "leave"} {
		api.HandleFunc("POST /api/group/"+action, func(w http.ResponseWriter, r *http.Request) {
			jid := r.URL.Query().Get("jid")
			r.URL.Path, r.URL.RawPath = "/api/app/group/"+action, ""
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
			rec := &statusRecorder{ResponseWriter: w, status: 200}
			s.bridge.ServeHTTP(rec, r)
			email, _ := r.Context().Value(emailKey{}).(string)
			s.logGroupChange("group_"+action, email, jid, rec.status, w.Header().Get("X-Group-Action-Result"))
		})
	}
	api.HandleFunc("GET /api/upload/{uid}", uploadProxy("", maxJSONBody))
	api.HandleFunc("PUT /api/upload/{uid}", uploadProxy("", maxChunk))
	api.HandleFunc("POST /api/upload/{uid}/finish", uploadProxy("/finish", maxJSONBody))
	for route := range runnerRoutes {
		method, path, _ := strings.Cut(route, " ")
		api.HandleFunc(method+" /api/claude"+path, func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path, r.URL.RawPath = strings.TrimPrefix(r.URL.Path, "/api/claude"), ""
			r.Body = http.MaxBytesReader(w, r.Body, maxJSONBody)
			s.runner.ServeHTTP(w, r)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: 200}
		w = rec
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		email, status, why := s.verify.Check(r)
		if status != http.StatusOK {
			s.logCall(email, r, status, why)
			writeJSON(w, status, map[string]string{"error": http.StatusText(status)})
			return
		}
		defer func() { s.logCall(email, r, rec.status, "") }()
		r = r.WithContext(context.WithValue(r.Context(), emailKey{}, email))
		if strings.HasPrefix(r.URL.Path, "/api/") {
			// Blocks a malicious page riding on the login cookie, on top of Access's SameSite=Lax.
			if r.Header.Get("Origin") != "" || r.Header.Get(ClientHeader) == "" {
				rec.status = http.StatusForbidden
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "Forbidden"})
				return
			}
			api.ServeHTTP(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// Flush and Unwrap keep streaming working through this wrapper.
func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *statusRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// logCall records who called what. Query strings are left out because they hold chat IDs; tokens
// and message text never reach it.
func (s *Server) logCall(email string, r *http.Request, status int, why string) {
	s.writeLog(map[string]any{
		"t": time.Now().Format(time.RFC3339), "email": email, "method": r.Method, "path": r.URL.Path, "result": status, "why": why,
	})
}

// logGroupChange is the one log line that names a chat, because it records a change other people see. The
// address came from the caller, so it's only kept if it's a valid group address.
func (s *Server) logGroupChange(event, email, jid string, status int, outcome string) {
	if !strings.HasSuffix(jid, "@g.us") || !jidRe.MatchString(jid) {
		jid = "not a group address"
	}
	s.writeLog(map[string]any{
		"t": time.Now().Format(time.RFC3339), "email": email, "event": event, "chat": jid, "result": status, "outcome": outcome,
	})
}

func (s *Server) writeLog(fields map[string]any) {
	line, _ := json.Marshal(fields)
	s.logMu.Lock()
	defer s.logMu.Unlock()
	if s.logFile == nil || s.logSize > logMaxBytes {
		if s.logFile != nil {
			s.logFile.Close()
			_ = os.Rename(s.cfg.CallsLog, s.cfg.CallsLog+".1")
		}
		_ = os.MkdirAll(filepath.Dir(s.cfg.CallsLog), 0o700)
		f, err := os.OpenFile(s.cfg.CallsLog, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		s.logFile = f
		if info, err := f.Stat(); err == nil {
			s.logSize = info.Size()
		}
	}
	n, _ := s.logFile.Write(append(line, '\n'))
	s.logSize += int64(n)
}

func (s *Server) handleLoginOK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>Signed in</title>
<body style="font:16px system-ui;display:grid;place-items:center;height:90vh;margin:0">Signed in. You can close this window.</body>`)
}

func (s *Server) installers() []os.DirEntry {
	if s.cfg.DownloadsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.cfg.DownloadsDir)
	if err != nil {
		return nil
	}
	var out []os.DirEntry
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".exe") {
			out = append(out, e)
		}
	}
	return out
}

// handleDownloads lists installers, so another PC can install the app after signing in once.
func (s *Server) handleDownloads(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>Install</title>
<body style="font:16px system-ui;max-width:32rem;margin:3rem auto;padding:0 1rem"><h1>Install</h1><ul>`)
	for _, e := range s.installers() {
		name := html.EscapeString(e.Name())
		fmt.Fprintf(w, `<li><a href="/download/%s">%s</a></li>`, url.PathEscape(e.Name()), name)
	}
	fmt.Fprint(w, `</ul></body>`)
}

func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, e := range s.installers() {
		if e.Name() == name {
			w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(name, `"`, ``)+`"`)
			http.ServeFile(w, r, filepath.Join(s.cfg.DownloadsDir, name))
			return
		}
	}
	http.NotFound(w, r)
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request) {
	email, _ := r.Context().Value(emailKey{}).(string)
	writeJSON(w, 200, map[string]any{"email": email, "version": Version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	bridge, up := s.bridgeStatus, s.runnerUp
	s.mu.Unlock()
	if bridge == nil {
		bridge = json.RawMessage(`{"connected":false,"logged_in":false,"bridge_down":true}`)
	}
	writeJSON(w, 200, map[string]any{"bridge": bridge, "runner_up": up, "version": Version})
}

// handleEvents is the one stream every client holds. Connecting sends the current state first, so a
// reconnecting client never shows stale status.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]string{"error": "no streaming"})
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	sub := s.hub.Subscribe()
	defer s.hub.Unsubscribe(sub)
	s.mu.Lock()
	bridge, up := s.bridgeStatus, s.runnerUp
	s.mu.Unlock()
	if bridge != nil {
		fmt.Fprintf(w, "data: %s\n\n", bridge)
	}
	fmt.Fprintf(w, "data: {\"type\":\"claude_status\",\"up\":%v}\n\n", up)
	flusher.Flush()
	beat := time.NewTicker(heartbeatEvery)
	defer beat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-beat.C:
			fmt.Fprint(w, ": hb\n\n")
			flusher.Flush()
		case data, open := <-sub:
			if !open {
				return
			}
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (s *Server) handleGroupsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.groups.Snapshot())
}

func readBody(w http.ResponseWriter, r *http.Request, into any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody)).Decode(into); err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": "bad request"})
		return false
	}
	return true
}

func groupResult(w http.ResponseWriter, s *Server, err error) {
	if err != nil {
		writeJSON(w, 400, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, 200, s.groups.Snapshot())
}

func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  string `json:"name"`
		Color string `json:"color"`
	}
	if readBody(w, r, &req) {
		groupResult(w, s, s.groups.Create(req.Name, req.Color))
	}
}

func (s *Server) handleGroupUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name  *string `json:"name"`
		Color *string `json:"color"`
		Order *int    `json:"order"`
	}
	if readBody(w, r, &req) {
		groupResult(w, s, s.groups.Update(r.PathValue("id"), req.Name, req.Color, req.Order))
	}
}

func (s *Server) handleGroupDelete(w http.ResponseWriter, r *http.Request) {
	groupResult(w, s, s.groups.Delete(r.PathValue("id")))
}

func (s *Server) handleAssign(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JIDs  []string `json:"jids"`
		Group string   `json:"group"`
	}
	if readBody(w, r, &req) {
		groupResult(w, s, s.groups.Assign(req.JIDs, req.Group))
	}
}

// Run starts the upstream streams and serves until the listener fails. The address is deliberately
// not configurable beyond the port: nothing reaches this except through cloudflared.
func (s *Server) Run(ctx context.Context) error {
	go s.pump(ctx, s.cfg.BridgeURL+"/api/app/events", "", s.onBridgeEvent, func() {
		s.mu.Lock()
		s.bridgeStatus = json.RawMessage(`{"type":"status","connected":false,"logged_in":false,"bridge_down":true}`)
		down := s.bridgeStatus
		s.mu.Unlock()
		s.hub.PublishRaw(down)
	})
	go s.pump(ctx, s.cfg.RunnerURL+"/events", s.token, s.onRunnerEvent, func() { s.setRunner(false) })
	if rule, _ := s.cfg.NameRule.compile(); rule != nil {
		go s.ruleLoop(ctx, rule)
	}
	srv := &http.Server{
		Addr:              net.JoinHostPort("127.0.0.1", strconv.Itoa(s.cfg.ListenPort)),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		_ = srv.Close()
	}()
	return srv.ListenAndServe()
}

func (s *Server) streamSilence() time.Duration {
	if s.silenceLimit > 0 {
		return s.silenceLimit
	}
	return 60 * time.Second
}

func (s *Server) setRunner(up bool) {
	s.mu.Lock()
	changed := s.runnerUp != up
	s.runnerUp = up
	s.mu.Unlock()
	if changed {
		s.hub.PublishRaw([]byte(fmt.Sprintf(`{"type":"claude_status","up":%v}`, up)))
	}
}

func (s *Server) onBridgeEvent(data []byte) {
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &head)
	if head.Type == "status" {
		s.mu.Lock()
		s.bridgeStatus = append(json.RawMessage{}, data...)
		s.mu.Unlock()
	}
	s.hub.PublishRaw(data)
}

func (s *Server) onRunnerEvent(data []byte) {
	s.setRunner(true)
	var head struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(data, &head)
	if head.Type != "hello" {
		s.hub.PublishRaw(data)
	}
}

// pump holds one upstream event stream open, reconnecting forever.
func (s *Server) pump(ctx context.Context, target, token string, onEvent func([]byte), onDown func()) {
	backoff := time.Second
	lastLogged := ""
	for ctx.Err() == nil {
		started := time.Now()
		err := s.readStream(ctx, target, token, onEvent)
		if ctx.Err() != nil {
			return
		}
		onDown()
		if err != nil && err.Error() != lastLogged {
			lastLogged = err.Error() // The same failure repeating isn't worth a line each time.
			log.Printf("stream %s ended: %v", target, err)
		}
		if time.Since(started) > 30*time.Second {
			backoff = time.Second
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func (s *Server) readStream(ctx context.Context, target, token string, onEvent func([]byte)) error {
	// The bridge and runner both send a heartbeat every 20 seconds, so a long silence means it has hung.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchdog := time.AfterFunc(s.streamSilence(), cancel)
	defer watchdog.Stop()
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set(RunnerTokenHeader, token)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	reader := bufio.NewReaderSize(resp.Body, 1<<20)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return err
		}
		watchdog.Reset(s.streamSilence())
		if len(line) > 6 && string(line[:6]) == "data: " {
			onEvent(append([]byte(nil), line[6:len(line)-1]...))
		}
	}
}
