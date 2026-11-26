//go:build windows

package main

// gwog's "Dreadnought Revival Patch" (modcheck/) is not compatible with this
// launcher's games (operator, 2026-11-26). The window asks the player once:
//
//   - Uninstall: the patch's files are deleted.
//   - Keep (remembered, settings.json): the patch stays for the player's
//     other use, but is off for this launcher's games. Play renames its two
//     DLLs, starts the game, watches the game until Windows has loaded its
//     wer.dll (a static import, so this happens while the process starts,
//     before any game code), and gives the names back -- within seconds,
//     while the game runs, also when the launcher closes on Play.
//
// A rename left over (the launcher stopped in between) is put back when the
// launcher starts and when the game closes. Settings offers Uninstall later.

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/modcheck"
	"golang.org/x/sys/windows"
)

// gwogLoadWait is how long Play waits for the game to load its wer.dll
// before it gives the names back anyway.
const gwogLoadWait = 60 * time.Second

// gwogModFiles are the patch's files in the game install the launcher uses.
func gwogModFiles(exeDir string, cfg Config) (root, bin string, files []modcheck.File) {
	p, _ := gameLocation(exeDir, cfg)
	if p == "" {
		return "", "", nil
	}
	root, bin = gameInstallRoot(p), filepath.Dir(p)
	return root, bin, modcheck.Find(root, bin)
}

// gwogModInfo is the list for the window (nil when the patch is not there).
func gwogModInfo(exeDir string, cfg Config) []modcheck.File {
	_, _, files := gwogModFiles(exeDir, cfg)
	return files
}

// restoreGwogMod puts back names a stopped launcher left renamed. Not while
// a game is starting (Play gives them back itself).
func restoreGwogMod(exeDir string, cfg Config) {
	p, _ := gameLocation(exeDir, cfg)
	if p == "" || !modcheck.Disabled(filepath.Dir(p)) {
		return
	}
	if r, err := modcheck.Restore(filepath.Dir(p)); err != nil {
		fmt.Printf("[!] gwog patch: could not give back the names of %v: %v\n", r, err)
	} else if len(r) > 0 {
		fmt.Printf("[*] gwog patch: names given back: %s\n", strings.Join(r, ", "))
	}
}

// RemoveGwogMod deletes the patch's files after the player chose Uninstall.
func (a *launcherAPI) RemoveGwogMod() map[string]any {
	if a.GameRunning() {
		return map[string]any{"ok": false, "error": "Close Dreadnought first, then try again."}
	}
	restoreGwogMod(a.exeDir, a.config())
	root, bin, files := gwogModFiles(a.exeDir, a.config())
	if len(files) == 0 {
		return map[string]any{"ok": true, "removed": []string{}, "game": gameInfo(a.exeDir, a.config())}
	}
	removed, failed, err := modcheck.Remove(root, bin)
	_ = updateSettings(func(s *launcherSettings) bool { s.GwogKeep = false; return true })
	out := map[string]any{"ok": err == nil, "removed": removed, "failed": failed, "game": gameInfo(a.exeDir, a.config())}
	for _, name := range removed {
		if strings.EqualFold(filepath.Base(name), "DreadnoughtLauncher.exe") {
			// The patch replaced the original launcher; Steam puts it back
			// (and with it this launcher's original look).
			out["steamVerify"] = true
		}
	}
	if err != nil {
		out["error"] = "Could not remove " + strings.Join(failed, ", ") +
			". Close Dreadnought and Steam and try again (" + err.Error() + ")."
	}
	return out
}

// KeepGwogMod remembers the player's Keep.
func (a *launcherAPI) KeepGwogMod() map[string]any {
	if err := updateSettings(func(s *launcherSettings) bool { s.GwogKeep = true; return true }); err != nil {
		return map[string]any{"ok": false, "error": "Could not save the choice: " + err.Error()}
	}
	return map[string]any{"ok": true, "game": gameInfo(a.exeDir, a.config())}
}

// gwogBeforeStart turns the kept patch off for the game Play starts. after
// gets the started game (nil when it did not start) and gives the names back.
func gwogBeforeStart(exeDir string, cfg Config) (after func(pid int), err error) {
	root, bin, files := gwogModFiles(exeDir, cfg)
	if len(files) == 0 {
		return func(int) {}, nil
	}
	disabled, err := modcheck.Disable(root, bin)
	if err != nil {
		return nil, err
	}
	if len(disabled) == 0 {
		return func(int) {}, nil
	}
	fmt.Printf("[*] gwog patch: off for this game: %s\n", strings.Join(disabled, ", "))
	return func(pid int) {
		if pid > 0 {
			if path, ok := waitForModule(uint32(pid), "wer.dll", gwogLoadWait); ok {
				fmt.Printf("[*] gwog patch: the game loaded %s\n", path)
			} else {
				fmt.Println("[!] gwog patch: did not see the game load wer.dll; giving the names back anyway")
			}
		}
		if r, err := modcheck.Restore(bin); err != nil {
			fmt.Printf("[!] gwog patch: could not give back the names (%v): %v\n", r, err)
		} else {
			fmt.Printf("[*] gwog patch: names given back: %s\n", strings.Join(r, ", "))
		}
	}, nil
}

// waitForModule waits until process pid has loaded a module called name and
// returns its path; false when the process ended or timeout passed first.
// Right after CreateProcess the module list may not be readable yet, so it
// is tried again every 100 ms.
func waitForModule(pid uint32, name string, timeout time.Duration) (string, bool) {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(h)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if path, ok := moduleLoaded(pid, name); ok {
			return path, true
		}
		if ev, _ := windows.WaitForSingleObject(h, 100); ev == windows.WAIT_OBJECT_0 {
			return "", false // the game ended
		}
	}
	return "", false
}

func moduleLoaded(pid uint32, name string) (string, bool) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPMODULE|windows.TH32CS_SNAPMODULE32, pid)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(snap)
	var me windows.ModuleEntry32
	me.Size = uint32(unsafe.Sizeof(me))
	for err = windows.Module32First(snap, &me); err == nil; err = windows.Module32Next(snap, &me) {
		if strings.EqualFold(windows.UTF16ToString(me.Module[:]), name) {
			return windows.UTF16ToString(me.ExePath[:]), true
		}
	}
	return "", false
}
