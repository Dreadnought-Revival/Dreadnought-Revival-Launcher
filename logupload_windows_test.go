//go:build windows

package main

import (
	"strings"
	"testing"
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

func TestTrimGameLog(t *testing.T) {
	small := []byte("short log")
	if string(trimGameLog(small)) != "short log" {
		t.Fatal("a short log changed")
	}
	big := make([]byte, logUploadMaxRaw+1000)
	for i := range big {
		big[i] = 'a'
	}
	copy(big, "START")
	copy(big[len(big)-3:], "END")
	out := trimGameLog(big)
	if len(out) > logUploadMaxRaw+128 || !strings.HasPrefix(string(out), "START") || !strings.HasSuffix(string(out), "END") ||
		!strings.Contains(string(out), "[launcher: 1000 bytes of the log cut here]") {
		t.Fatalf("trimmed: len %d", len(out))
	}
}
