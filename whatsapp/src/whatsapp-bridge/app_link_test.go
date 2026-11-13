package main

import (
	"testing"
	"time"

	wastore "go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types/events"
)

func TestLinkProblemFollowsEvents(t *testing.T) {
	l := &linkHealth{}
	if l.Problem() != "" {
		t.Fatal("starts with a problem")
	}
	for evt, want := range map[any]string{
		&events.LoggedOut{}:      linkLoggedOut,
		&events.StreamReplaced{}: linkStreamReplaced,
		&events.TemporaryBan{}:   linkTemporaryBan,
		&events.ClientOutdated{}: linkClientOutdated,
	} {
		l.note(evt)
		if l.Problem() != want {
			t.Fatalf("%T: got %q", evt, l.Problem())
		}
		l.note(&events.Connected{})
		if l.Problem() != "" {
			t.Fatalf("%T: not cleared by a connection", evt)
		}
	}
	// A short network drop isn't a problem to report by itself.
	l.note(&events.Disconnected{})
	if l.Problem() != "" {
		t.Fatal("a plain disconnect counted")
	}
}

func TestOutdatedRetriesOnceAtATime(t *testing.T) {
	ran := make(chan struct{}, 4)
	release := make(chan struct{})
	l := &linkHealth{reconnect: func() {
		ran <- struct{}{}
		<-release
	}}
	if !l.note(&events.ClientOutdated{}) {
		t.Fatal("no retry scheduled")
	}
	<-ran
	if l.note(&events.ClientOutdated{}) {
		t.Fatal("a second retry started during the first")
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		l.mu.Lock()
		done := !l.retrying
		l.mu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("retry never finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !l.note(&events.ClientOutdated{}) {
		t.Fatal("can't retry again after the first finished")
	}
}

func TestOnlyOutdatedRetries(t *testing.T) {
	called := make(chan struct{}, 4)
	l := &linkHealth{reconnect: func() { called <- struct{}{} }}
	// Reconnecting after a logout or a ban, or when another client took over, must be left to the user.
	for _, evt := range []any{&events.LoggedOut{}, &events.StreamReplaced{}, &events.TemporaryBan{}, &events.Disconnected{}} {
		if l.note(evt) {
			t.Fatalf("%T scheduled a retry", evt)
		}
	}
	select {
	case <-called:
		t.Fatal("reconnected")
	case <-time.After(20 * time.Millisecond):
	}
}

func TestNewerVersion(t *testing.T) {
	cur := wastore.WAVersionContainer{2, 3000, 100}
	for latest, want := range map[wastore.WAVersionContainer]bool{
		{2, 3000, 101}: true,
		{2, 3001, 1}:   true,
		{3, 0, 0}:      true,
		{2, 3000, 100}: false,
		{2, 3000, 99}:  false,
		{2, 2999, 500}: false,
		{0, 0, 0}:      false,
	} {
		if newerVersion(cur, latest) != want {
			t.Errorf("%v: want %v", latest, want)
		}
	}
}
