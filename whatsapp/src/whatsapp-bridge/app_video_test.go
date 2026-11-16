package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// boxOrder lists an MP4's top-level boxes.
func boxOrder(b []byte) []string {
	var out []string
	for i := 0; i+8 <= len(b); {
		size := uint64(binary.BigEndian.Uint32(b[i:]))
		kind := string(b[i+4 : i+8])
		if size == 1 && i+16 <= len(b) {
			size = binary.BigEndian.Uint64(b[i+8:])
		}
		out = append(out, kind)
		if size < 8 {
			break
		}
		i += int(size)
	}
	return out
}

func indexFirst(b []byte) bool {
	boxes := strings.Join(boxOrder(b), " ")
	m, d := strings.Index(boxes, "moov"), strings.Index(boxes, "mdat")
	return m >= 0 && d >= 0 && m < d
}

// makeClip renders a short synthetic test pattern with a tone. ffmpeg leaves the index at the end
// unless asked otherwise, like many downloaded videos.
func makeClip(t *testing.T, name string, codec ...string) string {
	t.Helper()
	exe := ffmpegPath()
	if exe == "" {
		t.Skip("ffmpeg not installed")
	}
	path := filepath.Join(t.TempDir(), name)
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=25:duration=3",
		"-f", "lavfi", "-i", "sine=frequency=440:duration=3", "-shortest"}
	args = append(args, codec...)
	args = append(args, path)
	if out, err := exec.Command(exe, args...).CombinedOutput(); err != nil {
		t.Fatalf("making the test clip: %v %s", err, out)
	}
	return path
}

func TestParseProbeReadsStreamsAndRotation(t *testing.T) {
	out := `  Duration: 00:00:45.91, start: 0.000000, bitrate: 863 kb/s
  Stream #0:0[0x1](und): Video: h264 (High) (avc1 / 0x31637661), yuv420p(tv, bt709, progressive), 460x458 [SAR 1:1 DAR 230:229], 539 kb/s, 29.10 fps
  Stream #0:1[0x2](und): Audio: aac (LC) (mp4a / 0x6134706D), 48000 Hz, stereo, fltp, 319 kb/s (default)`
	info, ok := parseProbe(out)
	if !ok || info.Video != "h264" || info.PixFmt != "yuv420p" || info.Audio != "aac" || info.Width != 460 || info.Height != 458 || info.Seconds != 46 {
		t.Fatalf("%+v %v", info, ok)
	}
	if !info.canCopy() {
		t.Fatal("H.264 + AAC should only need the index moved")
	}
	phone := `  Duration: 00:00:02.10, start: 0.000000
  Stream #0:0[0x1](und): Video: hevc (Main) (hvc1 / 0x31637668), yuv420p(tv, bt709), 1920x1080, 9000 kb/s, 30 fps
      Side data:
        displaymatrix: rotation of -90.00 degrees`
	info, _ = parseProbe(phone)
	if info.Width != 1080 || info.Height != 1920 || info.canCopy() {
		t.Fatalf("rotated HEVC: %+v", info)
	}
	if _, ok := parseProbe("Stream #0:0: Audio: mp3, 44100 Hz"); ok {
		t.Fatal("audio-only file read as video")
	}
}

func TestVideoWithIndexAtTheEndIsRewrittenWithoutReencoding(t *testing.T) {
	in := makeClip(t, "late-index.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac")
	raw, _ := os.ReadFile(in)
	if indexFirst(raw) {
		t.Fatal("test clip already has its index first")
	}
	out, info := prepareVideo(in)
	if out == in {
		t.Fatal("not rewritten")
	}
	defer os.Remove(out)
	fixed, _ := os.ReadFile(out)
	if !indexFirst(fixed) {
		t.Fatalf("index still after the data: %v", boxOrder(fixed))
	}
	if info.Video != "h264" || info.Audio != "aac" || info.Width != 320 || info.Height != 240 || info.Seconds != 3 {
		t.Fatalf("%+v", info)
	}
}

func TestOtherCodecsAreReencodedToH264AndAAC(t *testing.T) {
	in := makeClip(t, "mpeg4.mp4", "-c:v", "mpeg4", "-c:a", "libmp3lame")
	out, info := prepareVideo(in)
	if out == in {
		t.Fatal("not converted")
	}
	defer os.Remove(out)
	fixed, _ := os.ReadFile(out)
	if !indexFirst(fixed) || info.Video != "h264" || info.PixFmt != "yuv420p" || info.Audio != "aac" {
		t.Fatalf("%+v %v", info, boxOrder(fixed))
	}
}

func TestSentVideoIsUploadedWithItsIndexFirst(t *testing.T) {
	h := newHarness(t)
	src := makeClip(t, "clip.mp4", "-c:v", "libx264", "-pix_fmt", "yuv420p", "-c:a", "aac")
	// The upload folder's copy is removed after sending, like a real upload.
	path := filepath.Join(t.TempDir(), "upload")
	raw, _ := os.ReadFile(src)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	msg, err := h.app.buildMediaMessage(path, uploadMeta{Kind: "video", Filename: "clip.mp4", Width: 1, Height: 1})
	if err != nil {
		t.Fatal(err)
	}
	v := msg.GetVideoMessage()
	if v == nil || v.GetMimetype() != "video/mp4" {
		t.Fatalf("not sent as a video: %v", msg)
	}
	if !indexFirst(h.wa.lastUpload) {
		t.Fatalf("uploaded with the index last: %v", boxOrder(h.wa.lastUpload))
	}
	if v.GetWidth() != 320 || v.GetHeight() != 240 || v.GetSeconds() != 3 || int(v.GetFileLength()) != len(h.wa.lastUpload) {
		t.Fatalf("details: %dx%d %ds %d bytes", v.GetWidth(), v.GetHeight(), v.GetSeconds(), v.GetFileLength())
	}
	if _, err := os.Stat(path + ".wa.mp4"); !os.IsNotExist(err) {
		t.Fatal("converted copy left behind")
	}
	if bytes.Equal(raw, h.wa.lastUpload) {
		t.Fatal("original uploaded")
	}
}
