//go:build windows

package main

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows/registry"
)

// ---- TLS configuration --------------------------------------------------

func buildTLSConfig() *tls.Config {
	fingerprint := strings.TrimSpace(os.Getenv("TLS_CERT_FINGERPRINT"))
	if fingerprint == "" {
		// No CA of the server's and no pinned fingerprint: Windows' own trust
		// store decides (the server's CA is in it once installed, which the game
		// needs anyway). CHANGED 2026-11-17 (audit): this skipped verification
		// altogether, so the sign-in -- password included -- went to whoever
		// answered. Only setups without a CA reach here; the release always has
		// the server list's.
		return &tls.Config{MinVersion: tls.VersionTLS12}
	}
	expected := strings.ToLower(strings.ReplaceAll(fingerprint, ":", ""))
	return &tls.Config{
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			for _, raw := range rawCerts {
				cert, err := x509.ParseCertificate(raw)
				if err != nil {
					continue
				}
				sum := sha256.Sum256(cert.Raw)
				actual := hex.EncodeToString(sum[:])
				if strings.ToLower(actual) == expected {
					return nil
				}
			}
			return fmt.Errorf("server certificate fingerprint does not match TLS_CERT_FINGERPRINT")
		},
	}
}

// ---- DPAPI (Windows CryptProtectData) --------------------------------

var (
	crypt32dll         = syscall.NewLazyDLL("crypt32.dll")
	kernel32dll        = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtect   = crypt32dll.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32dll.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32dll.NewProc("LocalFree")
)

type dataBlob struct {
	cbData uint32
	pbData *byte
}

// dpapiEncrypt wraps CryptProtectData (user scope, no UI).
func dpapiEncrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	if len(data) > int(^uint32(0)) {
		return nil, fmt.Errorf("data too large")
	}
	inBlob := dataBlob{
		//nolint:gosec // Length is bounded above and DPAPI requires a uint32 byte count.
		cbData: uint32(len(data)),
		pbData: &data[0],
	}
	var outBlob dataBlob
	const CRYPTPROTECT_UI_FORBIDDEN = 1
	ret, _, _ := procCryptProtect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, // szDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		CRYPTPROTECT_UI_FORBIDDEN,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", syscall.GetLastError())
	}
	result := make([]byte, outBlob.cbData)
	copy(result, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	return result, nil
}

// dpapiDecrypt reverses dpapiEncrypt. It fails for a blob written by a
// different Windows user, which is the point: the launcher's stored session
// token is readable only by the account that signed in.
func dpapiDecrypt(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty data")
	}
	if len(data) > int(^uint32(0)) {
		return nil, fmt.Errorf("data too large")
	}
	inBlob := dataBlob{
		//nolint:gosec // Length is bounded above and DPAPI requires a uint32 byte count.
		cbData: uint32(len(data)),
		pbData: &data[0],
	}
	var outBlob dataBlob
	const CRYPTPROTECT_UI_FORBIDDEN = 1
	ret, _, _ := procCryptUnprotect.Call(
		uintptr(unsafe.Pointer(&inBlob)),
		0, // ppszDataDescr
		0, // pOptionalEntropy
		0, // pvReserved
		0, // pPromptStruct
		CRYPTPROTECT_UI_FORBIDDEN,
		uintptr(unsafe.Pointer(&outBlob)),
	)
	if ret == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", syscall.GetLastError())
	}
	result := make([]byte, outBlob.cbData)
	copy(result, unsafe.Slice(outBlob.pbData, outBlob.cbData))
	_, _, _ = procLocalFree.Call(uintptr(unsafe.Pointer(outBlob.pbData)))
	return result, nil
}

// ---- Sign-in response shapes -------------------------------------------

// authResponse is the JSON-RPC envelope a sign-in reply may come in
// (loginAccount falls back to it).
type authResponse struct {
	ID      string            `json:"id"`
	JSONRPC string            `json:"jsonrpc"`
	Result  []json.RawMessage `json:"result"`
}

type userObj struct {
	JWT         string `json:"jwt"`
	AccessToken string `json:"access_token"`
	Token       string `json:"token"`
	UserID      string `json:"user_id"`
	Username    string `json:"username"`
}

// ---- Registry --------------------------------------------------------

const (
	regPath      = `SOFTWARE\Grey Box\Dreadnought`
	regAuthToken = "AuthToken"
)

