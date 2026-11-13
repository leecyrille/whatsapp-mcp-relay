package relay

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The relay's stream to the bridge is what tells apps the bridge has gone. A bridge that hangs without
// closing the connection has to count too.
func TestSilentUpstreamCountsAsGone(t *testing.T) {
	f := newFixture(t)
	f.srv.silenceLimit = 150 * time.Millisecond
	silent := func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "data: {\"type\":\"status\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/silent", silent)
	mux.HandleFunc("/beating", func(w http.ResponseWriter, r *http.Request) {
		for i := 0; i < 6; i++ {
			fmt.Fprint(w, ": hb\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
		fmt.Fprint(w, "data: {\"type\":\"done\"}\n\n")
		w.(http.Flusher).Flush()
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux}
	go server.Serve(ln)
	defer server.Close()
	base := "http://" + ln.Addr().String()

	start := time.Now()
	if err := f.srv.readStream(context.Background(), base+"/silent", "", func([]byte) {}); err == nil {
		t.Fatal("no error from a silent stream")
	}
	if took := time.Since(start); took < 100*time.Millisecond || took > 3*time.Second {
		t.Fatalf("a silent stream took %v to count as gone", took)
	}
	// Longer than the limit in total, but never silent for that long.
	var last string
	f.srv.readStream(context.Background(), base+"/beating", "", func(b []byte) { last = string(b) })
	if last != `{"type":"done"}` {
		t.Fatalf("a stream with heartbeats was cut off: %q", last)
	}
}

func TestHeartbeatBeatsTheAppsSilenceLimit(t *testing.T) {
	const appSilenceLimit = 25 * time.Second
	if heartbeatEvery*2 >= appSilenceLimit {
		t.Fatalf("heartbeat %v leaves too little room", heartbeatEvery)
	}
}

// TestLiveOutageFront is a tool, not a test: the real relay code, reading the real bridge, behind a front
// that stands in for Cloudflare Access. It lets the real app see what happens when the real bridge stops.
// The front only passes GET, so nothing can be sent or changed through it, whatever the app tries.
//
//	LIVE_OUTAGE_PORT=18794 go test -run TestLiveOutageFront -timeout 30m ./internal/relay
func TestLiveOutageFront(t *testing.T) {
	raw := os.Getenv("LIVE_OUTAGE_PORT")
	if raw == "" {
		t.Skip("set LIVE_OUTAGE_PORT to run")
	}
	port, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	groupsFile := os.Getenv("LIVE_OUTAGE_GROUPS")
	if groupsFile == "" {
		groupsFile = `C:\mcp\whatsapp-app\data\groups.json`
	}
	if real, err := os.ReadFile(groupsFile); err == nil {
		os.WriteFile(filepath.Join(dir, "groups.json"), real, 0o600)
	}
	srv, err := New(Config{
		TeamDomain: "myteam", AUD: "aud-1", AllowedEmails: []string{"user@example.com"}, ListenPort: port + 1,
		BridgeURL: "http://127.0.0.1:18700", RunnerURL: "http://127.0.0.1:18705",
		DataDir: dir, TokenFile: filepath.Join(dir, "runner.token"), CallsLog: filepath.Join(dir, "calls.log"),
	}, testKeys)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Run(ctx)
	inner := srv.Handler()
	front := &http.Server{Addr: net.JoinHostPort("127.0.0.1", raw), Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login-ok" {
			w.Header().Set("Set-Cookie", "CF_Authorization=check; Max-Age=3600; HttpOnly; Path=/")
			w.Header().Set("Content-Type", "text/html")
			fmt.Fprint(w, "<!doctype html><title>ok</title>")
			return
		}
		// Groups here are this tool's own temporary copy. A picture change reaches the real bridge, for the
		// two-client test, which removes it again. Sending and reading state can never get through.
		writable := os.Getenv("LIVE_OUTAGE_WRITES") == "1" && (strings.HasPrefix(r.URL.Path, "/api/groups") || r.URL.Path == "/api/assign" || r.URL.Path == "/api/picture/own")
		if r.Method != http.MethodGet && !writable {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "That service isn't running on your home PC."})
			return
		}
		r.Header.Set("Cf-Access-Jwt-Assertion", goodToken(t))
		inner.ServeHTTP(w, r)
	})}
	t.Log(front.ListenAndServe())
}
