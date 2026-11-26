// Package modcheck finds, and on the player's say-so removes, the
// "Dreadnought Revival Patch" by gwog (github.com/SyST3MDeV/Dreadnought), an
// older community mod for playing without the official servers. It breaks
// this launcher's games (operator, 2026-11-26), so the launcher asks the
// player whether to uninstall it.
//
// What the patch installs, from its README and its three releases (v1-v3,
// 2024-07-14..16, Client.zip, measured 2026-11-26):
//
//	<game>\DreadnoughtLauncher.exe                         replaced: 11,776 bytes,
//	    starts DreadGame-Win64-Shipping.exe -log (the original is ~3.7 MB)
//	<game>\DreadGame\DreadGame\Binaries\Win64\wer.dll      13,824 bytes: the game
//	    loads it in place of Windows' wer.dll; it loads Dreadnought.dll and
//	    turns the Windows Error Reporting calls into no-ops (which also stops
//	    the crash reports this launcher sends)
//	<game>\DreadGame\DreadGame\Binaries\Win64\Dreadnought.dll  the mod: hooks
//	    ProcessEvent, the engine tick, the EasyAntiCheat popup and DX11 Present
//	    (an overlay on F1)
//
// The original game has no Dreadnought.dll and no wer.dll in Win64. A server
// operator's Win64 can hold the battle-server mod, which is also called
// wer.dll: it never contains the text "Dreadnought.dll" (checked against the
// live build, 2026-11-26), so it is never taken for the patch's shim. A
// launcher.exe is only the patch's when it is small and is the known build or
// carries the patch's start command, so this launcher installed under the
// original's name (update_windows.go) is never touched.
package modcheck

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// File is one installed file of the patch.
type File struct {
	Path string `json:"path"` // absolute
	Name string `json:"name"` // relative to the game folder, for the window
	What string `json:"what"`
	// loads: the game loads it (wer.dll, Dreadnought.dll); Disable renames
	// these.
	loads bool
}

const (
	modDLL      = "Dreadnought.dll"
	shimDLL     = "wer.dll"
	launcherExe = "DreadnoughtLauncher.exe"

	// Anything bigger is not one of the patch's tiny files (shim 13,824 B,
	// launcher 11,776 B); the cap also bounds what is read.
	maxShimBytes     = 256 << 10
	maxLauncherBytes = 1 << 20
)

// Known builds (sha256), the same in all three releases.
var (
	knownShims     = map[string]bool{"135684226faad1b6282831f1f41854bd4c1c4bc35a7e67e76fa23c85cd1b9486": true}
	knownLaunchers = map[string]bool{"3d3782230b0d805f92b329b7e7f41015dd32a3f94950ddb2a546bee9e12b08ee": true}
	// knownMods are the released Dreadnought.dll builds (v1, v2, v3); any
	// Dreadnought.dll in Win64 is the patch, these are only for the record.
	knownMods = map[string]string{
		"6d3e7de00e3603bbbfb083bba6ae10573f5292b9186c0550c34aba386002bb9a": "v1",
		"f44786403ba05af915171238f72418b83e58a49406ef2c41231514222c7029d8": "v2",
		"9594a9787c6bf796d0454e76996743f0a33060ba75686e51c1699128161c6bf1": "v3",
	}

	shimMarker     = []byte("Dreadnought.dll")
	launcherMarker = []byte(`DreadGame-Win64-Shipping.exe -log`)
)