func writeAuthToken(jwtToken string) error {
	encrypted, err := dpapiEncrypt([]byte(jwtToken))
	if err != nil {
		return fmt.Errorf("DPAPI encrypt: %w", err)
	}
	k, _, err := registry.CreateKey(registry.CURRENT_USER, regPath, registry.SET_VALUE)
	if err != nil {
		return fmt.Errorf("create registry key: %w", err)
	}
	defer func() {
		_ = k.Close()
	}()
	return k.SetBinaryValue(regAuthToken, encrypted)
}

// ---- Config ----------------------------------------------------------

type Config struct {
	// Server is the public hostname or IP of the private server. When set, the
	// launcher points the game at it and does the only machine setup needed (trusting the
	// CA certificate; no hosts file, no admin) -- see setup_windows.go. This is the one setting a
	// distributed launcher needs.
	Server string `json:"server"`
	// WebPort is the outside TCP port that reaches the server's HTTPS gateway
	// (launcher sign-in and news) when it is not 443 -- e.g. a router mapping
	// 8443 -> 443 because 443 is taken. Only the launcher uses that gateway;
	// the game's ports (65443, 48843, UDP 7777-7877) are unaffected.
	WebPort       string `json:"web_port"`
	AuthURL       string `json:"auth_url"`
	GatewayIP     string `json:"gateway_ip"`
	GatewayPort   string `json:"gateway_port"`
	FirmamentHost string `json:"firmament_host"`
	FirmamentPort string `json:"firmament_port"`
	GamePath      string `json:"game_path"`
	// detectedGamePath comes from DN_GAME_PATH (the Linux wrapper's find). It
	// ranks BELOW a folder chosen in the launcher window, unlike game_path.
	detectedGamePath string
	VerboseLogging   bool `json:"verbose_logging"`
	// LogWindow opens the game's log console (-LOG); also a toggle in the
	// launcher window, and DN_LOG_WINDOW=1.
	LogWindow      bool `json:"log_window"`
	SkipOnboarding bool `json:"skip_onboarding"`
	// AllowSteam drops the -NoSteam switch, letting the client keep its Steam
	// online subsystem alive. See the note where the switch is built.
	AllowSteam bool `json:"allow_steam"`
	// SkipUpdateCheck: do not ask GitHub for a newer launcher at start
	// (update_windows.go).
	SkipUpdateCheck bool `json:"skip_update_check"`
	// ServerListURL replaces the server list's download address
	// (serverlist.go); "none" turns the download off.
	ServerListURL string `json:"server_list_url"`
	// The CA of a server picked in the launcher window (servers_windows.go):
	// caDER from the server list, or caFile kept from the server; neither =
	// ca.crt beside the launcher / built-in. caSource says which.
	caDER    []byte
	caFile   string
	caSource string
}

// defaultServer is the server a distributed launcher points at when its
// dn-launcher.json does not say otherwise. Set at build time:
//
//	go build -ldflags "-X main.defaultServer=play.example.org" ./dn-launcher
var defaultServer = ""

// defaultWebPort goes with defaultServer: -X main.defaultWebPort=8443.
var defaultWebPort = ""

// launcherVersion is set by the release build (-X main.launcherVersion=1.0.N,
// .github/workflows/release.yml: VERSION plus the build number); "dev" for a
// local build. Shown in the console banner and the window's footer.
var launcherVersion = "dev"

// afterUpdateEnv is set for the launcher an update starts in place of itself
// (update_windows.go): the old one is still closing, and must not be taken
// for a launcher that is already running.
const afterUpdateEnv = "DN_AFTER_UPDATE"

func defaultConfig() Config {
	return Config{
		Server:        defaultServer,
		WebPort:       defaultWebPort,
		AuthURL:       "https://profile-api.prod.greybox.sixfoot.live/auth/",
		GatewayIP:     "10.0.0.73",
		GatewayPort:   "65443",
		FirmamentHost: "", // empty = use GatewayIP (avoids need for hosts file entry)
		FirmamentPort: "48843",
	}
}

