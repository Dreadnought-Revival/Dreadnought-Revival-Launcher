//go:build windows

package main

// The launcher's actions, shared by both front ends: the WebView2 window
// (desktop_windows.go binds these as JS functions) and the browser version
// (browser_windows.go serves the SAME page and forwards the same calls over a
// loopback HTTP API). One page and one set of actions, so the two cannot
// drift apart -- the browser version used to be sign-in only, with no game
// folder, options or certificate screen.

import (
	"fmt"
	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/crashupload"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/windows"
)

type launcherAPI struct {
	exeDir string
	cfg    Config
	// owner is the window that owns native dialogs (folder picker); 0 in the
	// browser version.
	owner func() uintptr

	mu       sync.Mutex
	token    string
	username string
	// saved is the stored sign-in; with a remembered password (saved.Password)
	// the player counts as signed in even after token expired, and Play signs
	// in again first. saved.Identifier fills the sign-in form otherwise.
	saved storedCredentials
	// game is the game process Play started and gameStarted when; WaitGame
	// waits for it.
	game        *os.Process
	gameStarted time.Time

	// netMu keeps a server switch (servers_windows.go) from running while the
	// news or Play is using the server setup, and the other way round.
	netMu sync.Mutex
}

func newLauncherAPI(exeDir string, cfg Config, owner func() uintptr) *launcherAPI {
	a := &launcherAPI{exeDir: exeDir, cfg: cfg, owner: owner}
	if !signOutRequested() {
		a.loadSaved()
	}
	return a
}

// loadSaved takes the stored sign-in of the current server. Callers hold a.mu
// or own a.
func (a *launcherAPI) loadSaved() {
	a.token, a.username, a.saved = "", "", storedCredentials{}
	creds, ok := loadCredentials()
	if !ok {
		return
	}
	a.saved = creds
	if !launcherTokenExpired(creds.Token) || creds.Password != "" {
		a.token, a.username = creds.Token, creds.Username
	}
}

func (a *launcherAPI) Init() map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	server := a.cfg.Server
	if server == "" {
		server = a.cfg.GatewayIP
	}
	settings := loadSettings()
	out := map[string]any{"signedIn": a.username != "", "username": a.username,
		"identifier": a.saved.Identifier, "server": server,
		"serverIP": a.serverIP(), "game": gameInfo(a.exeDir, a.cfg),
		"logWindow": settings.LogWindow, "verboseLog": settings.VerboseLog, "logUpload": !settings.NoLogUpload,
		"closeOnPlay": settings.CloseOnPlay, "trayMinimize": settings.TrayMinimize, "version": launcherVersion,
		"servers": a.serverInfo()}
	machineMu.RLock()
	ca, caSource := pendingCA, pendingCASource
	machineMu.RUnlock()
	if ca != nil && !caTrusted(ca) {
		name, fp := caSummary(ca)
		out["cert"] = map[string]any{"name": name, "fingerprint": fp, "source": caSource, "server": currentServerName}
	}
	return out
}

// PickGame shows Windows' folder picker. The choice is kept per Windows user
// (settings.json).
func (a *launcherAPI) PickGame() map[string]any {
	var owner uintptr
	if a.owner != nil {
		owner = a.owner()
	}
	dir, ok := pickFolder(owner, "Select the folder where Dreadnought is installed")
	if !ok {
		return map[string]any{"ok": false}
	}
	if gameBinaryIn(dir) == "" {
		return map[string]any{"ok": false, "error": "Dreadnought was not found in " + dir +
			". Pick the folder that contains the DreadGame folder."}
	}
	return a.setGameDir(dir)
}

// SetGameDir takes a typed path (the browser version's text field).
func (a *launcherAPI) SetGameDir(dir string) map[string]any {
	dir = strings.Trim(strings.TrimSpace(dir), `"`)
	// Not a network share: the game is started from this folder, and a typed
	// path is also what a hostile page would send (audit 2026-11-17). A share
	// can still be chosen in the folder picker, which only the player drives.
	if networkPath(dir) {
		return map[string]any{"ok": false, "error": "Enter a folder on this PC, not a network path (" + dir + ")."}
	}
	if gameBinaryIn(dir) == "" {
		return map[string]any{"ok": false, "error": "Dreadnought was not found in " + dir +
			". Enter the folder that contains the DreadGame folder."}
	}
	return a.setGameDir(dir)
}

