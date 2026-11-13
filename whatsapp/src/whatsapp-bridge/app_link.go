package main

import (
	"context"
	"net/http"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
)

// Why the link is down when it won't come back by itself. The watchdog reads this from /api/health, because
// "not connected" alone looks the same during a short network drop.
const (
	linkLoggedOut      = "logged_out"
	linkClientOutdated = "client_outdated"
	linkStreamReplaced = "stream_replaced"
	linkTemporaryBan   = "temporary_ban"
)

const outdatedRetryDelay = 15 * time.Minute

type linkHealth struct {
	mu       sync.Mutex
	problem  string
	retrying bool
	// Replaced in tests.
	delay     time.Duration
	reconnect func()
}

// note records what an event says about the link and returns true when a reconnect was scheduled.
func (l *linkHealth) note(evt any) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch evt.(type) {
	case *events.Connected:
		l.problem = ""
	case *events.LoggedOut:
		l.problem = linkLoggedOut
	case *events.StreamReplaced:
		l.problem = linkStreamReplaced
	case *events.TemporaryBan:
		l.problem = linkTemporaryBan
	case *events.ClientOutdated:
		l.problem = linkClientOutdated
		// The library doesn't retry this one. Without a retry the link stays down until someone restarts it.
		if !l.retrying && l.reconnect != nil {
			l.retrying = true
			go func() {
				time.Sleep(l.delay)
				l.reconnect()
				l.mu.Lock()
				l.retrying = false
				l.mu.Unlock()
			}()
			return true
		}
	}
	return false
}

func (l *linkHealth) Problem() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.problem
}

// link is set up once in main, and read by the health route.
var link = &linkHealth{delay: outdatedRetryDelay}

// newerVersion says whether WhatsApp's published version should replace the one the library was built with.
func newerVersion(current, latest wastore.WAVersionContainer) bool {
	for i := range current {
		if latest[i] != current[i] {
			return latest[i] > current[i]
		}
	}
	return false
}

// refreshWAVersion asks WhatsApp for its current web version, because an old one is what gets a client refused
// with error 405. Failing here is fine: the version built into the library is still used.
func refreshWAVersion(logger waLog.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	latest, err := whatsmeow.GetLatestVersion(ctx, &http.Client{Timeout: 15 * time.Second})
	if err != nil {
		logger.Warnf("Couldn't check WhatsApp's current version: %v", err)
		return
	}
	if current := wastore.GetWAVersion(); newerVersion(current, *latest) {
		wastore.SetWAVersion(*latest)
		logger.Infof("Using WhatsApp version %s instead of %s", latest, current)
	}
}
