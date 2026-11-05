//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestNewerVersion(t *testing.T) {
	for _, c := range []struct {
		latest, current string
		want            bool
	}{
		{"v1.0.4", "1.0.3", true},
		{"v1.1.0", "1.0.9", true},
		{"v2.0.0", "1.9.9", true},
		{"v1.0.3", "1.0.3", false},
		{"v1.0.2", "1.0.3", false},
		{"v1.0.10", "1.0.9", true},
		{"playtest-2", "1.0.3", false},
		{"v1.0.4", "dev", false},
		{"v1.0", "1.0.3", false},
	} {
		if got := newerVersion(c.latest, c.current); got != c.want {
			t.Errorf("newerVersion(%q, %q) = %v, want %v", c.latest, c.current, got, c.want)
		}
	}
}

func TestSumFor(t *testing.T) {
	sums := []byte("0123456789abcdef0123456789abcdef0123456789abcdef0123456789ABCDEF  Dreadnought-Revival.exe\n" +
		"1111111111111111111111111111111111111111111111111111111111111111  Dreadnought-Revival-linux.tar.gz\n")
	got, err := sumFor(sums, "Dreadnought-Revival.exe")
	if err != nil || got != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("sumFor = %q, %v", got, err)
	}
	if _, err := sumFor(sums, "other.exe"); err == nil {
		t.Fatal("found a file that is not listed")
	}
}

// The running exe is renamed aside, the new one takes its name, and the old
// one is removed at the next start.
func TestReplaceExe(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "DreadnoughtLauncher.exe")
	if err := os.WriteFile(exe, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := replaceExe(exe, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(exe); string(b) != "new" {
		t.Fatalf("exe = %q", b)
	}
	if b, _ := os.ReadFile(exe + ".old"); string(b) != "old" {
		t.Fatalf("old = %q", b)
	}
	if _, err := os.Stat(exe + ".new"); !os.IsNotExist(err) {
		t.Fatal(".new left behind")
	}
}

func TestCheckUpdate(t *testing.T) {
	rel := releaseInfo{Tag: "v1.0.9", Page: "https://example.invalid/releases/v1.0.9", Assets: []releaseAsset{
		{Name: "Dreadnought-Revival.exe", URL: "https://example.invalid/a"},
		{Name: "SHA256SUMS.txt", URL: "https://example.invalid/b"},
	}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rel)
	}))
	defer srv.Close()
	defer func(u, v string) { latestReleaseURL, launcherVersion = u, v }(latestReleaseURL, launcherVersion)
	latestReleaseURL = srv.URL
	a := &launcherAPI{exeDir: t.TempDir()}

	launcherVersion = "1.0.3"
	r := a.CheckUpdate()
	if r["available"] != true || r["version"] != "1.0.9" || r["inPlace"] != true || r["page"] != rel.Page {
		t.Fatalf("CheckUpdate = %v", r)
	}
	launcherVersion = "1.0.9"
	if r := a.CheckUpdate(); r["available"] != false {
		t.Fatalf("same version offered: %v", r)
	}
	launcherVersion = "dev"
	if r := a.CheckUpdate(); r["available"] != false {
		t.Fatalf("dev build offered: %v", r)
	}
	launcherVersion = "1.0.3"
	a.cfg.SkipUpdateCheck = true
	if r := a.CheckUpdate(); r["available"] != false {
		t.Fatalf("skip_update_check ignored: %v", r)
	}
}
