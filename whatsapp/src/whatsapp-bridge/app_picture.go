package main

// Pictures for the app: people's WhatsApp profile pictures, and the user's own replacements. Both live here on
// the home PC, so every PC shows the same ones. Nothing in this file changes anything on WhatsApp, including
// The user's own profile picture: it only reads profile pictures, and their replacements stay in this folder. Changing
// a group's real photo is separate, and only in app_group_photo.go.
//
// Bulk lookups against WhatsApp add ban risk, so a picture is only looked up when an app actually shows
// that chat, one at a time, with a long wait in between, and the result is kept for days.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	stddraw "image/draw"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types"
	"golang.org/x/image/draw"
	"golang.org/x/image/webp"
)

const (
	maxPictureBytes  = 10 << 20
	maxPicturePixels = 20_000_000
	pictureSize      = 256
	pictureQuality   = 85

	lookupGap        = 5 * time.Second // At most 12 a minute.
	lookupsPerDay    = 300
	lookupQueueMax   = 40
	pictureKeepFor   = 7 * 24 * time.Hour
	noPictureKeepFor = 3 * 24 * time.Hour
	lookupRatePause  = time.Hour // WhatsApp said to slow down.
	lookupFailPause  = 5 * time.Minute
)

var errNotAnImage = errors.New("that isn't a JPEG, PNG or WebP image")

// cleanPicture turns untrusted bytes into a small, square JPEG of our own, so nothing from the original
// file, including any metadata, gets any further. The format comes from the content, never a file name.
func cleanPicture(raw []byte) ([]byte, error) {
	return cleanPictureSized(raw, pictureSize)
}

func cleanPictureSized(raw []byte, size int) ([]byte, error) {
	if len(raw) == 0 || len(raw) > maxPictureBytes {
		return nil, fmt.Errorf("the image must be under %d MB", maxPictureBytes>>20)
	}
	var decode func(io.Reader) (image.Image, error)
	var config func(io.Reader) (image.Config, error)
	orientation := 1
	switch {
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}):
		decode, config = jpeg.Decode, jpeg.DecodeConfig
		orientation = jpegOrientation(raw)
	case bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")):
		decode, config = png.Decode, png.DecodeConfig
	case len(raw) > 12 && string(raw[:4]) == "RIFF" && string(raw[8:12]) == "WEBP":
		decode, config = webp.Decode, webp.DecodeConfig
	default:
		return nil, errNotAnImage
	}
	// The size in the header is checked first, so a small file can't expand into a huge one.
	info, err := safely(func() (image.Config, error) { return config(bytes.NewReader(raw)) })
	if err != nil {
		return nil, errNotAnImage
	}
	if info.Width <= 0 || info.Height <= 0 || int64(info.Width)*int64(info.Height) > maxPicturePixels {
		return nil, fmt.Errorf("the image must be under %d megapixels", maxPicturePixels/1_000_000)
	}
	img, err := safely(func() (image.Image, error) { return decode(bytes.NewReader(raw)) })
	if err != nil {
		return nil, errNotAnImage
	}
	return encodePicture(upright(img, orientation), size)
}

// safely turns a decoder panic on a damaged file into an ordinary error.
func safely[T any](fn func() (T, error)) (out T, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = errNotAnImage
		}
	}()
	return fn()
}

func encodePicture(img image.Image, size int) ([]byte, error) {
	bounds := img.Bounds()
	side := min(bounds.Dx(), bounds.Dy())
	x0 := bounds.Min.X + (bounds.Dx()-side)/2
	y0 := bounds.Min.Y + (bounds.Dy()-side)/2
	out := min(side, size)
	dst := image.NewRGBA(image.Rect(0, 0, out, out))
	stddraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, image.Rect(x0, y0, x0+side, y0+side), draw.Over, nil)
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: pictureQuality}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// jpegOrientation finds how a photo was turned when it was taken, so phone photos aren't shown sideways.
// It reads only one value from untrusted metadata, with every offset checked, and defaults to upright.
func jpegOrientation(raw []byte) int {
	for i := 2; i+4 <= len(raw) && raw[i] == 0xFF; {
		marker := raw[i+1]
		size := int(binary.BigEndian.Uint16(raw[i+2:]))
		if marker == 0xDA || size < 2 || i+2+size > len(raw) {
			return 1
		}
		if marker == 0xE1 && size >= 8 && string(raw[i+4:i+10]) == "Exif\x00\x00" {
			return exifOrientation(raw[i+10 : i+2+size])
		}
		i += 2 + size
	}
	return 1
}

