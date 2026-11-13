package relay

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Only the app asks for these, so they sit under /api and get its login and browser checks. The
// signature is what proves an update is genuine: the app checks it against a public key built into it.

var versionInName = regexp.MustCompile(`\d+(?:\.\d+)+`)

const maxSigBytes = 16 << 10

// installerVersion reads the dotted version from an installer's name. The updater needs exactly three
// parts, so anything else is treated as having no version.
func installerVersion(name string) ([3]int, string, bool) {
	var parsed [3]int
	found := versionInName.FindString(name)
	parts := strings.Split(found, ".")
	if len(parts) != len(parsed) {
		return parsed, "", false
	}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return parsed, "", false
		}
		parsed[i] = n
	}
	return parsed, found, true
}

func newerVersion(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// latestInstaller picks the highest version, as get_app_installer does for manual installs.
func (s *Server) latestInstaller() (name, version string, ok bool) {
	var best [3]int
	for _, e := range s.installers() {
		parsed, text, valid := installerVersion(e.Name())
		if valid && (!ok || newerVersion(parsed, best)) {
			best, name, version, ok = parsed, e.Name(), text, true
		}
	}
	return name, version, ok
}

// A release can be marked as a step with an empty "<installer>.step" file beside it. An app older than a
// step is offered the lowest such step first, even if something newer exists, so a release that has to run
// once (to carry settings forward, say) is never skipped. The app's version comes from its User-Agent,
// "wa-relay-app/1.2.3"; an app that doesn't say is treated as older than every step.
var clientVersion = regexp.MustCompile(`^wa-relay-app/(\d+\.\d+\.\d+)(?:$|[ /])`)

func (s *Server) stepFor(userAgent string) (name, version string, ok bool) {
	var client [3]int
	known := false
	if m := clientVersion.FindStringSubmatch(userAgent); m != nil {
		client, _, known = installerVersion(m[1])
	}
	var best [3]int
	for _, e := range s.installers() {
		parsed, text, valid := installerVersion(e.Name())
		if !valid || (known && !newerVersion(parsed, client)) {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.cfg.DownloadsDir, e.Name()+".step")); err != nil {
			continue
		}
		if !ok || newerVersion(best, parsed) {
			best, name, version, ok = parsed, e.Name(), text, true
		}
	}
	return name, version, ok
}

// handleUpdateLatest says which version to install. It doesn't give a URL: the app only ever downloads
// from its own relay, so this answer can't send the login cookie anywhere else. An installer without a
// signature is never offered, and 204 tells the updater there's nothing to do.
func (s *Server) handleUpdateLatest(w http.ResponseWriter, r *http.Request) {
	name, version, ok := s.latestInstaller()
	if step, stepVersion, isStep := s.stepFor(r.UserAgent()); isStep {
		name, version, ok = step, stepVersion, true
	}
	if !ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	sig, err := os.ReadFile(filepath.Join(s.cfg.DownloadsDir, name+".sig"))
	sig = bytes.TrimSpace(sig)
	if err != nil || len(sig) == 0 || len(sig) > maxSigBytes {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"version": version, "signature": string(sig), "file": name})
}

func (s *Server) handleUpdateInstaller(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	for _, e := range s.installers() {
		if e.Name() == name {
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeFile(w, r, filepath.Join(s.cfg.DownloadsDir, name))
			return
		}
	}
	http.NotFound(w, r)
}
