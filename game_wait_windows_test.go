//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestMain lets the test binary play the game in TestWaitGameReportsTheExit:
// with DN_TEST_EXIT_CODE set it exits with that code at once.
func TestMain(m *testing.M) {
	if code, err := strconv.Atoi(os.Getenv("DN_TEST_EXIT_CODE")); err == nil {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// The launcher waits for the game and reports how it ended: a clean exit is
// not a crash, a non-zero exit code is.
func TestWaitGameReportsTheExit(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	for _, c := range []struct {
		code    string
		want    int
		crashed bool
	}{{"0", 0, false}, {"3", 3, true}} {
		// The test binary itself stands in for the game (TestMain).
		cmd := exec.Command(os.Args[0])
		cmd.Env = append(os.Environ(), "DN_TEST_EXIT_CODE="+c.code)
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		a := &launcherAPI{game: cmd.Process, gameStarted: time.Now()}
		if !a.GameRunning() {
			t.Error("GameRunning = false with a game started")
		}
		r := a.WaitGame()
		if r["ok"] != true || r["exitCode"] != c.want || r["crashed"] != c.crashed {
			t.Errorf("exit %s: %v", c.code, r)
		}
		if a.GameRunning() {
			t.Error("GameRunning = true after the game closed")
		}
	}
	if r := (&launcherAPI{}).WaitGame(); r["ok"] != false {
		t.Errorf("no game started: %v", r)
	}
}

// A crash folder counts only when the game wrote it during this run, and in
// Saved\Logs only the crash reporter's own folders count.
func TestNewestCrashFolder(t *testing.T) {
	saved := t.TempDir()
	mk := func(rel string, age time.Duration) string {
		p := filepath.Join(saved, rel)
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(p, when, when); err != nil {
			t.Fatal(err)
		}
		return p
	}
	started := time.Now().Add(-10 * time.Minute)
	mk(filepath.Join("Crashes", "UE4CC-Windows-OLD"), time.Hour)
	mk(filepath.Join("Logs", "Backup"), time.Minute) // not a crash folder
	if got := newestCrashFolder([]string{saved, filepath.Join(saved, "missing")}, started); got != "" {
		t.Errorf("no crash this run, got %q", got)
	}
	want := mk(filepath.Join("Logs", "UE4CC-Windows-NEW_0000"), time.Minute)
	mk(filepath.Join("Crashes", "UE4CC-Windows-EARLIER"), 5*time.Minute)
	if got := newestCrashFolder([]string{saved}, started); got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// The two window options are saved with the others.
func TestWindowOptionsAreSaved(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	a := &launcherAPI{}
	if !a.SetOption("closeOnPlay", true) || !a.SetOption("trayMinimize", true) {
		t.Fatal("SetOption refused a window option")
	}
	if s := loadSettings(); !s.CloseOnPlay || !s.TrayMinimize {
		t.Errorf("settings = %+v", s)
	}
	if init := a.Init(); init["closeOnPlay"] != true || init["trayMinimize"] != true {
		t.Errorf("Init = %v", init)
	}
	if a.SetOption("noSuchOption", true) {
		t.Error("an unknown option was accepted")
	}
}