func loadConfig(exeDir string) Config {
	cfg := defaultConfig()
	configPath := filepath.Join(exeDir, "dn-launcher.json")
	//nolint:gosec // Path is the launcher's sibling config file in its own installation directory.
	data, err := os.ReadFile(configPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			// A config that exists but does not parse silently reverted every
			// setting to its default, which is indistinguishable from having
			// no config at all. Say so.
			fmt.Fprintf(os.Stderr, "[!] %s is not valid JSON (%v); using defaults for everything.\n",
				configPath, err)
			return cfg
		}
		fmt.Printf("[+] Config: %s\n", configPath)
	default:
		// Not an error -- the defaults are a working configuration -- but the
		// operator who just edited a file somewhere else needs to know this is
		// the path that counts.
		fmt.Printf("[+] Config: none at %s, using defaults\n", configPath)
	}
	// DN_VERBOSE_LOG lets verbose logging be toggled per-launch (e.g. for a
	// one-off debugging session) without editing dn-launcher.json.
	if v := strings.TrimSpace(os.Getenv("DN_VERBOSE_LOG")); v != "" && v != "0" {
		cfg.VerboseLogging = true
	}
	// DN_SKIP_ONBOARDING is a debugging escape hatch that jumps straight to the
	// hangar. Onboarding is on by default so new players get the same first-run
	// experience they had on the live servers.
	// DN_GAME_PATH points at the game executable (a Windows path). Set by the
	// Linux wrapper (dn-launcher-linux.sh), which runs this launcher inside the
	// game's Wine/Proton prefix while the game lives outside the launcher's
	// folder.
	if v := strings.TrimSpace(os.Getenv("DN_GAME_PATH")); v != "" {
		cfg.detectedGamePath = v
	}
	if v := strings.TrimSpace(os.Getenv("DN_LOG_WINDOW")); v != "" && v != "0" {
		cfg.LogWindow = true
	}
	if v := strings.TrimSpace(os.Getenv("DN_SKIP_ONBOARDING")); v != "" && v != "0" {
		cfg.SkipOnboarding = true
	}
	if v := strings.TrimSpace(os.Getenv("DN_ALLOW_STEAM")); v != "" && v != "0" {
		cfg.AllowSteam = true
	}
	// A command-line flag as well, because the other two routes both have a
	// trap an operator hit for real: `set DN_ALLOW_STEAM=1` in PowerShell is
	// Set-Variable and never reaches the environment (it needs
	// $env:DN_ALLOW_STEAM=1), and dn-launcher.json has to sit beside the
	// executable rather than beside the game. A flag has neither problem.
	if flagRequested("allow-steam", "allowsteam", "steam") {
		cfg.AllowSteam = true
	}
	return cfg
}

// ---- Game binary detection -------------------------------------------

// Where the game came from, for the launcher window.
const (
	gameSourceConfig = "dn-launcher.json"
	gameSourceChosen = "chosen"
	gameSourceLocal  = "launcher folder"
	gameSourceSteam  = "Steam"
	gameSourceLinux  = "Linux launcher"
)

// gameLocation finds the game executable and says how it was found. Order: a
// game_path in dn-launcher.json, then the folder chosen in the launcher window,
// then DN_GAME_PATH (what dn-launcher-linux.sh found), then the launcher's own
// folder, then Steam's libraries.
// A manual choice therefore always wins over detection, and "Use automatic
// detection" in the window clears it.
func gameLocation(exeDir string, cfg Config) (path, source string) {
	if cfg.GamePath != "" {
		if _, err := os.Stat(cfg.GamePath); err == nil {
			return cfg.GamePath, gameSourceConfig
		}
	}
	if p := gameBinaryIn(loadSettings().GameDir); p != "" {
		return p, gameSourceChosen
	}
	if cfg.detectedGamePath != "" {
		if _, err := os.Stat(cfg.detectedGamePath); err == nil {
			return cfg.detectedGamePath, gameSourceLinux
		}
	}
	if p := gameBinaryIn(exeDir); p != "" {
		return p, gameSourceLocal
	}
	if p := gameBinaryIn(steamGameDir(steamLibraries(steamRoots()))); p != "" {
		return p, gameSourceSteam
	}
	if p := gameBinaryIn(steamUninstallDir()); p != "" {
		return p, gameSourceSteam
	}
	return "", ""
}

func findGameBinary(exeDir string, cfg Config) string {
	p, _ := gameLocation(exeDir, cfg)
	return p
}

// ---- Main ------------------------------------------------------------

