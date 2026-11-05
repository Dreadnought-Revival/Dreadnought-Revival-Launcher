//go:build windows

package main

// Launcher updates (added 2026-11-05). At start the window asks GitHub for
// this repository's latest release; when its version (tag v1.0.N, see
// .github/workflows/release.yml) is newer than launcherVersion, the page offers
// an Update button. Updating downloads Dreadnought-Revival.exe from that
// release, checks it against the release's SHA256SUMS.txt (both over GitHub's
// verified HTTPS), and swaps it in for the running exe -- whatever this exe is
// called, e.g. DreadnoughtLauncher.exe when it replaced the original -- then
// starts it and closes. Windows will not overwrite a running exe but lets it
// be renamed, so the old one becomes <exe>.old and is deleted at the next
// start. When the launcher's folder cannot be written (Program Files), the
// button opens the release page instead.
//
// A local build (launcherVersion "dev") never checks. skip_update_check in
// dn-launcher.json turns the check off. Nothing is downloaded without the
// player pressing Update.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// latestReleaseURL is the GitHub API's latest release of this repository.
var latestReleaseURL = "https://api.github.com/repos/Dreadnought-Revival/Dreadnought-Revival-Launcher/releases/latest"

const (
	updateAssetName   = "Dreadnought-Revival.exe"
	updateSumsName    = "SHA256SUMS.txt"
	updateMaxExeBytes = 64 << 20
)

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

type releaseInfo struct {
	Tag    string         `json:"tag_name"`
	Page   string         `json:"html_url"`
	Assets []releaseAsset `json:"assets"`
}

// parseVersion reads "1.0.3" or "v1.0.3"; ok is false for anything else
// ("dev", "playtest-2").
func parseVersion(v string) (out [3]int, ok bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(v), "v"), ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// newerVersion reports whether latest is a newer version than current.
func newerVersion(latest, current string) bool {
	l, ok1 := parseVersion(latest)
	c, ok2 := parseVersion(current)
	if !ok1 || !ok2 {
		return false
	}
	for i := range l {
		if l[i] != c[i] {
			return l[i] > c[i]
		}
	}
	return false
}

// updateHTTPClient checks certificates the system's way -- not
// launcherHTTPClient, which sends every connection to the game server.
func updateHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout}
}

func fetchLatestRelease() (releaseInfo, error) {
	var rel releaseInfo
	req, err := http.NewRequest(http.MethodGet, latestReleaseURL, nil)
	if err != nil {
		return rel, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Dreadnought-Revival-launcher/"+launcherVersion)
	resp, err := updateHTTPClient(8 * time.Second).Do(req)
	if err != nil {
		return rel, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return rel, fmt.Errorf("GitHub answered HTTP %d", resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return rel, fmt.Errorf("release info: %w", err)
	}
	return rel, nil
}

func (r releaseInfo) asset(name string) (releaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return releaseAsset{}, false
}

// CheckUpdate is the page's update check.
func (a *launcherAPI) CheckUpdate() map[string]any {
	out := map[string]any{"available": false, "current": launcherVersion}
	if _, ok := parseVersion(launcherVersion); !ok || a.config().SkipUpdateCheck {
		return out
	}
	rel, err := fetchLatestRelease()
	if err != nil {
		fmt.Printf("[!] Update check: %v\n", err)
		out["error"] = err.Error()
		return out
	}
	if !newerVersion(rel.Tag, launcherVersion) {
		fmt.Printf("[*] Launcher %s is up to date (latest %s)\n", launcherVersion, rel.Tag)
		return out
	}
	_, hasExe := rel.asset(updateAssetName)
	_, hasSums := rel.asset(updateSumsName)
	fmt.Printf("[*] Launcher update available: %s (this is %s)\n", rel.Tag, launcherVersion)
	out["available"] = true
	out["version"] = strings.TrimPrefix(rel.Tag, "v")
	out["page"] = rel.Page
	out["inPlace"] = hasExe && hasSums && a.exeDirWritable()
	return out
}

// exeDirWritable: can the launcher put a file beside itself?
func (a *launcherAPI) exeDirWritable() bool {
	f, err := os.CreateTemp(a.exeDir, ".dn-update-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// ApplyUpdate downloads, checks and installs the latest release, then starts
// it. ok means the new launcher was started and this one should close.
func (a *launcherAPI) ApplyUpdate() map[string]any {
	fail := func(err error) map[string]any {
		fmt.Printf("[!] Update failed: %v\n", err)
		return map[string]any{"ok": false, "error": "The update failed: " + err.Error()}
	}
	rel, err := fetchLatestRelease()
	if err != nil {
		return fail(err)
	}
	if !newerVersion(rel.Tag, launcherVersion) {
		return map[string]any{"ok": false, "error": "This launcher is already the latest version."}
	}
	exeAsset, ok1 := rel.asset(updateAssetName)
	sumsAsset, ok2 := rel.asset(updateSumsName)
	if !ok1 || !ok2 {
		return fail(fmt.Errorf("release %s has no %s or %s", rel.Tag, updateAssetName, updateSumsName))
	}
	sums, err := download(sumsAsset.URL, 64<<10)
	if err != nil {
		return fail(err)
	}
	want, err := sumFor(sums, updateAssetName)
	if err != nil {
		return fail(err)
	}
	exe, err := download(exeAsset.URL, updateMaxExeBytes)
	if err != nil {
		return fail(err)
	}
	if got := sha256.Sum256(exe); hex.EncodeToString(got[:]) != want {
		return fail(errors.New("the download does not match the release's checksum"))
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	if err := replaceExe(self, exe); err != nil {
		return fail(err)
	}
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Dir = filepath.Dir(self)
	if err := cmd.Start(); err != nil {
		return fail(fmt.Errorf("installed, but could not start it: %w (start the launcher again)", err))
	}
	fmt.Printf("[+] Updated to %s; started the new launcher\n", rel.Tag)
	return map[string]any{"ok": true, "version": strings.TrimPrefix(rel.Tag, "v")}
}

func download(url string, limit int64) ([]byte, error) {
	resp, err := updateHTTPClient(5 * time.Minute).Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("download too large")
	}
	return data, nil
}

// sumFor finds name's hash in sha256sum output ("<hex>  <name>").
func sumFor(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name && len(f[0]) == 64 {
			return strings.ToLower(f[0]), nil
		}
	}
	return "", fmt.Errorf("%s lists no %s", updateSumsName, name)
}

// replaceExe puts data in place of the exe at path: written beside it first,
// then the running exe renamed to path.old and the new one renamed to path.
// On failure the old exe is put back.
func replaceExe(path string, data []byte) error {
	tmp := path + ".new"
	if err := os.WriteFile(tmp, data, 0o755); err != nil {
		return err
	}
	old := path + ".old"
	_ = os.Remove(old)
	if err := os.Rename(path, old); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("move the old launcher aside: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Rename(old, path)
		_ = os.Remove(tmp)
		return fmt.Errorf("put the new launcher in place: %w", err)
	}
	return nil
}

// removeOldExe deletes the exe an update left behind (at start; it was still
// running when it was renamed).
func removeOldExe() {
	if self, err := os.Executable(); err == nil {
		_ = os.Remove(self + ".old")
	}
}
