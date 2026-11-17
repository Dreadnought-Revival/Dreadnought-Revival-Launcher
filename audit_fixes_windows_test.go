//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Browser mode answers only requests that name it (DNS rebinding guard).
func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:5000": true, "localhost:5000": true, "[::1]:5000": true, "LOCALHOST:5000": true,
		"127.0.0.1:5001": false, "evil.example:5000": false, "127.0.0.1": false, "": false,
	} {
		if got := loopbackHost(host, 5000); got != want {
			t.Errorf("%q: %v, want %v", host, got, want)
		}
	}
}

func TestNetworkPath(t *testing.T) {
	for p, want := range map[string]bool{
		`\\server\share\Dreadnought`: true, `//server/share`: true, `\\?\UNC\server\share`: true,
		`C:\Games\Dreadnought`: false, `\\?\C:\Games`: false, `D:/Games`: false,
	} {
		if got := networkPath(p); got != want {
			t.Errorf("%q: %v, want %v", p, got, want)
		}
	}
	a := &launcherAPI{}
	if r := a.SetGameDir(`\\server\share\Dreadnought`); r["ok"] != false || !strings.Contains(r["error"].(string), "network") {
		t.Errorf("SetGameDir took a network path: %v", r)
	}
}

// A log ending with the engine's last line counts as a clean shutdown.
func TestGameLogClosedCleanly(t *testing.T) {
	dir := t.TempDir()
	clean := filepath.Join(dir, "clean.log")
	cut := filepath.Join(dir, "cut.log")
	_ = os.WriteFile(clean, []byte(strings.Repeat("x\r\n", 5000)+"LogExit: Exiting.\r\nLog file closed, 11/17/26 22:18:33\r\n"), 0o600)
	_ = os.WriteFile(cut, []byte(strings.Repeat("x\r\n", 5000)+"LogNet: still playing\r\n"), 0o600)
	if !gameLogClosedCleanly(clean) {
		t.Error("clean log not recognised")
	}
	if gameLogClosedCleanly(cut) || gameLogClosedCleanly(filepath.Join(dir, "missing.log")) {
		t.Error("an unfinished or missing log counted as closed")
	}
}

// readGameLog keeps a big log's start and end, as trimGameLog does in memory.
func TestReadGameLogHeadAndTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	big := make([]byte, logUploadMaxRaw+1000)
	for i := range big {
		big[i] = 'a'
	}
	copy(big, "START")
	copy(big[len(big)-3:], "END")
	if err := os.WriteFile(path, big, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readGameLog(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(trimGameLog(big)) {
		t.Fatalf("readGameLog (%d bytes) differs from trimGameLog (%d)", len(got), len(trimGameLog(big)))
	}
}

// Concurrent changes to settings.json all land.
func TestUpdateSettingsConcurrent(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = updateSettings(func(s *launcherSettings) bool {
				s.Certs = append(s.Certs, certRecord{SHA256: strings.Repeat("a", i+1)})
				return true
			})
		}(i)
	}
	wg.Wait()
	if n := len(loadSettings().Certs); n != 20 {
		t.Fatalf("%d of 20 changes kept", n)
	}
}
