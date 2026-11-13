package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
)

const tinyWebP = "UklGRiIAAABXRUJQVlA4IBYAAAAwAQCdASoBAAEADsD+JaQAA3AAAAAA"

func solid(w, h int, c color.Color) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func pngBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegBytes(t *testing.T, img image.Image) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A real JPEG with an Exif block holding the given orientation and some other metadata.
func jpegWithExif(t *testing.T, img image.Image, orientation uint16, comment string) []byte {
	t.Helper()
	plain := jpegBytes(t, img)
	var tiff bytes.Buffer
	tiff.WriteString("II")
	binary.Write(&tiff, binary.LittleEndian, uint16(42))
	binary.Write(&tiff, binary.LittleEndian, uint32(8))
	binary.Write(&tiff, binary.LittleEndian, uint16(1))
	binary.Write(&tiff, binary.LittleEndian, uint16(0x0112))
	binary.Write(&tiff, binary.LittleEndian, uint16(3))
	binary.Write(&tiff, binary.LittleEndian, uint32(1))
	binary.Write(&tiff, binary.LittleEndian, orientation)
	binary.Write(&tiff, binary.LittleEndian, uint16(0))
	binary.Write(&tiff, binary.LittleEndian, uint32(0))
	tiff.WriteString(comment)
	payload := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(plain[:2])
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(plain[2:])
	return out.Bytes()
}

func decodeOut(t *testing.T, raw []byte) image.Image {
	t.Helper()
	if !bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("output isn't a JPEG: %x", raw[:4])
	}
	img, err := jpeg.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestPicturesAreReEncodedFromEachAllowedFormat(t *testing.T) {
	webpRaw, err := base64.StdEncoding.DecodeString(tinyWebP)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"png":  pngBytes(t, solid(900, 500, color.RGBA{200, 10, 10, 255})),
		"jpeg": jpegBytes(t, solid(500, 900, color.RGBA{10, 200, 10, 255})),
		"webp": webpRaw,
	} {
		out, err := cleanPicture(raw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		img := decodeOut(t, out)
		b := img.Bounds()
		if b.Dx() != b.Dy() || b.Dx() > pictureSize || b.Dx() == 0 {
			t.Errorf("%s: came out %dx%d", name, b.Dx(), b.Dy())
		}
	}
}

func TestAnythingElseIsRefusedByContentNotName(t *testing.T) {
	refused := map[string][]byte{
		"svg":        []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"><image href="file:///c:/secret"/></svg>`),
		"xml svg":    []byte(`<?xml version="1.0"?><svg/>`),
		"gif":        []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"html":       []byte("<html><script>alert(1)</script></html>"),
		"program":    []byte("MZ\x90\x00\x03\x00\x00\x00"),
		"empty":      {},
		"jpeg magic": append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("not really a jpeg at all")...),
		"png magic":  []byte("\x89PNG\r\n\x1a\nnot really a png"),
		"webp magic": []byte("RIFF\x10\x00\x00\x00WEBPVP8 garbage"),
		"cut off":    pngBytes(t, solid(50, 50, color.Black))[:60],
		"too big":    bytes.Repeat([]byte{0xFF}, maxPictureBytes+1),
	}
	for name, raw := range refused {
		if _, err := cleanPicture(raw); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// A tiny file claiming to be enormous must be refused before anything tries to decode it.
func TestDecompressionBombIsRefusedFromTheHeader(t *testing.T) {
	var buf bytes.Buffer
	buf.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], 60000)
	binary.BigEndian.PutUint32(ihdr[4:], 60000)
	ihdr[8], ihdr[9] = 8, 6
	chunk := append([]byte("IHDR"), ihdr...)
	binary.Write(&buf, binary.BigEndian, uint32(13))
	buf.Write(chunk)
	binary.Write(&buf, binary.BigEndian, crc32.ChecksumIEEE(chunk))
	start := time.Now()
	_, err := cleanPicture(buf.Bytes())
	if err == nil || !strings.Contains(err.Error(), "megapixels") {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("took too long, so it probably tried to decode it")
	}
}

