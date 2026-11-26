package modcheck

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// install lays out a game folder; files maps a path relative to root to its
// contents.
func install(t *testing.T, files map[string][]byte) (root, bin string) {
	t.Helper()
	root = t.TempDir()
	bin = filepath.Join(root, "DreadGame", "DreadGame", "Binaries", "Win64")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	files["DreadGame/DreadGame/Binaries/Win64/DreadGame-Win64-Shipping.exe"] = []byte("MZ game")
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root, bin
}

func names(fs []File) map[string]bool {
	out := map[string]bool{}
	for _, f := range fs {
		out[filepath.ToSlash(f.Name)] = true
	}
	return out
}

// Stand-ins shaped like the real files: the shim names Dreadnought.dll, the
// patch's launcher carries its start command.
var (
	shim          = append([]byte("MZ\x90\x00 shim "), []byte("Dreadnought.dll\x00WerReportCreate")...)
	patchLauncher = []byte(`MZ .\DreadGame\DreadGame\Binaries\Win64\DreadGame-Win64-Shipping.exe -log`)
)

func TestFindsAllThreeFilesOfThePatch(t *testing.T) {
	root, bin := install(t, map[string][]byte{
		"DreadnoughtLauncher.exe":                            patchLauncher,
		"DreadGame/DreadGame/Binaries/Win64/wer.dll":         shim,
		"DreadGame/DreadGame/Binaries/Win64/Dreadnought.dll": []byte("MZ mod"),
	})
	got := names(Find(root, bin))
	for _, want := range []string{"DreadnoughtLauncher.exe", "DreadGame/DreadGame/Binaries/Win64/wer.dll", "DreadGame/DreadGame/Binaries/Win64/Dreadnought.dll"} {
		if !got[want] {
			t.Errorf("%s not found; got %v", want, got)
		}
	}
}

// A clean install, the original launcher, this launcher under the original's
// name, and an operator's battle-server wer.dll are all left alone.
func TestLeavesEverythingElseAlone(t *testing.T) {
	big := bytes.Repeat([]byte{0}, maxLauncherBytes+1)
	copy(big, patchLauncher) // even with the marker: far too big to be the patch's
	cases := map[string]map[string][]byte{
		"clean":                 {},
		"original launcher":     {"DreadnoughtLauncher.exe": big},
		"this launcher renamed": {"DreadnoughtLauncher.exe": append([]byte("MZ go launcher "), bytes.Repeat([]byte{1}, 2048)...)},
		"battle-server wer.dll": {"DreadGame/DreadGame/Binaries/Win64/wer.dll": []byte("MZ dn_host_loadout WerReportSubmit")},
	}
	for name, files := range cases {
		root, bin := install(t, files)
		if got := Find(root, bin); len(got) != 0 {
			t.Errorf("%s: found %v", name, got)
		}
	}
}

func TestKnownReleaseIsNamed(t *testing.T) {
	// Not the real DLL; an unknown build is still the patch.
	root, bin := install(t, map[string][]byte{"DreadGame/DreadGame/Binaries/Win64/Dreadnought.dll": []byte("MZ own build")})
	got := Find(root, bin)
	if len(got) != 1 || got[0].What != "the patch's game mod" {
		t.Fatalf("got %+v", got)
	}
}

func TestRemoveDeletesOnlyThePatch(t *testing.T) {
	root, bin := install(t, map[string][]byte{
		"DreadnoughtLauncher.exe":                            patchLauncher,
		"DreadGame/DreadGame/Binaries/Win64/wer.dll":         shim,
		"DreadGame/DreadGame/Binaries/Win64/Dreadnought.dll": []byte("MZ mod"),
		"DreadGame/DreadGame/Binaries/Win64/steam_api64.dll": []byte("MZ steam"),
	})
	removed, failed, err := Remove(root, bin)
	if err != nil || len(failed) != 0 || len(removed) != 3 {
		t.Fatalf("removed %v failed %v err %v", removed, failed, err)
	}
	if got := Find(root, bin); len(got) != 0 {
		t.Errorf("still found %v", got)
	}
	for _, keep := range []string{"DreadGame/DreadGame/Binaries/Win64/steam_api64.dll", "DreadGame/DreadGame/Binaries/Win64/DreadGame-Win64-Shipping.exe"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(keep))); err != nil {
			t.Errorf("%s was removed", keep)
		}
	}
}

