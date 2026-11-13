package relay

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

func (f *fixture) publish(t *testing.T, name, body, sig string) {
	t.Helper()
	if err := os.MkdirAll(f.cfg.DownloadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.DownloadsDir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if sig != "" {
		if err := os.WriteFile(filepath.Join(f.cfg.DownloadsDir, name+".sig"), []byte(sig), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func installerPath(name string) string {
	return "/api/update/installer/" + url.PathEscape(name)
}

func TestUpdatesNeedLoginAndTheApp(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "WA Relay_1.1.0_x64-setup.exe", "MZ", "sig")
	for _, path := range []string{"/api/update/latest", installerPath("WA Relay_1.1.0_x64-setup.exe")} {
		if resp := f.do("GET", path, "", app(), ""); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s without a login: %d", path, resp.StatusCode)
		}
		if resp := f.do("GET", path, goodToken(t), nil, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s from a browser: %d", path, resp.StatusCode)
		}
		if resp := f.do("GET", path, goodToken(t), map[string]string{ClientHeader: "1", "Origin": "https://evil.example"}, ""); resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s with an Origin: %d", path, resp.StatusCode)
		}
		if resp := f.do("GET", path, goodToken(t), app(), ""); resp.StatusCode != http.StatusOK {
			t.Errorf("%s from the app: %d", path, resp.StatusCode)
		}
	}
}

func TestLatestIsTheHighestVersionNotTheLastAlphabetically(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "WA Relay_1.9.0_x64-setup.exe", "old", "sig-old")
	f.publish(t, "WA Relay_1.10.0_x64-setup.exe", "new", "sig-new\r\n")
	resp := f.do("GET", "/api/update/latest", goodToken(t), app(), "")
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != "1.10.0" || got["signature"] != "sig-new" || got["file"] != "WA Relay_1.10.0_x64-setup.exe" {
		t.Fatalf("got %v", got)
	}
	if _, hasURL := got["url"]; hasURL {
		t.Fatal("the answer must not choose where the app sends its login cookie")
	}
	resp = f.do("GET", installerPath(got["file"]), goodToken(t), app(), "")
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "new" || resp.Header.Get("Cache-Control") != "private, no-store" {
		t.Fatalf("installer: %q, cache %q", body, resp.Header.Get("Cache-Control"))
	}
}

func TestUnsignedBuildIsNeverOffered(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "WA Relay_1.0.0_x64-setup.exe", "no updater", "")
	if resp := f.do("GET", "/api/update/latest", goodToken(t), app(), ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unsigned: %d", resp.StatusCode)
	}
	// A newer build whose signature is missing doesn't fall back to an older one either.
	f.publish(t, "WA Relay_1.1.0_x64-setup.exe", "signed", "sig")
	f.publish(t, "WA Relay_1.2.0_x64-setup.exe", "not signed yet", "")
	if resp := f.do("GET", "/api/update/latest", goodToken(t), app(), ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("newest unsigned: %d", resp.StatusCode)
	}
}

func TestNoInstallersMeansNoUpdate(t *testing.T) {
	f := newFixture(t)
	if resp := f.do("GET", "/api/update/latest", goodToken(t), app(), ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("empty: %d", resp.StatusCode)
	}
}

func TestUpdateRouteOnlyServesInstallers(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "WA Relay_1.1.0_x64-setup.exe", "MZ", "sig")
	os.WriteFile(filepath.Join(f.cfg.DownloadsDir, "secret.db"), []byte("x"), 0o600)
	for _, name := range []string{"secret.db", "WA Relay_1.1.0_x64-setup.exe.sig", "..%2Fruntime.json", "..%5Csecret.db", "missing.exe"} {
		if resp := f.do("GET", "/api/update/installer/"+name, goodToken(t), app(), ""); resp.StatusCode == http.StatusOK {
			t.Errorf("%s was served", name)
		}
	}
}

func TestVersionParsing(t *testing.T) {
	for name, want := range map[string]string{
		"WA Relay_1.2.3_x64-setup.exe": "1.2.3",
		"WA Relay_1.2_x64-setup.exe":   "",
		"WA Relay_1.2.3.4_x64.exe":     "",
		"WA Relay_x64-setup.exe":       "",
	} {
		_, got, _ := installerVersion(name)
		if got != want {
			t.Errorf("%s: %q", name, got)
		}
	}
}

func (f *fixture) latestFor(t *testing.T, agent string) string {
	t.Helper()
	resp := f.do("GET", "/api/update/latest", goodToken(t), map[string]string{ClientHeader: "1", "User-Agent": agent}, "")
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent {
		return ""
	}
	var got map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	return got["version"]
}

func TestAStepReleaseIsNeverSkipped(t *testing.T) {
	f := newFixture(t)
	f.publish(t, "WA Relay_1.2.16_x64-setup.exe", "a", "sig-a")
	f.publish(t, "WA Relay_1.2.17_x64-setup.exe", "step", "sig-step")
	f.publish(t, "WA Relay_1.2.18_x64-setup.exe", "b", "sig-b")
	f.publish(t, "WA Relay_1.2.19_x64-setup.exe", "c", "sig-c")
	if got := f.latestFor(t, "wa-relay-app/1.2.15"); got != "1.2.19" {
		t.Fatalf("with no step, the newest: %q", got)
	}
	if err := os.WriteFile(filepath.Join(f.cfg.DownloadsDir, "WA Relay_1.2.17_x64-setup.exe.step"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for agent, want := range map[string]string{
		"wa-relay-app/1.2.15":         "1.2.17", // older than the step: the step first
		"wa-relay-app/1.2.16":         "1.2.17",
		"wa-relay-app/1.2.17":         "1.2.19", // past it: the newest
		"wa-relay-app/1.2.18":         "1.2.19",
		"wa-relay-app/updater/1.2.16": "1.2.17", // can't tell: as if older
		"Mozilla/5.0":                 "1.2.17",
		"wa-relay-app/1.2":            "1.2.17",
	} {
		if got := f.latestFor(t, agent); got != want {
			t.Errorf("%s: got %q, want %q", agent, got, want)
		}
	}
	// A step that isn't signed holds everything back rather than being skipped.
	os.Remove(filepath.Join(f.cfg.DownloadsDir, "WA Relay_1.2.17_x64-setup.exe.sig"))
	if got := f.latestFor(t, "wa-relay-app/1.2.16"); got != "" {
		t.Fatalf("unsigned step: got %q", got)
	}
}