func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	ifd := int64(order.Uint32(tiff[4:]))
	if ifd+2 > int64(len(tiff)) {
		return 1
	}
	count := int64(order.Uint16(tiff[ifd:]))
	for n := int64(0); n < count; n++ {
		at := ifd + 2 + n*12
		if at+12 > int64(len(tiff)) {
			return 1
		}
		if order.Uint16(tiff[at:]) == 0x0112 {
			return int(order.Uint16(tiff[at+8:]))
		}
	}
	return 1
}

func upright(img image.Image, orientation int) image.Image {
	if orientation != 3 && orientation != 6 && orientation != 8 {
		return img
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	var dst *image.RGBA
	if orientation == 3 {
		dst = image.NewRGBA(image.Rect(0, 0, w, h))
	} else {
		dst = image.NewRGBA(image.Rect(0, 0, h, w))
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := img.At(b.Min.X+x, b.Min.Y+y)
			switch orientation {
			case 3:
				dst.Set(w-1-x, h-1-y, c)
			case 6:
				dst.Set(h-1-y, x, c)
			case 8:
				dst.Set(y, w-1-x, c)
			}
		}
	}
	return dst
}

// pictureEntry is what's known about one chat's picture.
type pictureEntry struct {
	Version int64  `json:"v"`
	Own     bool   `json:"own,omitempty"`
	WA      bool   `json:"wa,omitempty"`
	WAID    string `json:"wa_id,omitempty"`
	Checked int64  `json:"checked,omitempty"`
}

// Pictures is the store and the lookup queue.
type Pictures struct {
	dir   string
	wa    waAPI
	hub   *Hub
	fetch func(ctx context.Context, rawURL string) ([]byte, error)
	known func(ctx context.Context, jid string) bool

	gap, ratePause, failPause time.Duration

	mu         sync.Mutex
	entries    map[string]*pictureEntry
	queue      []string
	queued     map[string]bool
	wake       chan struct{}
	pauseUntil time.Time
	dayStart   time.Time
	dayCount   int
	lastChange int64
	groupInfos map[string]keptInfo
}

type keptInfo struct {
	info *types.GroupInfo
	err  error
	at   time.Time
}

// NewPictures opens the folder. A damaged index only costs fresh lookups, since the pictures can be fetched
// again; the user's own pictures are recovered from their files.
func NewPictures(dir string, wa waAPI, hub *Hub, known func(context.Context, string) bool) (*Pictures, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := &Pictures{
		dir: dir, wa: wa, hub: hub, fetch: fetchProfilePicture, known: known,
		gap: lookupGap, ratePause: lookupRatePause, failPause: lookupFailPause,
		entries: map[string]*pictureEntry{}, queued: map[string]bool{}, wake: make(chan struct{}, 1), groupInfos: map[string]keptInfo{},
	}
	if raw, err := os.ReadFile(p.indexPath()); err == nil {
		_ = json.Unmarshal(raw, &p.entries)
	}
	for jid, entry := range p.entries {
		if entry == nil {
			delete(p.entries, jid)
		}
	}
	return p, nil
}

func (p *Pictures) indexPath() string { return filepath.Join(p.dir, "index.json") }

func (p *Pictures) file(jid, kind string) string {
	sum := sha256.Sum256([]byte(jid))
	return filepath.Join(p.dir, hex.EncodeToString(sum[:16])+"."+kind+".jpg")
}