func (a *launcherAPI) setGameDir(dir string) map[string]any {
	if err := updateSettings(func(s *launcherSettings) bool { s.GameDir = dir; return true }); err != nil {
		return map[string]any{"ok": false, "error": "Could not save the setting: " + err.Error()}
	}
	return map[string]any{"ok": true, "game": gameInfo(a.exeDir, a.cfg)}
}

// AutoGame goes back to automatic detection: forget the chosen folder.
func (a *launcherAPI) AutoGame() map[string]any {
	if err := updateSettings(func(s *launcherSettings) bool { s.GameDir = ""; return true }); err != nil {
		return map[string]any{"ok": false, "error": "Could not save the setting: " + err.Error()}
	}
	return map[string]any{"ok": true, "game": gameInfo(a.exeDir, a.cfg)}
}

// SetOption saves a toggle, per Windows user.
func (a *launcherAPI) SetOption(name string, on bool) bool {
	known := true
	err := updateSettings(func(settings *launcherSettings) bool {
		switch name {
		case "logWindow":
			settings.LogWindow = on
		case "verboseLog":
			settings.VerboseLog = on
		case "logUpload":
			settings.NoLogUpload = !on
		case "closeOnPlay":
			settings.CloseOnPlay = on
		case "trayMinimize":
			settings.TrayMinimize = on
		default:
			known = false
		}
		return known
	})
	return known && err == nil
}

func (a *launcherAPI) InstallCert() map[string]any {
	machineMu.RLock()
	ca := pendingCA
	machineMu.RUnlock()
	if ca == nil || caTrusted(ca) {
		return map[string]any{"ok": true}
	}
	if err := installCA(ca); err != nil || !caTrusted(ca) {
		return map[string]any{"ok": false, "error": "The certificate was not installed. " +
			"Windows asks you to confirm it -- choose Yes to continue."}
	}
	noteCertUsed(ca, currentServerName) // starts its 30 days (certs_windows.go)
	return map[string]any{"ok": true}
}

// currentCA is the CA of the server in use (nil without one).
func currentCA() []byte {
	machineMu.RLock()
	defer machineMu.RUnlock()
	return pendingCA
}

// Submit signs in (or creates the account, then signs in). remember keeps the
// password, DPAPI-encrypted, so the launcher can sign in again by itself
// when the 24-hour token runs out.
func (a *launcherAPI) Submit(mode, username, identifier, password string, remember bool) map[string]any {
	identifier, username = strings.TrimSpace(identifier), strings.TrimSpace(username)
	fail := func(msg string) map[string]any { return map[string]any{"ok": false, "error": msg} }
	if identifier == "" || password == "" {
		return fail("Fill in every field.")
	}
	if mode == "register" {
		if username == "" {
			return fail("Pick a callsign.")
		}
		if len(password) < 6 {
			return fail("Use at least 6 characters for the password.")
		}
		if err := registerAccount(a.cfg.AuthURL, username, identifier, password); err != nil {
			return fail(capitalise(err.Error()))
		}
	}
	creds, err := loginAccount(a.cfg.AuthURL, identifier, password)
	if err != nil {
		return fail(capitalise(err.Error()))
	}
	if remember {
		creds.Password = password
	}
	if err := saveCredentials(creds); err != nil {
		fmt.Printf("[!] Could not remember this sign-in (%v)\n", err)
	}
	a.mu.Lock()
	a.token, a.username, a.saved = creds.Token, creds.Username, creds
	a.mu.Unlock()
	return map[string]any{"ok": true, "username": creds.Username}
}

func (a *launcherAPI) SignOut() {
	clearCredentials()
	a.mu.Lock()
	a.token, a.username, a.saved = "", "", storedCredentials{}
	a.mu.Unlock()
}

