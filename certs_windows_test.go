//go:build windows

package main

import (
	"os"
	"testing"
	"time"
)

func TestStaleCerts(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	records := []certRecord{
		{SHA256: "fresh", LastUsed: now.Add(-29 * day)},
		{SHA256: "old", LastUsed: now.Add(-31 * day)},
		{SHA256: "current", LastUsed: now.Add(-90 * day)},
		{SHA256: "declined", LastUsed: now.Add(-60 * day), Declined: now.Add(-2 * day)},
		{SHA256: "declined-long-ago", LastUsed: now.Add(-60 * day), Declined: now.Add(-8 * day)},
	}
	var got []string
	for _, r := range staleCerts(records, "current", now) {
		got = append(got, r.SHA256)
	}
	if len(got) != 2 || got[0] != "old" || got[1] != "declined-long-ago" {
		t.Fatalf("stale = %v", got)
	}
}

// A certificate is recorded only once it is in the store, and a second use
// updates the same record.
func TestNoteCertUsedOnlyWhenTrusted(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	_, _, der := testCA(t)
	noteCertUsed(der, "Test") // a fresh test CA is not in the store
	if n := len(loadSettings().Certs); n != 0 {
		t.Fatalf("recorded an untrusted certificate (%d records)", n)
	}
}

// Real store test, opt-in: DN_LAUNCHER_REAL_STORE=1. Installs a fresh test CA
// into the CURRENT USER Trusted Root store of the machine (or Wine prefix) it
// runs on, removes it the way the 30-day clean-up does, and checks it is gone.
// On Windows both steps show a confirmation; answer Yes.
func TestRemoveCertAgainstTheRealStore(t *testing.T) {
	if os.Getenv("DN_LAUNCHER_REAL_STORE") != "1" {
		t.Skip("set DN_LAUNCHER_REAL_STORE=1 to run against the real certificate store")
	}
	_, _, der := testCA(t)
	if err := installCA(der); err != nil || !caTrusted(der) {
		t.Fatalf("install: %v (trusted=%v)", err, caTrusted(der))
	}
	removed, err := removeCertFromStore(certHash(der))
	if err != nil || !removed {
		t.Fatalf("remove: removed=%v, %v", removed, err)
	}
	if caTrusted(der) {
		t.Fatal("still trusted after removal")
	}
}