func main() {
	fmt.Println("=================================================")
	fmt.Println("  Dreadnought Revival launcher " + launcherVersion)
	fmt.Println("=================================================")
	fmt.Println()

	exePath, _ := os.Executable()
	exeDir := filepath.Dir(exePath)
	removeOldExe() // left by an update (update_windows.go)

	// --remove-certs: take every server certificate the launcher knows of out
	// of the Trusted Root store (certs_windows.go), e.g. from an uninstaller.
	if flagRequested("remove-certs") {
		ensureConsole()
		if removeAllCerts() > 0 {
			os.Exit(1)
		}
		return
	}

	cfg, err := applySavedServer(exeDir, loadConfig(exeDir))
	if err != nil {
		fatalf("[!] Setting up this PC for the server failed: %v", err)
	}

	// The launcher page: in the desktop window (WebView2), or -- without the
	// WebView2 runtime, under Wine (dn-launcher-linux.sh), or with --console --
	// the SAME page in the default browser (browser_windows.go). Both offer
	// sign-in, the certificate, the game folder, the options and Play.
	if user, pass := os.Getenv("DN_USERNAME"), os.Getenv("DN_PASSWORD"); user != "" && pass != "" {
		runUnattended(exeDir, cfg, user, pass)
		return
	}
	if !flagRequested("console") && runDesktopLauncher(exeDir, cfg) {
		return
	}
	ensureConsole()
	if !flagRequested("console") {
		fmt.Println("[*] The desktop window needs Microsoft Edge WebView2, which is not installed; opening the launcher in your web browser instead.")
	}
	if signOutRequested() {
		clearCredentials()
	}
	if err := runBrowserLauncher(exeDir, cfg); err != nil {
		fatalf("[!] %v", err)
	}
}

// runUnattended signs in with DN_USERNAME / DN_PASSWORD and starts the game,
// with no window: for automation (the server repo's scripts/wine-client.sh).
// REPLACES 2026-11-17 the pinned identity (player_id / DN_PLAYER_ID), which
// signed in through the server's Steam-ticket route -- an unverified ticket
// built from the PC and Windows user names. That route is gone.
func runUnattended(exeDir string, cfg Config, user, pass string) {
	ensureConsole()
	if pendingCA != nil {
		if err := ensureCAInstalled(pendingCA); err != nil {
			fatalf("[!] %v", err)
		}
	}
	fmt.Printf("[*] Signing in as %s ...\n", user)
	creds, err := loginAccount(cfg.AuthURL, user, pass)
	if err != nil {
		fatalf("[!] Auth failed: %v", err)
	}
	fmt.Printf("[+] Authenticated as: %s\n", creds.Username)
	if _, err := startGame(exeDir, cfg, creds.Token, creds.Username); err != nil {
		fatalf("[!] %v", err)
	}
}