func TestMetadataDoesNotSurvive(t *testing.T) {
	secret := "GPS-LATITUDE-SECRET-VALUE"
	out, err := cleanPicture(jpegWithExif(t, solid(100, 100, color.White), 1, secret))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(secret)) || bytes.Contains(out, []byte("Exif")) {
		t.Fatal("metadata came through")
	}
}

func TestSidewaysPhotosComeOutUpright(t *testing.T) {
	// Top half red. Stored rotated, with the flag saying how to turn it back.
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			if x < 50 { // Orientation 6 stores what should be the top on the left.
				img.Set(x, y, color.RGBA{255, 0, 0, 255})
			} else {
				img.Set(x, y, color.RGBA{0, 0, 255, 255})
			}
		}
	}
	out, err := cleanPicture(jpegWithExif(t, img, 6, ""))
	if err != nil {
		t.Fatal(err)
	}
	result := decodeOut(t, out)
	r, _, b, _ := result.At(50, 10).RGBA()
	if r < b {
		t.Fatalf("the top isn't red, so it wasn't turned: r=%d b=%d", r, b)
	}
}

func TestDamagedMetadataIsHarmless(t *testing.T) {
	for _, tiff := range [][]byte{nil, []byte("II"), []byte("XX\x00\x00\x00\x00\x00\x00"), []byte("II*\x00\xff\xff\xff\xff"), []byte("II*\x00\x08\x00\x00\x00\xff\xff")} {
		if got := exifOrientation(tiff); got != 1 {
			t.Errorf("%x: %d", tiff, got)
		}
	}
	for _, raw := range [][]byte{{0xFF, 0xD8}, {0xFF, 0xD8, 0xFF, 0xE1, 0xFF, 0xFF}, {0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x00}} {
		if got := jpegOrientation(raw); got != 1 {
			t.Errorf("%x: %d", raw, got)
		}
	}
}

type pictureHarness struct {
	*harness
	p *Pictures
}

const pictureChat = "15550000001@s.whatsapp.net"

func newPictureHarness(t *testing.T) *pictureHarness {
	t.Helper()
	h := newHarness(t)
	h.db.Exec(`INSERT INTO app_chat (jid, seq) VALUES (?, 1)`, pictureChat)
	p := h.app.pictures
	p.gap, p.ratePause, p.failPause = time.Millisecond, 200*time.Millisecond, 200*time.Millisecond
	p.fetch = func(ctx context.Context, rawURL string) ([]byte, error) {
		return pngBytes(t, solid(40, 40, color.RGBA{0, 0, 200, 255})), nil
	}
	return &pictureHarness{h, p}
}

func (f *fakeWA) setPicture(info *types.ProfilePictureInfo, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.picInfo, f.picErr = info, err
}

func (f *fakeWA) lookups() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.picCalls...)
}

func runPictures(t *testing.T, p *Pictures) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		p.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func waitFor(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestLookupFetchesOnDemandCachesAndAnnounces(t *testing.T) {
	h := newPictureHarness(t)
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "pic1", URL: "https://pps.whatsapp.net/x"}, nil)
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)
	runPictures(t, h.p)
	if got := h.wa.lookups(); len(got) != 0 {
		t.Fatalf("looked up before anything asked: %v", got)
	}
	resp, err := http.Get(h.srv.URL + "/api/app/picture?jid=" + pictureChat)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first ask: %d", resp.StatusCode)
	}
	waitFor(t, "the picture to arrive", func() bool { return len(h.p.Versions()) == 1 })
	event := string(<-sub)
	if !strings.Contains(event, `"type":"picture"`) || !strings.Contains(event, pictureChat) {
		t.Fatalf("event: %s", event)
	}
	for i := 0; i < 3; i++ {
		resp, err := http.Get(h.srv.URL + "/api/app/picture?jid=" + pictureChat)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/jpeg" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("second ask: %d %v", resp.StatusCode, resp.Header)
		}
		resp.Body.Close()
	}
	time.Sleep(30 * time.Millisecond)
	if got := h.wa.lookups(); len(got) != 1 {
		t.Fatalf("expected one lookup against WhatsApp, got %d", len(got))
	}
}