// serverIP is the address the launcher and the game use: the last DNS answer
// with a server setting, else gateway_ip.
func (a *launcherAPI) serverIP() string {
	if strings.TrimSpace(a.cfg.Server) != "" {
		return dialIP()
	}
	return a.cfg.GatewayIP
}

func (a *launcherAPI) News() map[string]any {
	a.netMu.Lock()
	defer a.netMu.Unlock()
	// Looked up again, so the window shows the address Play will use and a
	// lookup that failed at start gets another chance.
	cfg := a.config()
	if _, err := refreshServerIP(&cfg); err != nil {
		return map[string]any{"online": false, "error": err.Error(), "serverIP": a.serverIP()}
	}
	tiles, err := fetchNews()
	if err != nil {
		return map[string]any{"online": false, "error": err.Error(), "serverIP": a.serverIP()}
	}
	noteCertUsed(currentCA(), currentServerName) // reached the server with it
	return map[string]any{"online": true, "tiles": tiles, "serverIP": a.serverIP()}
}

// PatchNotes is the project's patch notes feed (patchnotes.go) and the
// Patreon link, for the window's right-hand panel. Independent of the server:
// a server that cannot be reached still gets its notes.
func (a *launcherAPI) PatchNotes() map[string]any {
	notes, link, err := fetchPatchNotes()
	out := map[string]any{"patreon": patreonURL}
	if err != nil {
		fmt.Printf("[!] Patch notes not available: %v\n", err)
		out["error"] = err.Error()
		return out
	}
	out["notes"], out["link"] = notes, link
	return out
}

func (a *launcherAPI) config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// Assets is the original launcher's look for the page (origassets_windows.go),
// read from DreadnoughtLauncher.exe in the game's folder or the launcher's.
// No assets is not an error the page shows: it keeps its own styling.
func (a *launcherAPI) Assets() map[string]any {
	var dirs []string
	if p := findGameBinary(a.exeDir, a.config()); p != "" {
		dirs = append(dirs, gameInstallRoot(p))
	}
	dirs = append(dirs, a.exeDir)
	assets, path, err := originalAssets(dirs...)
	switch {
	case err != nil:
		fmt.Printf("[!] Original launcher look not available: %v\n", err)
		return map[string]any{"error": err.Error()}
	case assets == nil:
		fmt.Println("[*] No " + originalLauncherExe + " found; the launcher uses its own look.")
		return map[string]any{}
	}
	fmt.Printf("[*] Original launcher look from %s\n", path)
	return map[string]any{"assets": assets, "source": path}
}

// OpenURL opens a news tile's link in the default browser.
func (a *launcherAPI) OpenURL(url string) bool {
	if !strings.HasPrefix(url, "https://") && !strings.HasPrefix(url, "http://") {
		return false
	}
	openBrowser(url)
	return true
}

// Play starts the game. ok means the game was started; the front end then
// closes, or hides and waits for the game (WaitGame).
func (a *launcherAPI) Play() map[string]any {
	a.netMu.Lock()
	defer a.netMu.Unlock()
	// One game at a time: the page turns Play off while the game runs, but
	// signing in or switching server turned it on again (audit 2026-11-17).
	if a.GameRunning() {
		return map[string]any{"ok": false, "error": "Dreadnought is already running."}
	}
	a.mu.Lock()
	token, username := a.token, a.username
	a.mu.Unlock()
	if ca := currentCA(); ca != nil && !caTrusted(ca) {
		return map[string]any{"ok": false, "cert": true,
			"error": "Install the public testing certificate first; the game cannot connect without it."}
	}
	if findGameBinary(a.exeDir, a.config()) == "" {
		return map[string]any{"ok": false, "game": true,
			"error": "Dreadnought was not found. Choose your game folder above."}
	}
	if token == "" || launcherTokenExpired(token) {
		var err error
		if token, username, err = a.renew(); err != nil {
			if unreachable(err) {
				return map[string]any{"ok": false, "error": capitalise(err.Error()), "serverIP": a.serverIP()}
			}
			return map[string]any{"ok": false, "error": "Your sign-in has expired. Please sign in again.", "signIn": true}
		}
	}
	// The last session's log, if it was not sent when that game closed. Read
	// now, because starting the game replaces DreadGame.log; sent meanwhile.
	if p := a.prepareGameLog(gameSession{exitCode: -1, late: true}); p != nil {
		go func() {
			if err := p.send(token); err != nil {
				fmt.Printf("[!] Last game log not sent: %v\n", err)
			}
		}()
	}
	// Crash reports not sent yet (crashupload_windows.go).
	go a.uploadCrashReports(gameSession{exitCode: -1, late: true}, time.Now().Add(-crashUploadMaxAge), false)
	started := time.Now()
	game, err := startGame(a.exeDir, a.config(), token, username)
	if err != nil {
		return map[string]any{"ok": false, "error": capitalise(err.Error()), "serverIP": a.serverIP()}
	}
	a.mu.Lock()
	a.game, a.gameStarted = game, started
	a.mu.Unlock()
	noteCertUsed(currentCA(), currentServerName)
	return map[string]any{"ok": true, "serverIP": a.serverIP()}
}

