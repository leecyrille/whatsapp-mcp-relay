package main

import (
	"bytes"
	"context"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
)

// getFull asks for the big copy and returns the status, version header and decoded width.
func (h *pictureHarness) getFull(t *testing.T, jid string) (int, string, int) {
	t.Helper()
	resp, err := http.Get(h.srv.URL + "/api/app/picture/full?jid=" + jid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return resp.StatusCode, "", 0
	}
	if resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers: %v", resp.Header)
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("not a JPEG: %v", err)
	}
	return 200, resp.Header.Get("X-Picture-Version"), img.Bounds().Dx()
}

func (h *pictureHarness) smallVersion(t *testing.T, jid string) string {
	t.Helper()
	resp, err := http.Get(h.srv.URL + "/api/app/picture?jid=" + jid)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.Header.Get("X-Picture-Version")
}

func TestFullPictureIsKeptFromTheSameDownload(t *testing.T) {
	h := newPictureHarness(t)
	fetched := 0
	var mu sync.Mutex
	h.p.fetch = func(ctx context.Context, u string) ([]byte, error) {
		mu.Lock()
		fetched++
		mu.Unlock()
		return pngBytes(t, solid(800, 600, color.RGBA{0, 120, 0, 255})), nil
	}
	if status, _, _ := h.getFull(t, pictureChat); status != http.StatusNoContent {
		t.Fatalf("before any lookup: %d", status)
	}
	if got := h.wa.lookups(); len(got) != 0 {
		t.Fatalf("asking for the big copy looked up WhatsApp: %v", got)
	}
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "p1", URL: "https://pps.whatsapp.net/x"}, nil)
	h.p.lookup(context.Background(), pictureChat)
	status, version, width := h.getFull(t, pictureChat)
	if status != 200 || width != 600 {
		t.Fatalf("big copy: %d, %d px", status, width)
	}
	if small := h.smallVersion(t, pictureChat); small != version || version == "" {
		t.Fatalf("versions differ: small %q, full %q", small, version)
	}
	if fetched != 1 || len(h.wa.lookups()) != 1 {
		t.Fatalf("expected one lookup and one download, got %d and %d", len(h.wa.lookups()), fetched)
	}
}

func TestFullPictureIsCappedAndRemovedWithTheSmallOne(t *testing.T) {
	h := newPictureHarness(t)
	h.p.fetch = func(ctx context.Context, u string) ([]byte, error) {
		return pngBytes(t, solid(2000, 2000, color.RGBA{9, 9, 9, 255})), nil
	}
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "big", URL: "https://pps.whatsapp.net/x"}, nil)
	h.p.lookup(context.Background(), pictureChat)
	if _, _, width := h.getFull(t, pictureChat); width != fullPictureSize {
		t.Fatalf("not capped: %d px", width)
	}
	h.wa.setPicture(nil, whatsmeow.ErrProfilePictureNotSet)
	h.p.lookup(context.Background(), pictureChat)
	if status, _, _ := h.getFull(t, pictureChat); status != http.StatusNoContent {
		t.Fatalf("after the picture was removed: %d", status)
	}
	if _, err := os.Stat(h.p.file(pictureChat, "wa-full")); !os.IsNotExist(err) {
		t.Fatalf("big copy left on disk: %v", err)
	}
}

func TestOldPictureGetsItsBigCopyThroughTheNormalQueue(t *testing.T) {
	h := newPictureHarness(t)
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "old", URL: "https://pps.whatsapp.net/x"}, nil)
	h.p.lookup(context.Background(), pictureChat)
	// As if saved by an older bridge: the small copy only.
	if err := os.Remove(h.p.file(pictureChat, "wa-full")); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := h.getFull(t, pictureChat); status != http.StatusNoContent {
		t.Fatalf("missing big copy: %d", status)
	}
	if len(h.p.queue) != 1 || h.p.queue[0] != pictureChat {
		t.Fatalf("not queued: %v", h.p.queue)
	}
	h.p.lookup(context.Background(), pictureChat)
	if h.wa.lastExisting != "" {
		t.Fatalf("asked WhatsApp only for changes, so the big copy could never arrive: %q", h.wa.lastExisting)
	}
	if status, _, _ := h.getFull(t, pictureChat); status != 200 {
		t.Fatalf("after the queued lookup: %d", status)
	}
	// Once both copies exist, lookups only ask for changes again.
	h.wa.setPicture(nil, nil)
	h.p.lookup(context.Background(), pictureChat)
	if h.wa.lastExisting != "old" {
		t.Fatalf("existing ID not sent: %q", h.wa.lastExisting)
	}
}

func TestFullPictureForUnknownChatsNeverLooksUp(t *testing.T) {
	h := newPictureHarness(t)
	if status, _, _ := h.getFull(t, "19998887777@s.whatsapp.net"); status != http.StatusNoContent {
		t.Fatalf("unknown chat: %d", status)
	}
	if len(h.p.queue) != 0 || len(h.wa.lookups()) != 0 {
		t.Fatal("an unknown number was queued")
	}
	for _, bad := range []string{"status@broadcast", "not a jid"} {
		if status, _, _ := h.getFull(t, bad); status != 400 {
			t.Errorf("%q: %d", bad, status)
		}
	}
}

func TestOwnPictureServesItsBigCopy(t *testing.T) {
	h := newPictureHarness(t)
	resp, err := http.Post(h.srv.URL+"/api/app/picture/own?jid="+pictureChat, "application/octet-stream", bytes.NewReader(pngBytes(t, solid(700, 700, color.RGBA{200, 0, 0, 255}))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	status, version, width := h.getFull(t, pictureChat)
	if status != 200 || width != 700 || version != h.smallVersion(t, pictureChat) {
		t.Fatalf("own big copy: %d %d px v%s", status, width, version)
	}
	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+"/api/app/picture/own?jid="+pictureChat, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if _, err := os.Stat(h.p.file(pictureChat, "own-full")); !os.IsNotExist(err) {
		t.Fatalf("own big copy left on disk: %v", err)
	}
}