func TestOnlyKnownChatsAreEverLookedUp(t *testing.T) {
	h := newPictureHarness(t)
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "x", URL: "https://pps.whatsapp.net/x"}, nil)
	runPictures(t, h.p)
	resp, err := http.Get(h.srv.URL + "/api/app/picture?jid=19998887777@s.whatsapp.net")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	time.Sleep(50 * time.Millisecond)
	if got := h.wa.lookups(); len(got) != 0 {
		t.Fatalf("an arbitrary number was looked up: %v", got)
	}
	for _, bad := range []string{"status@broadcast", "1@newsletter", "not a jid", ""} {
		r, err := http.Get(h.srv.URL + "/api/app/picture?jid=" + bad)
		if err != nil {
			continue
		}
		r.Body.Close()
		if r.StatusCode != 400 {
			t.Errorf("%q: %d", bad, r.StatusCode)
		}
	}
}

func TestLookupsAreSpacedOut(t *testing.T) {
	h := newPictureHarness(t)
	h.p.gap = 40 * time.Millisecond
	h.wa.setPicture(nil, whatsmeow.ErrProfilePictureNotSet)
	for i := 0; i < 4; i++ {
		jid := string(rune('2'+i)) + "5550000001@s.whatsapp.net"
		h.db.Exec(`INSERT INTO app_chat (jid, seq) VALUES (?, ?)`, jid, i+2)
		h.p.Enqueue(jid)
	}
	start := time.Now()
	runPictures(t, h.p)
	waitFor(t, "all lookups", func() bool { return len(h.wa.lookups()) == 4 })
	if took := time.Since(start); took < 100*time.Millisecond {
		t.Fatalf("4 lookups took only %v", took)
	}
}

func TestTheDailyCapAndTheQueueCapHold(t *testing.T) {
	h := newPictureHarness(t)
	for i := 0; i < lookupQueueMax+25; i++ {
		h.p.Enqueue(string(rune('A'+i%26)) + strings.Repeat("x", i%7) + "@g.us" + string(rune('0'+i%10)))
	}
	if len(h.p.queue) > lookupQueueMax {
		t.Fatalf("queue grew to %d", len(h.p.queue))
	}
	h.p.queue, h.p.queued = nil, map[string]bool{}
	h.p.Enqueue(pictureChat)
	now := time.Now()
	h.p.dayStart, h.p.dayCount = now, lookupsPerDay
	jid, wait := h.p.nextLookup(now)
	if jid != "" || wait <= 0 {
		t.Fatalf("over the daily cap: %q %v", jid, wait)
	}
	jid, _ = h.p.nextLookup(now.Add(25 * time.Hour))
	if jid != pictureChat {
		t.Fatalf("the next day: %q", jid)
	}
}

func TestNewestRequestGoesFirst(t *testing.T) {
	h := newPictureHarness(t)
	h.p.Enqueue("1@g.us")
	h.p.Enqueue("2@g.us")
	jid, _ := h.p.nextLookup(time.Now())
	if jid != "2@g.us" {
		t.Fatalf("got %q", jid)
	}
}

func TestRateLimitPausesLookups(t *testing.T) {
	h := newPictureHarness(t)
	h.p.ratePause = time.Hour
	h.wa.setPicture(nil, whatsmeow.ErrIQRateOverLimit)
	h.p.lookup(context.Background(), pictureChat)
	h.p.Enqueue(pictureChat)
	jid, wait := h.p.nextLookup(time.Now())
	if jid != "" || wait < 50*time.Minute {
		t.Fatalf("didn't back off: %q %v", jid, wait)
	}
}