// WaitGame blocks until the game Play started has closed and says how it
// went: its exit code and whether it looks like a crash -- a non-zero exit
// code, or a crash folder the game wrote while it ran. ok is false when no
// game was started.
func (a *launcherAPI) WaitGame() map[string]any {
	a.mu.Lock()
	game, started := a.game, a.gameStarted
	a.mu.Unlock()
	if game == nil {
		return map[string]any{"ok": false}
	}
	code := -1
	state, err := game.Wait()
	if err == nil {
		code = state.ExitCode()
	} else {
		fmt.Printf("[!] Waiting for the game failed: %v\n", err)
	}
	a.mu.Lock()
	if a.game == game {
		a.game = nil
	}
	a.mu.Unlock()
	crashFolder := newestCrashFolder(a.gameSavedFolders(), started)
	if crashFolder == "" {
		// The game's Windows Error Report (crashupload_windows.go): where
		// this build's crash handler actually leaves its report.
		if reports := crashupload.Find(a.crashRoots(), started); len(reports) > 0 {
			crashFolder = reports[0].Dir
		}
	}
	crashed := code != 0 || crashFolder != ""
	// A non-zero exit after the log's last line is the crash on exit every
	// quit has (gameLogClosedCleanly): the player saw a normal close.
	exitCrash := false
	if crashed {
		if dir := a.newestLogFolder(); dir != "" {
			p := filepath.Join(dir, "DreadGame.log")
			if info, err := os.Stat(p); err == nil && !info.ModTime().Before(started) && gameLogClosedCleanly(p) {
				crashed, exitCrash = false, true
			}
		}
	}
	how := ""
	if exitCrash {
		how = ", after a clean shutdown"
	}
	fmt.Printf("[*] The game closed (exit code %d%s) after %s\n", code, how, time.Since(started).Round(time.Second))
	go a.uploadGameLogAfterGame(gameSession{exitCode: code, crashed: crashed, seconds: int(time.Since(started).Seconds())})
	if crashed || exitCrash {
		go a.uploadCrashReportsAfterGame(gameSession{exitCode: code, crashed: crashed, seconds: int(time.Since(started).Seconds())}, started, exitCrash)
	}
	return map[string]any{"ok": true, "exitCode": code, "crashed": crashed, "exitCrash": exitCrash,
		"crashFolder": crashFolder, "seconds": int(time.Since(started).Seconds())}
}

// GameRunning reports whether the game Play started is still running.
func (a *launcherAPI) GameRunning() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.game != nil
}

// gameSavedFolders are the game's Saved folders (the parents of
// gameLogFolders).
func (a *launcherAPI) gameSavedFolders() []string {
	var dirs []string
	for _, logs := range a.gameLogFolders() {
		dirs = append(dirs, filepath.Dir(logs))
	}
	return dirs
}