// The real files from the patch's releases, when a copy is at hand
// (MODCHECK_RELEASE_DIR holds the extracted Client.zip).
func TestRealReleaseFiles(t *testing.T) {
	dir := os.Getenv("MODCHECK_RELEASE_DIR")
	if dir == "" {
		t.Skip("MODCHECK_RELEASE_DIR not set")
	}
	files := map[string][]byte{}
	for src, dst := range map[string]string{
		"DreadnoughtLauncher.exe": "DreadnoughtLauncher.exe",
		"wer.dll":                 "DreadGame/DreadGame/Binaries/Win64/wer.dll",
		"Dreadnought.dll":         "DreadGame/DreadGame/Binaries/Win64/Dreadnought.dll",
	} {
		data, err := os.ReadFile(filepath.Join(dir, src))
		if err != nil {
			t.Fatal(err)
		}
		files[dst] = data
	}
	root, bin := install(t, files)
	got := Find(root, bin)
	if len(got) != 3 {
		t.Fatalf("found %+v", got)
	}
	for _, f := range got {
		t.Logf("%s: %s", f.Name, f.What)
	}
}

// The live battle-server wer.dll (MODCHECK_BATTLE_DLL) is not the patch.
func TestRealBattleServerDLL(t *testing.T) {
	p := os.Getenv("MODCHECK_BATTLE_DLL")
	if p == "" {
		t.Skip("MODCHECK_BATTLE_DLL not set")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	root, bin := install(t, map[string][]byte{"DreadGame/DreadGame/Binaries/Win64/wer.dll": data})
	if got := Find(root, bin); len(got) != 0 {
		t.Errorf("battle-server DLL taken for the patch: %+v", got)
	}
}

// Keep: the DLLs are renamed for one of our games and get their names back;
// the replaced launcher is not touched.
func TestDisableAndRestore(t *testing.T) {
	root, bin := install(t, map[string][]byte{
		"DreadnoughtLauncher.exe":                            patchLauncher,
		"DreadGame/DreadGame/Binaries/Win64/wer.dll":         shim,
		"DreadGame/DreadGame/Binaries/Win64/Dreadnought.dll": []byte("MZ mod"),
	})
	disabled, err := Disable(root, bin)
	if err != nil || len(disabled) != 2 {
		t.Fatalf("disabled %v err %v", disabled, err)
	}
	got := names(Find(root, bin))
	if len(got) != 1 || !got["DreadnoughtLauncher.exe"] {
		t.Errorf("while disabled found %v, want only the launcher", got)
	}
	if !Disabled(bin) {
		t.Error("Disabled says no")
	}
	restored, err := Restore(bin)
	if err != nil || len(restored) != 2 {
		t.Fatalf("restored %v err %v", restored, err)
	}
	if len(Find(root, bin)) != 3 || Disabled(bin) {
		t.Errorf("after restore found %v", Find(root, bin))
	}
	if data, _ := os.ReadFile(filepath.Join(bin, "wer.dll")); !bytes.Equal(data, shim) {
		t.Error("wer.dll content changed")
	}
}

// A file reinstalled while ours was renamed is kept; a stale renamed copy
// does not block the next Disable.
func TestRestoreKeepsAReinstalledFile(t *testing.T) {
	root, bin := install(t, map[string][]byte{"DreadGame/DreadGame/Binaries/Win64/wer.dll": shim})
	if _, err := Disable(root, bin); err != nil {
		t.Fatal(err)
	}
	newer := append([]byte("newer "), shim...)
	if err := os.WriteFile(filepath.Join(bin, "wer.dll"), newer, 0o644); err != nil {
		t.Fatal(err)
	}
	if r, err := Restore(bin); err != nil || len(r) != 0 {
		t.Fatalf("restored %v err %v", r, err)
	}
	if data, _ := os.ReadFile(filepath.Join(bin, "wer.dll")); !bytes.Equal(data, newer) {
		t.Error("the reinstalled wer.dll was overwritten")
	}
	if d, err := Disable(root, bin); err != nil || len(d) != 1 {
		t.Fatalf("second disable %v %v", d, err)
	}
	if data, _ := os.ReadFile(filepath.Join(bin, "wer.dll"+OffSuffix)); !bytes.Equal(data, newer) {
		t.Error("the renamed copy is not the live file")
	}
}