func (p *Pictures) saveLocked() {
	raw, err := json.MarshalIndent(p.entries, "", " ")
	if err != nil {
		return
	}
	tmp := p.indexPath() + ".tmp"
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, p.indexPath())
	}
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// nextVersionLocked always moves forward, even for two changes in the same millisecond.
func (p *Pictures) nextVersionLocked() int64 {
	v := time.Now().UnixMilli()
	if v <= p.lastChange {
		v = p.lastChange + 1
	}
	p.lastChange = v
	return v
}

func (p *Pictures) entryLocked(jid string) *pictureEntry {
	entry := p.entries[jid]
	if entry == nil {
		entry = &pictureEntry{}
		p.entries[jid] = entry
	}
	return entry
}

func (p *Pictures) publish(jid string, version int64) {
	p.mu.Lock()
	entry := p.entries[jid]
	own := entry != nil && entry.Own
	p.mu.Unlock()
	p.hub.Publish(map[string]any{"type": "picture", "jid": jid, "v": version, "own": own})
}

// Versions lists every chat with a picture, so an app that was offline can catch up.
func (p *Pictures) Versions() map[string]int64 {
	versions, _ := p.list()
	return versions
}

func (p *Pictures) list() (map[string]int64, []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	versions := map[string]int64{}
	own := []string{}
	for jid, entry := range p.entries {
		if entry.Own || entry.WA {
			versions[jid] = entry.Version
		}
		if entry.Own {
			own = append(own, jid)
		}
	}
	return versions, own
}

// SetOwn stores the user's replacement. Only this folder changes.
func (p *Pictures) SetOwn(jid string, raw []byte) (int64, error) {
	clean, err := cleanPicture(raw)
	if err != nil {
		return 0, err
	}
	p.mu.Lock()
	if err := writeFileAtomic(p.file(jid, "own"), clean); err != nil {
		p.mu.Unlock()
		return 0, err
	}
	entry := p.entryLocked(jid)
	entry.Own = true
	entry.Version = p.nextVersionLocked()
	version := entry.Version
	p.saveLocked()
	p.mu.Unlock()
	p.publish(jid, version)
	return version, nil
}

// RemoveOwn goes back to the WhatsApp picture.
func (p *Pictures) RemoveOwn(jid string) int64 {
	p.mu.Lock()
	entry := p.entries[jid]
	if entry == nil || !entry.Own {
		p.mu.Unlock()
		return 0
	}
	_ = os.Remove(p.file(jid, "own"))
	entry.Own = false
	entry.Version = p.nextVersionLocked()
	version := entry.Version
	p.saveLocked()
	p.mu.Unlock()
	p.publish(jid, version)
	return version
}

// Read returns the picture to show. needsLookup says a lookup should be queued, either because there's
// nothing yet or because what's here is old.
func (p *Pictures) Read(jid string, now time.Time) (data []byte, version int64, needsLookup bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[jid]
	if entry == nil {
		return nil, 0, true
	}
	checked := time.Unix(entry.Checked, 0)
	stale := entry.Checked == 0 || now.Sub(checked) > noPictureKeepFor
	if entry.WA {
		stale = now.Sub(checked) > pictureKeepFor
	}
	kind := ""
	switch {
	case entry.Own:
		kind = "own"
		stale = false // WhatsApp's picture isn't shown, so there's no reason to ask.
	case entry.WA:
		kind = "wa"
	default:
		return nil, entry.Version, stale
	}
	raw, err := os.ReadFile(p.file(jid, kind))
	if err != nil {
		return nil, entry.Version, !entry.Own
	}
	return raw, entry.Version, stale
}