func TestPrivateOrMissingPictureIsRememberedAndFallsBack(t *testing.T) {
	for name, cause := range map[string]error{"hidden": whatsmeow.ErrProfilePictureUnauthorized, "none": whatsmeow.ErrProfilePictureNotSet} {
		h := newPictureHarness(t)
		h.wa.setPicture(nil, cause)
		h.p.lookup(context.Background(), pictureChat)
		data, _, needs := h.p.Read(pictureChat, time.Now())
		if data != nil || needs {
			t.Errorf("%s: asks again straight away: %v", name, needs)
		}
		if _, _, needs := h.p.Read(pictureChat, time.Now().Add(noPictureKeepFor+time.Hour)); !needs {
			t.Errorf("%s: never checks again", name)
		}
	}
}

func TestUnchangedPictureAvoidsADownload(t *testing.T) {
	h := newPictureHarness(t)
	fetched := 0
	var mu sync.Mutex
	original := h.p.fetch
	h.p.fetch = func(ctx context.Context, u string) ([]byte, error) {
		mu.Lock()
		fetched++
		mu.Unlock()
		return original(ctx, u)
	}
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "same", URL: "https://pps.whatsapp.net/x"}, nil)
	h.p.lookup(context.Background(), pictureChat)
	h.wa.setPicture(nil, nil) // What WhatsApp answers when the ID hasn't changed.
	h.p.lookup(context.Background(), pictureChat)
	if fetched != 1 || h.wa.lastExisting != "same" {
		t.Fatalf("fetched %d, existing %q", fetched, h.wa.lastExisting)
	}
	if data, _, _ := h.p.Read(pictureChat, time.Now()); data == nil {
		t.Fatal("lost the picture")
	}
}

func TestOwnPictureWinsAndRemovingGoesBack(t *testing.T) {
	h := newPictureHarness(t)
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "pic", URL: "https://pps.whatsapp.net/x"}, nil)
	h.p.lookup(context.Background(), pictureChat)
	whatsApp, v1, _ := h.p.Read(pictureChat, time.Now())
	sub := h.hub.Subscribe()
	defer h.hub.Unsubscribe(sub)

	resp, err := http.Post(h.srv.URL+"/api/app/picture/own?jid="+pictureChat, "application/octet-stream", bytes.NewReader(pngBytes(t, solid(64, 64, color.RGBA{255, 200, 0, 255}))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("set: %d", resp.StatusCode)
	}
	own, v2, needs := h.p.Read(pictureChat, time.Now().Add(365*24*time.Hour))
	if bytes.Equal(own, whatsApp) || v2 <= v1 {
		t.Fatal("the replacement isn't shown")
	}
	if needs {
		t.Fatal("no need to ask WhatsApp while a replacement is showing")
	}
	if !strings.Contains(string(<-sub), `"picture"`) {
		t.Fatal("other PCs weren't told")
	}

	req, _ := http.NewRequest(http.MethodDelete, h.srv.URL+"/api/app/picture/own?jid="+pictureChat, nil)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	back, v3, _ := h.p.Read(pictureChat, time.Now())
	if !bytes.Equal(back, whatsApp) || v3 <= v2 {
		t.Fatal("removing didn't go back to the WhatsApp picture")
	}
	if !strings.Contains(string(<-sub), `"picture"`) {
		t.Fatal("other PCs weren't told about the removal")
	}
}

func TestBadUploadsChangeNothing(t *testing.T) {
	h := newPictureHarness(t)
	resp, err := http.Post(h.srv.URL+"/api/app/picture/own?jid="+pictureChat, "image/png", strings.NewReader("<svg/>"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("svg: %d", resp.StatusCode)
	}
	resp, err = http.Post(h.srv.URL+"/api/app/picture/own?jid=19998887777@s.whatsapp.net", "image/png", bytes.NewReader(pngBytes(t, solid(4, 4, color.Black))))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Fatalf("unknown chat: %d", resp.StatusCode)
	}
	if len(h.p.Versions()) != 0 {
		t.Fatal("something was stored")
	}
}

