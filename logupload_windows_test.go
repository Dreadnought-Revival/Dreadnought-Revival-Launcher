//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/logupload"
)

// A log as the client writes it (names and token made up).
const testGameLog = "LogInit: Command line: /Game/Maps/Launch_P -noeac\r\n" +
	"[2026.11.17-07.59.31:453][  0]LogInit: Computer: DESKTOP-TEST01\r\n" +
	"[2026.11.17-07.59.31:453][  0]LogInit: User: testwinuser\r\n" +
	"LogInit: Base directory: C:\\Users\\testwinuser\\Games\\Dreadnought\\\r\n" +
	"LogPaths: D:/Profiles/tw/AppData/Local/DreadGame/Saved/\r\n" +
	"LogNet: host desktop-test01 ready\r\n" +
	"[2026.11.17-07.59.42:735][ 51]LogWebServicesPlugin: Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl\r\n" +
	"[2026.11.17-07.59.43:487][ 96]LogWebServicesPlugin: Authorization: Session a0c2e098-0000-0000-0000-000000000000, TestPilot\r\n"

func TestRedactGameLog(t *testing.T) {
	got := string(redactGameLog([]byte(testGameLog), "DESKTOP-TEST01", `D:\Profiles\tw`))
	for _, gone := range []string{"DESKTOP-TEST01", "desktop-test01", "testwinuser", "Profiles/tw", "eyJ", "c2lnbmF0dXJl"} {
		if strings.Contains(got, gone) {
			t.Errorf("%q is still in the log:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"/Game/Maps/Launch_P", "LogInit: Computer: <removed>\r\n", "LogInit: User: <removed>\r\n",
		`C:\Users\<user>\Games\Dreadnought\`, "<user folder>/AppData/Local/DreadGame/Saved/", "host <computer> ready",
		"Authorization: Bearer <token removed>\r\n", "Session a0c2e098-0000-0000-0000-000000000000, TestPilot"} {
		if !strings.Contains(got, kept) {
			t.Errorf("want %q in the log:\n%s", kept, got)
		}
	}
	// A short computer name is not blanked across the log.
	if got := string(redactGameLog([]byte("/Game/Maps/x GAME"), "GAME", "")); got != "/Game/Maps/x GAME" {
		t.Errorf("short computer name blanked: %q", got)
	}
}

// The launcher's whole path from log file to parts, with a PC's own names: a
// log past the cap keeps its start and end, nothing private is left.
func TestSpoolGameLogLikeThePlayPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "DreadGame.log")
	var b strings.Builder
	b.WriteString(testGameLog)
	for i := 0; i < 20000; i++ {
		b.WriteString("[2026.11.17-08.00.00:000][  0]LogNet: filler line of the test log, filler filler filler\r\n")
	}
	b.WriteString("LogNet: host desktop-test01 at the end\r\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := logupload.Spool(path, t.TempDir(), logupload.NewRedactor("DESKTOP-TEST01", `D:\Profiles\tw`),
		logupload.Options{MaxRaw: 400 << 10, KeepTop: 100 << 10, PartSize: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	if m.Cut == 0 || m.Raw() > 400<<10+128 || len(m.Parts) < 5 {
		t.Fatalf("cut %d, raw %d, parts %d", m.Cut, m.Raw(), len(m.Parts))
	}
	m.Remove()
}