// Find lists the patch's files in a game install. root is the folder that
// holds DreadGame (and DreadnoughtLauncher.exe), binDir the folder of
// DreadGame-Win64-Shipping.exe. Nothing found is an empty list.
func Find(root, binDir string) []File {
	var out []File
	rel := func(p string) string {
		if r, err := filepath.Rel(root, p); err == nil && root != "" {
			return r
		}
		return p
	}
	if binDir != "" {
		p := filepath.Join(binDir, modDLL)
		if regular(p) {
			what := "the patch's game mod"
			if v := knownMods[fileHash(p, 64<<20)]; v != "" {
				what += " (release " + v + ")"
			}
			out = append(out, File{Path: p, Name: rel(p), What: what, loads: true})
		}
		p = filepath.Join(binDir, shimDLL)
		if data, ok := smallFile(p, maxShimBytes); ok && (knownShims[sum(data)] || bytes.Contains(data, shimMarker)) {
			out = append(out, File{Path: p, Name: rel(p), What: "the patch's loader (it loads Dreadnought.dll into the game)", loads: true})
		}
	}
	if root != "" {
		p := filepath.Join(root, launcherExe)
		if data, ok := smallFile(p, maxLauncherBytes); ok && (knownLaunchers[sum(data)] || bytes.Contains(data, launcherMarker)) {
			out = append(out, File{Path: p, Name: rel(p), What: "the patch's replacement of the original Dreadnought launcher"})
		}
	}
	return out
}

// Remove deletes the files Find reported, checking each again first so only
// the patch's own files go. removed and failed hold File.Name values; err
// joins the reasons.
func Remove(root, binDir string) (removed, failed []string, err error) {
	var errs []error
	for _, f := range Find(root, binDir) {
		if e := os.Remove(f.Path); e != nil && !errors.Is(e, os.ErrNotExist) {
			failed = append(failed, f.Name)
			errs = append(errs, e)
			continue
		}
		removed = append(removed, f.Name)
	}
	return removed, failed, errors.Join(errs...)
}

// OffSuffix is added to the patch's DLLs while one of this launcher's games
// starts, for players who keep the patch.
const OffSuffix = ".dn-revival-off"

// Disable renames the patch's DLLs (name + OffSuffix) so the game started
// next does not load them; Restore puts the names back. wer.dll is a static
// import of DreadGame-Win64-Shipping.exe (its import table, checked
// 2026-11-26), so Windows resolves it while the process starts: once the
// game has loaded a wer.dll (Windows' own, with the patch's renamed) the
// names can go back while the game runs. On a partial failure what was
// renamed is put back and err says why.
func Disable(root, binDir string) (disabled []string, err error) {
	for _, f := range Find(root, binDir) {
		if !f.loads {
			continue
		}
		off := f.Path + OffSuffix
		if _, e := os.Lstat(off); e == nil {
			// An older renamed copy (e.g. the launcher stopped before it
			// restored): it would block the rename; the live file wins.
			if e := os.Remove(off); e != nil {
				Restore(binDir)
				return nil, e
			}
		}
		if e := os.Rename(f.Path, off); e != nil {
			Restore(binDir)
			return nil, e
		}
		disabled = append(disabled, f.Name)
	}
	return disabled, nil
}

// Restore gives the patch's renamed DLLs their names back. A name that is
// taken again (the player reinstalled the patch meanwhile) keeps the new
// file; the renamed copy is then left as it is.
func Restore(binDir string) (restored []string, err error) {
	if binDir == "" {
		return nil, nil
	}
	var errs []error
	for _, name := range []string{shimDLL, modDLL} {
		p := filepath.Join(binDir, name)
		off := p + OffSuffix
		if !regular(off) {
			continue
		}
		if _, e := os.Lstat(p); e == nil {
			continue
		}
		if e := os.Rename(off, p); e != nil {
			errs = append(errs, e)
			continue
		}
		restored = append(restored, name)
	}
	return restored, errors.Join(errs...)
}

// Disabled reports whether any of the patch's DLLs is renamed right now.
func Disabled(binDir string) bool {
	return binDir != "" && (regular(filepath.Join(binDir, shimDLL+OffSuffix)) || regular(filepath.Join(binDir, modDLL+OffSuffix)))
}

func regular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// smallFile reads p when it is a regular file of at most limit bytes.
func smallFile(p string, limit int64) ([]byte, bool) {
	st, err := os.Stat(p)
	if err != nil || !st.Mode().IsRegular() || st.Size() > limit {
		return nil, false
	}
	data, err := os.ReadFile(p)
	return data, err == nil
}

func sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func fileHash(p string, limit int64) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, limit)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
