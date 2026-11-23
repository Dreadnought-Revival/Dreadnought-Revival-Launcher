//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func writeCrashReport(t *testing.T, root, name string, when time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "Microsoft", "Windows", "WER", "ReportQueue", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "Report.wer")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, when, when); err != nil {
		t.Fatal(err)
	}
	return dir
}

// The game's queued Windows Error Report, written during the run, marks the
// session as crashed and is named as its crash folder; another program's is not.
func TestWaitGameFindsTheGamesErrorReport(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("ProgramData", t.TempDir())
	writeCrashReport(t, local, "AppCrash_notepad.exe_1", time.Now())
	want := writeCrashReport(t, local, "AppCrash_UE4-DreadGame_0f1e2d", time.Now().Add(time.Second))

	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "DN_TEST_EXIT_CODE=7")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	a := &launcherAPI{game: cmd.Process, gameStarted: time.Now().Add(-time.Minute)}
	r := a.WaitGame()
	if r["crashed"] != true || r["crashFolder"] != want {
		t.Errorf("WaitGame = %v, want crashed with %s", r, want)
	}
}

// Crash-on-exit and next-Play reports go once a week at most: inside the week
// they are passed over (remembered as handled) without any upload.
func TestCrashOnExitReportsAreWeekly(t *testing.T) {
	local := t.TempDir()
	t.Setenv("LOCALAPPDATA", local)
	t.Setenv("ProgramData", t.TempDir())
	if err := updateSettings(func(s *launcherSettings) bool { s.ExitCrashSent = time.Now().Add(-time.Hour).Unix(); return true }); err != nil {
		t.Fatal(err)
	}
	writeCrashReport(t, local, "AppCrash_UE4-DreadGame_a", time.Now())
	writeCrashReport(t, local, "AppCrash_DreadGame-Win64-Shipping_b", time.Now())
	a := &launcherAPI{}
	a.uploadCrashReports(gameSession{exitCode: -1073741819}, time.Now().Add(-time.Minute), true)
	got := loadSettings().CrashesSent
	if len(got) != 2 {
		t.Fatalf("handled %v, want both reports passed over", got)
	}
	// Turned off: nothing is even looked at.
	writeCrashReport(t, local, "AppCrash_UE4-DreadGame_c", time.Now())
	_ = updateSettings(func(s *launcherSettings) bool { s.NoLogUpload = true; return true })
	a.uploadCrashReports(gameSession{late: true}, time.Now().Add(-time.Minute), false)
	if n := len(loadSettings().CrashesSent); n != 2 {
		t.Errorf("with uploads off, %d reports handled", n)
	}
}