// startGame hands the token to the game (registry, DPAPI) and starts it with
// the server's addresses. It returns the game's process, which the desktop
// window waits on. Shared by the console flow and the desktop window.
func startGame(exeDir string, cfg Config, jwtToken, username string) (*os.Process, error) {
	// The server's name is looked up again right before the game starts: its
	// outside IP may have changed since the launcher was opened.
	if ip, err := refreshServerIP(&cfg); err != nil {
		return nil, fmt.Errorf("could not look up the server %s: %w", cfg.Server, err)
	} else if cfg.Server != "" {
		fmt.Printf("[*] Server: %s (%s)\n", cfg.Server, ip)
	}
	fmt.Println("[*] Writing auth token to registry...")
	if err := writeAuthToken(jwtToken); err != nil {
		return nil, fmt.Errorf("could not store the sign-in for the game: %w", err)
	}
	fmt.Println("[+] Auth token written.")
	// The player's clock, so the server's chat lines show their local time
	// (clock_windows.go). Short timeout: it must never hold up the game.
	clockClient := launcherHTTPClient()
	clockClient.Timeout = 5 * time.Second
	if err := reportClock(clockClient, clockURL, jwtToken); err != nil {
		fmt.Printf("[!] Could not send the clock (chat times will be UTC): %v\n", err)
	}

	gamePath := findGameBinary(exeDir, cfg)
	if gamePath == "" {
		fmt.Fprintln(os.Stderr, "[!] Could not find the game.")
		fmt.Fprintln(os.Stderr, "    Choose your Dreadnought folder in the launcher window (Game folder, Change),")
		fmt.Fprintln(os.Stderr, "    put Dreadnought-Revival.exe in your Dreadnought install directory,")
		fmt.Fprintln(os.Stderr, "    or set 'game_path' in dn-launcher.json to the full path.")
		return nil, errors.New("could not find Dreadnought -- choose your game folder under Game folder")
	}
	fmt.Printf("[*] Game binary: %s\n", gamePath)

	// FirmamentHost defaults to GatewayIP so no hosts-file entry is needed.
	// The cert now includes IP:10.0.0.73 as a SAN so TLS verification passes.
	firmamentHost := cfg.FirmamentHost
	if firmamentHost == "" {
		firmamentHost = cfg.GatewayIP
	}

	// The launcher window's toggles (settings.json) add to dn-launcher.json.
	settings := loadSettings()
	// No start-URL argument. REMOVED 2026-09-28: the launcher passed
	// "?PlayerName=<account>" so FYMmogClient::Init (0x142A33C70) would read it
	// into the client's nickname. It did not work (battle-server joins still
	// had an empty "?Name=") and it broke new players: a positional argument is
	// also the engine's START URL, and a new player's game loaded the offline
	// tutorial map (S01E00_00_Tutorial_P?PlayerName=...) instead of the login
	// map /Game/Maps/Launch_P (DefaultEngine.ini GameDefaultMap) -- it never
	// contacted the server and looped in the tutorial (a tester's log,
	// 2026-09-28). Names now come from battle-server-mod via mmogbrain's
	// /battle/player.
	//
	// RE-ADDED 2026-09-28 as an EXPLICIT start URL: the login map itself, with
	// the option. The hangar's player card shows no name because the client's
	// nickname is empty: FYOnlineIdentityMmog::GetPlayerNickname (0x2AAB5F0)
	// returns the mmog client's +0x3540, and the only writer of that is
	// FYMmogClient::Init (0x2A33C70), which UYGameEngine::Browse (0x535A9F)
	// calls with the BROWSED URL's options -- "PlayerName". So the name has to
	// ride on the start URL. Naming the map (GameDefaultMap, Launch_P) means
	// the engine has no reason to fall back to another map, which is what the
	// bare "?PlayerName=" token did. NOT verified with a new account yet:
	// DN_NO_START_URL=1 turns it off if the tutorial loop comes back.
	var args []string
	if url := startURL(username); url != "" {
		args = append(args, url)
	}
	args = append(args,
		"-GatewayAddress="+cfg.GatewayIP,
		"-GatewayPort="+cfg.GatewayPort,
		"-YFirmamentAddress="+firmamentHost,
		"-YFirmamentPort="+cfg.FirmamentPort,
		"-noeac",
		// DreadGame/Config/DefaultEngine.ini sets NativePlatformService=Steam,
		// so the client still initializes the real Steam online subsystem for
		// achievements/presence alongside our Mmogbrain subsystem even though
		// matchmaking doesn't need it. Against a private server, real Steam
		// calls (e.g. GetAchievementAndUnlockTime) can't succeed and appear to
		// block on a long timeout rather than failing fast — -NoSteam skips
		// Steam subsystem init entirely, matching the flag the project's own
		// dedicated-server launch command already uses (see README.md).
		//
		// SUSPECTED COST, and the reason for allow_steam: the player's name and
		// avatar come from Steam, not from us. There is no name field anywhere
		// in the mmog protocol -- YA_PlayerGet has none, and
		// YA_GetPlayersInformation's is exactly
		// infos/DisplayInfo/UnlockedFleetType/Elite/Rank -- so nothing we send
		// can fill them. A client log from an operator with Steam running shows
		// the subsystem loading, receiving stats for the real persona, and then
		// shutting down:
		//
		//	STEAM: Loading Steam SDK 1.32
		//	STEAM: FOnlineAsyncEventSteamStatsReceived bWasSuccessful: 1
		//	       User: [DC-Lan Party] DARKACE
		//	STEAM: OnlineSubsystemSteam::Shutdown()
		//
		// If the UI reads the persona through that subsystem, this switch is why
		// the name and the avatar are blank. Unproven -- hence opt-in rather
		// than a default change, because the timeout above is a real cost.
		// Set allow_steam in dn-launcher.json, or DN_ALLOW_STEAM=1.
	)
	if cfg.AllowSteam {
		fmt.Println("[+] Steam: leaving the client's Steam subsystem enabled (allow_steam)")
	} else {
		fmt.Println("[+] Steam: passing -NoSteam. Set allow_steam in dn-launcher.json, " +
			"or DN_ALLOW_STEAM=1, to keep it enabled.")
		args = append(args, "-NoSteam")
	}

	if cfg.SkipOnboarding {
		// UYDreadnoughtLocalPlayer's constructor reads
		// m_noOnboarding = FParse::Param(FCommandLine::Get(), "noonboarding"),
		// and every onboarding gate — including the tutorial check that
		// UUI_LoginGateScreen::EnterGame runs before it will leave the loading
		// screen — short-circuits to "satisfied" when it is set.
		//
		// This is off by default on purpose: new players are supposed to go
		// through onboarding, exactly as they did on the live servers. The
		// server persists the client's own onboarding save blob (YA_SaveGame ->
		// the SGD field of YA_PlayerGet), so progress sticks across logins
		// without skipping anything. Use this only to get straight to the
		// hangar while debugging.
		args = append(args, "-noonboarding")
		fmt.Println("[*] Onboarding disabled (DN_SKIP_ONBOARDING / skip_onboarding) — the tutorial gate is bypassed.")
	}

	// The game's log, whole and in order (gameLogArgs in logupload_windows.go).
	args = append(args, gameLogArgs()...)

	if cfg.LogWindow || settings.LogWindow {
		// -LOG only opens the console window; the log FILE (Saved\Logs\
		// DreadGame.log) is written either way.
		args = append(args, "-LOG")
	}
	if cfg.VerboseLogging || settings.VerboseLog {
		// Bumps every UE4 log category (LogNet, LogHTTP, LogOnline,
		// LogYMmogbrain, LogWebServicesPlugin, etc.) to Verbose, and forces
		// the log file to flush after every line so nothing is lost if the
		// client crashes mid-session. Substantially larger log files —
		// intended for one-off debugging, not routine play.
		//
		// LogYComVOComponent is deliberately held down: raising it past Verbose
		// makes the client crash. UYComVOComponent::PlayVoiceLineInternal
		// (YComVOComponent.cpp:461) logs "%s with %s" from two UObject names,
		// and it does so before validating them -- by the time the tutorial's
		// intro movie ends one of those objects has already been destroyed, so
		// its NamePrivate slot holds a recycled heap pointer and FName::ToString
		// indexes the name pool with garbage.
		//
		// Confirmed from a full-memory dump of that crash: access violation at
		// FName::ToString, the "FName" read from object+0x18 was the pointer
		// 0x000001f898b6f300, and the category's live verbosity byte was 7.
		// The guard is `if (5 < verbosity)`, so anything up to Verbose (5) is
		// safe and VeryVerbose (6) or All (7) is not.
		args = append(args, `-LogCmds=global verbose, LogYComVOComponent log`, "-forcelogflush")
		fmt.Println("[*] Verbose logging enabled (DN_VERBOSE_LOG / verbose_logging) — expect much larger client log files.")
	}

	fmt.Printf("[*] Launching: %s\n", gamePath)
	fmt.Printf("    Args: %s\n", strings.Join(args, " "))
	fmt.Println()

	game, err := startGameProcess(gamePath, args, filepath.Dir(gamePath)) // Dreadnought install root
	if err != nil {
		return nil, fmt.Errorf("could not start the game: %w", err)
	}
	fmt.Printf("[+] Game launched (PID %d).\n", game.Pid)
	return game, nil
}

