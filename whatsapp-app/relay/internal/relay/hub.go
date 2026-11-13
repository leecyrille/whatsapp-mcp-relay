package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Hub fans events out to clients. A client that falls behind is disconnected rather than waited for;
// it reconnects and catches up by sequence number.
type Hub struct {
	mu   sync.Mutex
	subs map[chan []byte]struct{}
}

// NewHub makes an empty hub.
func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

// Subscribe adds a client.
func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, 1024)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe removes a client.
func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
}

// Publish encodes and sends an event.
func (h *Hub) Publish(v any) {
	if data, err := json.Marshal(v); err == nil {
		h.PublishRaw(data)
	}
}

// PublishRaw sends an already encoded event.
func (h *Hub) PublishRaw(data []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- data:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// EnsureToken reads the shared secret between relay and runner, creating it on first use. Whichever
// starts first creates it; O_EXCL makes that safe if they start together.
func EnsureToken(path string) (string, error) {
	for i := 0; i < 20; i++ {
		raw, err := os.ReadFile(path)
		if err == nil {
			if token := strings.TrimSpace(string(raw)); len(token) >= 32 {
				return token, nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			continue // The other process just created it; read it on the next pass.
		}
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			f.Close()
			return "", err
		}
		token := hex.EncodeToString(buf)
		_, err = f.WriteString(token)
		f.Close()
		if err != nil {
			return "", err
		}
		return token, nil
	}
	return "", errors.New("couldn't create or read the token file")
}
