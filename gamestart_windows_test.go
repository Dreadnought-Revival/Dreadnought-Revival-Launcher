//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// The game is started with CreateProcess (no inherited standard handles); the
// launcher must still be able to wait for it and read its exit code.
func TestStartGameProcessWaitsAndReportsExitCode(t *testing.T) {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		sys = `C:\windows`
	}
	cmd := filepath.Join(sys, "system32", "cmd.exe")
	p, err := startGameProcess(cmd, []string{"/c", "exit", "7"}, sys)
	if err != nil {
		t.Fatal(err)
	}
	state, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if state.ExitCode() != 7 {
		t.Fatalf("exit code %d, want 7", state.ExitCode())
	}
}
