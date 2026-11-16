package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WhatsApp on phones streams a video while it downloads, so it needs H.264 video, AAC audio and the
// MP4 index (the moov box) at the front of the file. Many downloaded or exported MP4s keep the index
// at the end, and Android then shows "something is wrong with the video file". Before a video is
// uploaded it is rewritten with ffmpeg: copied as-is when the streams are already right (fast, no
// quality loss), re-encoded otherwise. Without ffmpeg the file goes as before.

type videoInfo struct {
	Video, PixFmt, Audio   string
	Width, Height, Seconds uint32
	Rotated                bool
}

var (
	ffmpegOnce sync.Once
	ffmpegExe  string
)

// ffmpegPath finds ffmpeg: WA_FFMPEG, then PATH, then next to the bridge's own exe.
func ffmpegPath() string {
	ffmpegOnce.Do(func() {
		if p := os.Getenv("WA_FFMPEG"); p != "" {
			if _, err := os.Stat(p); err == nil {
				ffmpegExe = p
				return
			}
		}
		if p, err := exec.LookPath("ffmpeg"); err == nil {
			ffmpegExe = p
			return
		}
		if self, err := os.Executable(); err == nil {
			p := filepath.Join(filepath.Dir(self), "ffmpeg.exe")
			if _, err := os.Stat(p); err == nil {
				ffmpegExe = p
			}
		}
	})
	return ffmpegExe
}

var (
	reVideo    = regexp.MustCompile(`Stream #\d+:\d+[^:]*: Video: (\w+)[^,]*, (\w+)[^\n]*?, (\d{2,5})x(\d{2,5})`)
	reAudio    = regexp.MustCompile(`Stream #\d+:\d+[^:]*: Audio: (\w+)`)
	reDuration = regexp.MustCompile(`Duration: (\d+):(\d\d):(\d\d)\.(\d+)`)
	reRotation = regexp.MustCompile(`rotation of (-?\d+(?:\.\d+)?) degrees|rotate\s*: (-?\d+)`)
)

// parseProbe reads what ffmpeg prints about an input file.
func parseProbe(out string) (videoInfo, bool) {
	var info videoInfo
	m := reVideo.FindStringSubmatch(out)
	if m == nil {
		return info, false
	}
	info.Video, info.PixFmt = m[1], m[2]
	w, _ := strconv.Atoi(m[3])
	h, _ := strconv.Atoi(m[4])
	if r := reRotation.FindStringSubmatch(out); r != nil {
		deg := r[1]
		if deg == "" {
			deg = r[2]
		}
		if f, err := strconv.ParseFloat(deg, 64); err == nil {
			if n := int(f) % 180; n == 90 || n == -90 {
				w, h = h, w
				info.Rotated = true
			}
		}
	}
	info.Width, info.Height = uint32(w), uint32(h)
	if a := reAudio.FindStringSubmatch(out); a != nil {
		info.Audio = a[1]
	}
	if d := reDuration.FindStringSubmatch(out); d != nil {
		hh, _ := strconv.Atoi(d[1])
		mm, _ := strconv.Atoi(d[2])
		ss, _ := strconv.Atoi(d[3])
		frac, _ := strconv.ParseFloat("0."+d[4], 64)
		total := float64(hh*3600+mm*60+ss) + frac
		info.Seconds = uint32(total + 0.5)
		if info.Seconds == 0 && total > 0 {
			info.Seconds = 1
		}
	}
	return info, true
}

func probeVideo(ctx context.Context, exe, path string) (videoInfo, bool) {
	// ffmpeg with only an input prints the stream details and exits 1; that's expected.
	out, _ := exec.CommandContext(ctx, exe, "-hide_banner", "-nostdin", "-i", path).CombinedOutput()
	return parseProbe(string(out))
}

// canCopy says whether the streams already play everywhere, so only the box order needs changing.
func (v videoInfo) canCopy() bool {
	return v.Video == "h264" && (v.PixFmt == "yuv420p" || v.PixFmt == "yuvj420p") && (v.Audio == "" || v.Audio == "aac")
}

// prepareVideo returns a file WhatsApp's players accept and what's known about it. When it made a new
// file, out differs from in and the caller removes it. On any problem it returns in unchanged.
func prepareVideo(in string) (out string, info videoInfo) {
	exe := ffmpegPath()
	if exe == "" {
		return in, info
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	src, ok := probeVideo(ctx, exe, in)
	if !ok {
		return in, info
	}
	out = in + ".wa.mp4"
	args := []string{"-hide_banner", "-nostdin", "-loglevel", "error", "-y", "-i", in,
		"-map", "0:v:0", "-map", "0:a:0?", "-sn", "-dn", "-map_metadata", "-1"}
	if src.canCopy() {
		args = append(args, "-c", "copy")
	} else {
		args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-pix_fmt", "yuv420p",
			"-vf", "scale=trunc(iw/2)*2:trunc(ih/2)*2", "-c:a", "aac", "-b:a", "128k", "-ac", "2")
	}
	args = append(args, "-movflags", "+faststart", "-f", "mp4", out)
	if msg, err := exec.CommandContext(ctx, exe, args...).CombinedOutput(); err != nil {
		fmt.Printf("video: ffmpeg failed, sending the original: %v %s\n", err, strings.TrimSpace(string(msg)))
		_ = os.Remove(out)
		return in, src
	}
	if done, ok := probeVideo(ctx, exe, out); ok {
		return out, done
	}
	return out, src
}