func TestPicturesSurviveARestart(t *testing.T) {
	h := newPictureHarness(t)
	if _, err := h.p.SetOwn(pictureChat, pngBytes(t, solid(8, 8, color.Black))); err != nil {
		t.Fatal(err)
	}
	again, err := NewPictures(h.p.dir, h.wa, h.hub, h.app.knownChat)
	if err != nil {
		t.Fatal(err)
	}
	if data, _, _ := again.Read(pictureChat, time.Now()); data == nil {
		t.Fatal("lost after a restart")
	}
}

func TestADamagedIndexIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json"), 0o644)
	h := newPictureHarness(t)
	if _, err := NewPictures(dir, h.wa, h.hub, h.app.knownChat); err != nil {
		t.Fatal(err)
	}
}

func TestPicturesOnlyComeFromWhatsAppServers(t *testing.T) {
	for _, bad := range []string{"http://pps.whatsapp.net/x", "https://evil.example/x", "https://pps.whatsapp.net.evil.example/x", "https://evilwhatsapp.net/x", "file:///c:/secret", "https://127.0.0.1/x", "https://169.254.169.254/x"} {
		if _, err := fetchProfilePicture(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "unexpected") {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

func TestLookupFailureDoesNotLoseAPicture(t *testing.T) {
	h := newPictureHarness(t)
	h.wa.setPicture(&types.ProfilePictureInfo{ID: "a", URL: "https://pps.whatsapp.net/x"}, nil)
	h.p.lookup(context.Background(), pictureChat)
	h.wa.setPicture(nil, errors.New("network trouble"))
	h.p.lookup(context.Background(), pictureChat)
	if data, _, _ := h.p.Read(pictureChat, time.Now()); data == nil {
		t.Fatal("a network error wiped the picture")
	}
}

// The relay's project-code rule reads these names. A new group and a rename, with made-up data.
func TestGroupNamesFollowNewGroupsAndRenames(t *testing.T) {
	h := newHarness(t)
	names := func() map[string]string {
		var body struct {
			Chats []struct{ JID, Name string } `json:"chats"`
		}
		if status := h.getJSON("/api/app/group-names", &body); status != 200 {
			t.Fatalf("status %d", status)
		}
		out := map[string]string{}
		for _, c := range body.Chats {
			out[c.JID] = c.Name
		}
		return out
	}
	group := types.NewJID("120363000000000042", types.GroupServer)
	// A chat's row appears with its first message. A name can arrive before or after that.
	if _, err := h.db.Exec(`INSERT INTO chats (jid, name) VALUES (?, '')`, group.String()); err != nil {
		t.Fatal(err)
	}
	h.app.handleEvent(&events.JoinedGroup{GroupInfo: types.GroupInfo{JID: group, GroupName: types.GroupName{Name: "NEW-2601 Made up"}}})
	if got := names()[group.String()]; got != "NEW-2601 Made up" {
		t.Fatalf("new group: %q", got)
	}
	h.app.handleEvent(&events.GroupInfo{JID: group, Name: &types.GroupName{Name: "SOL-2699 Renamed"}})
	if got := names()[group.String()]; got != "SOL-2699 Renamed" {
		t.Fatalf("rename: %q", got)
	}
	// One-to-one chats are never listed, however they're named.
	h.db.Exec(`INSERT INTO chats (jid, name) VALUES ('15550000001@s.whatsapp.net', 'ABC-1234 A person')`)
	if _, listed := names()["15550000001@s.whatsapp.net"]; listed {
		t.Fatal("a person was listed")
	}
}
