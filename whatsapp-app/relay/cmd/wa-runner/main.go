// wa-runner runs Claude jobs for the app, on 127.0.0.1 only. It runs under the MCP-Claude task, so
// it starts at logon in the user's session, and mcpctl stop claude stops it and every job it started.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"wa-relay/internal/relay"
	"wa-relay/internal/runner"
)

func main() {
	exe, err := os.Executable()
	if err != nil {
		exe = "."
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(exe), "runner.json"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner refusing to start: %v\n", err)
		os.Exit(2)
	}
	var cfg runner.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "runner refusing to start: %v\n", err)
		os.Exit(2)
	}
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "runner refusing to start: %v\n", err)
		os.Exit(2)
	}
	token, err := relay.EnsureToken(cfg.TokenFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner refusing to start: %v\n", err)
		os.Exit(2)
	}
	r, err := runner.New(cfg, token)
	if err != nil {
		fmt.Fprintf(os.Stderr, "runner refusing to start: %v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := r.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "runner stopped: %v\n", err)
		os.Exit(1)
	}
}