// Enqueue asks for a lookup. The newest request goes first, because that's what's on screen now, and old
// requests fall off rather than building up.
func (p *Pictures) Enqueue(jid string) {
	p.mu.Lock()
	if p.queued[jid] {
		p.mu.Unlock()
		return
	}
	p.queued[jid] = true
	p.queue = append(p.queue, jid)
	if len(p.queue) > lookupQueueMax {
		delete(p.queued, p.queue[0])
		p.queue = p.queue[1:]
	}
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// waitTime says how long until another lookup is allowed, and counts it if the answer is now.
func (p *Pictures) nextLookup(now time.Time) (string, time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.queue) == 0 {
		return "", 0
	}
	if now.Before(p.pauseUntil) {
		return "", p.pauseUntil.Sub(now)
	}
	if p.dayStart.IsZero() || now.Sub(p.dayStart) >= 24*time.Hour {
		p.dayStart, p.dayCount = now, 0
	}
	if p.dayCount >= lookupsPerDay {
		return "", p.dayStart.Add(24 * time.Hour).Sub(now)
	}
	p.dayCount++
	last := len(p.queue) - 1
	jid := p.queue[last]
	p.queue = p.queue[:last]
	delete(p.queued, jid)
	return jid, 0
}

// Run works through lookups until the context ends.
func (p *Pictures) Run(ctx context.Context) {
	for ctx.Err() == nil {
		if !p.wa.Connected() {
			if !sleepCtx(ctx, 30*time.Second) {
				return
			}
			continue
		}
		jid, wait := p.nextLookup(time.Now())
		if jid == "" {
			if wait > 0 {
				if !sleepCtx(ctx, wait) {
					return
				}
				continue
			}
			select {
			case <-ctx.Done():
				return
			case <-p.wake:
			}
			continue
		}
		p.lookup(ctx, jid)
		if !sleepCtx(ctx, p.gap) {
			return
		}
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// lookup asks WhatsApp for one picture. Reading a profile picture doesn't tell the other person.
func (p *Pictures) lookup(ctx context.Context, jid string) {
	parsed, err := parseChatJID(jid)
	if err != nil || !p.known(ctx, jid) {
		return
	}
	p.mu.Lock()
	existing := ""
	if entry := p.entries[jid]; entry != nil && entry.WA {
		existing = entry.WAID
	}
	p.mu.Unlock()
	callCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	info, err := p.wa.ProfilePicture(callCtx, parsed, existing)
	switch {
	case errors.Is(err, whatsmeow.ErrProfilePictureNotSet), errors.Is(err, whatsmeow.ErrProfilePictureUnauthorized):
		p.storeNone(jid)
		return
	case errors.Is(err, whatsmeow.ErrIQRateOverLimit):
		p.pause(p.ratePause)
		return
	case err != nil:
		p.pause(p.failPause)
		return
	}
	if info == nil { // Unchanged since last time.
		p.mu.Lock()
		p.entryLocked(jid).Checked = time.Now().Unix()
		p.saveLocked()
		p.mu.Unlock()
		return
	}
	raw, err := p.fetch(callCtx, info.URL)
	if err != nil {
		p.pause(p.failPause)
		return
	}
	clean, err := cleanPicture(raw)
	if err != nil {
		p.storeNone(jid) // A picture that won't decode is treated as no picture, and retried in a few days.
		return
	}
	p.mu.Lock()
	if err := writeFileAtomic(p.file(jid, "wa"), clean); err != nil {
		p.mu.Unlock()
		return
	}
	entry := p.entryLocked(jid)
	entry.WA, entry.WAID, entry.Checked = true, info.ID, time.Now().Unix()
	entry.Version = p.nextVersionLocked()
	version, own := entry.Version, entry.Own
	p.saveLocked()
	p.mu.Unlock()
	if !own {
		p.publish(jid, version)
	}
}

func (p *Pictures) storeNone(jid string) {
	p.mu.Lock()
	entry := p.entryLocked(jid)
	removed := entry.WA
	if removed {
		_ = os.Remove(p.file(jid, "wa"))
		entry.Version = p.nextVersionLocked()
	}
	entry.WA, entry.WAID, entry.Checked = false, "", time.Now().Unix()
	version, own := entry.Version, entry.Own
	p.saveLocked()
	p.mu.Unlock()
	if removed && !own {
		p.publish(jid, version)
	}
}

func (p *Pictures) pause(d time.Duration) {
	p.mu.Lock()
	p.pauseUntil = time.Now().Add(d)
	p.mu.Unlock()
}

// fetchProfilePicture downloads from WhatsApp's own servers and nowhere else, since the address comes
// from over the network.
func fetchProfilePicture(ctx context.Context, rawURL string) ([]byte, error) {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if parsed.Scheme != "https" || !pictureHostAllowed(parsed.Hostname()) {
		return nil, errors.New("unexpected picture address")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPictureBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxPictureBytes {
		return nil, errors.New("picture too large")
	}
	return raw, nil
}

func pictureHostAllowed(host string) bool {
	host = strings.ToLower(host)
	return strings.HasSuffix(host, ".whatsapp.net") || strings.HasSuffix(host, ".cdninstagram.com") || strings.HasSuffix(host, ".fbcdn.net")
}

func (a *App) pictureJID(w http.ResponseWriter, r *http.Request) (string, bool) {
	parsed, err := parseChatJID(r.URL.Query().Get("jid"))
	if err != nil {
		fail(w, 400, "bad address")
		return "", false
	}
	return a.wa.Canon(parsed.String()), true
}

// knownChat limits lookups and replacements to the user's own chats and contacts, so this can't be used to
// look up arbitrary numbers on WhatsApp.
func (a *App) knownChat(ctx context.Context, jid string) bool {
	if a.knownNamed(ctx, jid) {
		return true
	}
	var found int
	err := a.store.db.QueryRowContext(ctx, `SELECT 1 FROM app_chat WHERE jid = ? LIMIT 1`, jid).Scan(&found)
	if err == nil {
		return true
	}
	parsed, err := types.ParseJID(jid)
	if err != nil {
		return false
	}
	contacts, err := a.wa.Contacts(ctx)
	if err != nil {
		return false
	}
	if _, ok := contacts[parsed]; ok {
		return true
	}
	aliases, err := a.wa.Aliases(ctx)
	if err != nil {
		return false
	}
	for _, pair := range aliases {
		if pair[1] != jid {
			continue
		}
		if a.store.db.QueryRowContext(ctx, `SELECT 1 FROM app_chat WHERE jid = ? LIMIT 1`, pair[0]).Scan(&found) == nil {
			return true
		}
	}
	return false
}

func (a *App) handlePictureList(w http.ResponseWriter, r *http.Request) {
	versions, own := a.pictures.list()
	writeJSON(w, 200, map[string]any{"pictures": versions, "own": own})
}

func (a *App) handlePicture(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.pictureJID(w, r)
	if !ok {
		return
	}
	data, version, needsLookup := a.pictures.Read(jid, time.Now())
	if needsLookup && a.knownChat(r.Context(), jid) {
		a.pictures.Enqueue(jid)
	}
	if data == nil {
		if needsLookup {
			writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "state": "looking"})
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Picture-Version", fmt.Sprint(version))
	_, _ = w.Write(data)
}

func (a *App) handlePictureSet(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.pictureJID(w, r)
	if !ok {
		return
	}
	if !a.knownChat(r.Context(), jid) {
		fail(w, 404, "That isn't one of your chats or contacts.")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPictureBytes+1))
	if err != nil {
		fail(w, 413, "The image must be under 10 MB.")
		return
	}
	version, err := a.pictures.SetOwn(jid, raw)
	if err != nil {
		fail(w, 400, "Couldn't use that image: "+err.Error()+".")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "jid": jid, "v": version})
}

func (a *App) handlePictureRemove(w http.ResponseWriter, r *http.Request) {
	jid, ok := a.pictureJID(w, r)
	if !ok {
		return
	}
	version := a.pictures.RemoveOwn(jid)
	writeJSON(w, 200, map[string]any{"ok": true, "jid": jid, "v": version, "changed": version != 0})
}