// signOutRequested reports whether the player asked to switch accounts, via
// --sign-out on the command line or DN_SIGN_OUT in the environment.
// flagRequested reports whether any of the given names was passed on the command
// line, in any of the spellings an operator might reasonably try.
func flagRequested(names ...string) bool {
	for _, arg := range os.Args[1:] {
		got := strings.ToLower(strings.TrimSpace(arg))
		got = strings.TrimLeft(got, "-/")
		for _, name := range names {
			if got == name {
				return true
			}
		}
	}
	return false
}

func signOutRequested() bool {
	if strings.TrimSpace(os.Getenv("DN_SIGN_OUT")) != "" {
		return true
	}
	for _, arg := range os.Args[1:] {
		switch strings.ToLower(strings.TrimSpace(arg)) {
		case "--sign-out", "-sign-out", "/signout", "--logout":
			return true
		}
	}
	return false
}

func fatalf(format string, args ...any) {
	if !consoleReady {
		// Built as a GUI program (-H windowsgui): with no console, a message
		// printed here would go nowhere.
		messageBox("Dreadnought launcher", strings.TrimSpace(strings.TrimPrefix(fmt.Sprintf(format, args...), "[!]")))
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	waitExit(1)
}

func waitExit(code int) {
	fmt.Println("\nPress Enter to exit (or wait 5 seconds)...")
	done := make(chan struct{})
	go func() {
		_, _ = fmt.Scanln()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	os.Exit(code)
}

// startURL is the game's start URL: the login map with the player's name, or
// "" when there is no usable name or DN_NO_START_URL=1. Only characters that
// cannot break a URL option are kept.
func startURL(username string) string {
	if os.Getenv("DN_NO_START_URL") == "1" {
		return ""
	}
	var b strings.Builder
	for _, r := range username {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '.' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "/Game/Maps/Launch_P?PlayerName=" + b.String()
}