// newestCrashFolder is the newest folder the game's crash reporter made since
// the given time: a folder under Saved\Crashes, or a "UE4CC-..." one under
// Saved\Logs. "" when there is none. These are where later Unreal Engine 4
// versions put a crash's dump and log; this build (4.13) has no "Crashes"
// literal and files a Windows Error Report instead (verified in the exe
// 2026-11-23, crashupload), which WaitGame looks for when this finds nothing.
func newestCrashFolder(savedDirs []string, since time.Time) string {
	best, bestTime := "", time.Time{}
	for _, saved := range savedDirs {
		for _, sub := range []string{"Crashes", "Logs"} {
			entries, err := os.ReadDir(filepath.Join(saved, sub))
			if err != nil {
				continue
			}
			for _, e := range entries {
				if !e.IsDir() || (sub == "Logs" && !strings.HasPrefix(e.Name(), "UE4CC")) {
					continue
				}
				info, err := e.Info()
				if err != nil || info.ModTime().Before(since) || !info.ModTime().After(bestTime) {
					continue
				}
				best, bestTime = filepath.Join(saved, sub, e.Name()), info.ModTime()
			}
		}
	}
	return best
}

// renew signs in again with the remembered password. A refusal (password
// changed, account gone) forgets the password; an unreachable server does not.
func (a *launcherAPI) renew() (token, username string, err error) {
	a.mu.Lock()
	saved, authURL := a.saved, a.cfg.AuthURL
	a.mu.Unlock()
	creds, err := renewCredentials(authURL, saved)
	if err != nil {
		if saved.Password != "" && !unreachable(err) {
			fmt.Printf("[!] The remembered password was refused (%v); sign in again.\n", err)
			saved.Password = ""
			_ = saveCredentials(saved)
			a.mu.Lock()
			a.saved = saved
			a.mu.Unlock()
		}
		return "", "", err
	}
	a.mu.Lock()
	a.token, a.username, a.saved = creds.Token, creds.Username, creds
	a.mu.Unlock()
	return creds.Token, creds.Username, nil
}

// gameLogFolders are where the game may write DreadGame.log: the per-user
// Saved folder of a shipping build, then the Saved folder inside the install.
func (a *launcherAPI) gameLogFolders() []string {
	var dirs []string
	if la := os.Getenv("LOCALAPPDATA"); la != "" {
		dirs = append(dirs, filepath.Join(la, "DreadGame", "Saved", "Logs"))
	}
	if exe := findGameBinary(a.exeDir, a.cfg); exe != "" {
		dirs = append(dirs, filepath.Join(gameInstallRoot(exe), "DreadGame", "Saved", "Logs"))
	}
	return dirs
}

// OpenLogs opens the folder holding the game's newest log in Explorer, so a
// player can send it with a bug report.
func (a *launcherAPI) OpenLogs() map[string]any {
	best := a.newestLogFolder()
	if best == "" {
		return map[string]any{"ok": false, "error": "No game logs yet. Start the game once, then try again."}
	}
	verb, _ := windows.UTF16PtrFromString("open")
	target, _ := windows.UTF16PtrFromString(best)
	if err := windows.ShellExecute(0, verb, target, nil, nil, windows.SW_SHOWNORMAL); err != nil {
		return map[string]any{"ok": false, "error": "Could not open " + best + ": " + err.Error()}
	}
	return map[string]any{"ok": true, "path": best}
}

// newestLogFolder is the log folder whose DreadGame.log is newest, else the
// first log folder that exists, else "".
func (a *launcherAPI) newestLogFolder() string {
	best, bestTime := "", time.Time{}
	for _, dir := range a.gameLogFolders() {
		info, err := os.Stat(filepath.Join(dir, "DreadGame.log"))
		if err == nil && info.ModTime().After(bestTime) {
			best, bestTime = dir, info.ModTime()
		} else if best == "" {
			if st, err := os.Stat(dir); err == nil && st.IsDir() {
				best = dir
			}
		}
	}
	return best
}

// networkPath: a UNC path (\\server\share, //server/share), including the
// \\?\UNC\ form.
func networkPath(dir string) bool {
	d := strings.ReplaceAll(dir, "/", `\`)
	return strings.HasPrefix(d, `\\`) && !strings.HasPrefix(strings.ToUpper(d), `\\?\`) ||
		strings.HasPrefix(strings.ToUpper(d), `\\?\UNC\`)
}
