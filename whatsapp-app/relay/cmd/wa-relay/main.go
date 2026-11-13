// wa-relay is the app's public entry point, reached only through cloudflared on 127.0.0.1:8704.
// It won't start on bad config.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"wa-relay/internal/relay"
)

func main() {
	path := filepath.Join(filepath.Dir(exePath()), "relay.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay refusing to start: %v\n", err)
		os.Exit(2)
	}
	var cfg relay.Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		fmt.Fprintf(os.Stderr, "relay refusing to start: %v\n", err)
		os.Exit(2)
	}
	srv, err := relay.New(cfg, nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "relay refusing to start: %v\n", err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := srv.Run(ctx); err != nil && ctx.Err() == nil {
		fmt.Fprintf(os.Stderr, "relay stopped: %v\n", err)
		os.Exit(1)
	}
}

func exePath() string {
	p, err := os.Executable()
	if err != nil {
		return "."
	}
	return p
}
