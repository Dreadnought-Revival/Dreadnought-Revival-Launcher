//go:build windows

package main

// Server picker: the launcher window's Server / Change... (originally
// PR #81 by starveil-bunny, darkace1998/Dreadnought-Revival-project). The
// picker offers, in this order:
//
//   - the server this launcher was built or configured for (Config.Server,
//     "default"), when there is one;
//   - the servers of the server list (serverlist.go, "listed");
//   - servers the player added by address ("custom"; "localhost" for one on
//     the same computer).
//
// The choice is kept per Windows user in settings.json. With no choice made
// and nothing built in, the first listed server is used, so a launcher
// downloaded from the releases page needs no setup at all.
//
// Certificates. The default server's CA is ca.crt beside the launcher or the
// built-in one, as before. A listed server's CA comes from the list (GitHub's
// verified HTTPS is what vouches for it). Any other server -- added by hand,
// or listed without ca_cert -- is asked for its CA (GET /dn/ca.crt on its
// gateway, see the server's gateway/main.go); the launcher checks that the CA
// really issued the certificate the gateway presented and keeps it as
// servers\<key>.crt. In every case the certificate screen shows the
// fingerprint before anything is installed.
//
// Accounts live on a server, so each server keeps its own remembered sign-in
// (credentialServer in account.go).

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// savedServer is a server the player added by address (settings.json).
type savedServer struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	WebPort string `json:"web_port,omitempty"`
}

// Kinds of picker entry.
const (
	serverKindDefault = "default"
	serverKindListed  = "listed"
	serverKindCustom  = "custom"
)

// serverChoice is one entry of the picker.
type serverChoice struct {
	Kind        string
	ListID      string // listed: the list's id
	Name        string
	Description string
	Address     string // "" for the default
	WebPort     string // "" = 443
	// GatewayPort / FirmamentPort are a listed server's game ports ("" =
	// the configured ones, 65443 / 48843 by default).
	GatewayPort   string
	FirmamentPort string
	// CA is a listed server's CA from the list; nil = ask the server.
	CA []byte
}

// baseConfig is the configuration before any choice made in the window
// (dn-launcher.json + build-time defaults): the default entry.
var baseConfig Config

// servers is the server list of this run (applySavedServer).
var servers serverList

// id is how the page and settings.json name a server: "" for the default,
// "list:<id>" for a listed one, else address[:web port].
func (s serverChoice) id() string {
	switch s.Kind {
	case serverKindDefault:
		return ""
	case serverKindListed:
		return "list:" + s.ListID
	}
	if s.WebPort != "" && s.WebPort != "443" {
		return s.Address + ":" + s.WebPort
	}
	return s.Address
}

// key is id() made safe for a file name ("" for the default, so its sign-in
// stays account.json).
func (s serverChoice) key() string {
	return fileKey(s.id())
}

func fileKey(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' {
			return r
		}
		return '_'
	}, id)
}

// caFile is where a CA fetched from the server is kept.
func (s serverChoice) caFile() string {
	return filepath.Join(filepath.Dir(settingsPath()), "servers", s.key()+".crt")
}

// displayAddress is the address the picker shows.
func (s serverChoice) displayAddress() string {
	if s.WebPort != "" && s.WebPort != "443" {
		return s.Address + ":" + s.WebPort
	}
	return s.Address
}

func (s savedServer) choice() serverChoice {
	return serverChoice{Kind: serverKindCustom, Name: s.Name, Address: s.Address, WebPort: s.WebPort}
}

func listedChoice(l listedServer) serverChoice {
	return serverChoice{Kind: serverKindListed, ListID: l.ID, Name: l.Name, Description: l.Description,
		Address: l.Host, WebPort: portString(l.WebPort), GatewayPort: portString(l.GatewayPort),
		FirmamentPort: portString(l.FirmamentPort), CA: l.caDER}
}

// parseServerAddress takes what a player types: "host", "host:port", with or
// without https:// and a trailing slash. The port is the web (sign-in) port.
func parseServerAddress(text string) (savedServer, error) {
	t := strings.TrimSpace(text)
	t = strings.TrimPrefix(strings.TrimPrefix(t, "https://"), "http://")
	t = strings.TrimSuffix(t, "/")
	if t == "" {
		return savedServer{}, errors.New("enter the server's address")
	}
	host, port := t, ""
	if h, p, err := net.SplitHostPort(t); err == nil {
		host, port = h, p
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return savedServer{}, fmt.Errorf("%q is not a port number", p)
		}
	}
	if host == "" || strings.ContainsAny(host, " /\\?#@[]:") {
		return savedServer{}, fmt.Errorf("%q is not a server address", host)
	}
	if port == "443" {
		port = ""
	}
	return savedServer{Address: strings.ToLower(host), WebPort: port}, nil
}

// serverChoices is every entry the picker shows: the default (when this
// launcher has one), the listed servers, then the added ones. An added server
// that is also listed shows once, as the listed one.
func serverChoices(settings launcherSettings) []serverChoice {
	var out []serverChoice
	if !defaultUnset() {
		out = append(out, serverChoice{Kind: serverKindDefault, Name: "Default server"})
	}
	listed := map[string]bool{}
	for _, l := range servers.Servers {
		c := listedChoice(l)
		listed[c.displayAddress()] = true
		out = append(out, c)
	}
	for _, s := range settings.Servers {
		if c := s.choice(); s.Address != "" && !listed[c.displayAddress()] {
			out = append(out, c)
		}
	}
	return out
}

func findServer(settings launcherSettings, id string) (serverChoice, bool) {
	for _, s := range serverChoices(settings) {
		if s.id() == id {
			return s, true
		}
	}
	return serverChoice{}, false
}

// configFor is baseConfig pointed at choice.
func configFor(choice serverChoice) Config {
	cfg := baseConfig
	if choice.Kind == serverKindDefault {
		return cfg
	}
	cfg.Server, cfg.WebPort = choice.Address, choice.WebPort
	if choice.GatewayPort != "" {
		cfg.GatewayPort = choice.GatewayPort
	}
	if choice.FirmamentPort != "" {
		cfg.FirmamentPort = choice.FirmamentPort
	}
	// runMachineSetup fills both from the resolved address; a firmament_host
	// in dn-launcher.json belongs to the default server.
	cfg.GatewayIP, cfg.FirmamentHost = "", ""
	cfg.caDER, cfg.caFile, cfg.caSource = nil, "", caSourceServer
	if choice.CA != nil {
		cfg.caDER, cfg.caSource = choice.CA, caSourceList
	} else {
		cfg.caFile = choice.caFile()
	}
	return cfg
}

// selectServer points the launcher at choice: fetches the server's CA when
// one is needed and not kept yet, then reruns runMachineSetup. On failure the
// previous setup is put back by the caller.
func selectServer(exeDir string, choice serverChoice) (Config, error) {
	cfg := configFor(choice)
	if cfg.caFile != "" {
		if _, err := os.Stat(cfg.caFile); errors.Is(err, os.ErrNotExist) {
			ip, err := resolveServerIP(choice.Address)
			if err != nil {
				return cfg, err
			}
			der, err := fetchServerCA(ip, choice.WebPort)
			if err != nil {
				return cfg, err
			}
			if err := os.MkdirAll(filepath.Dir(cfg.caFile), 0o700); err != nil {
				return cfg, err
			}
			if err := os.WriteFile(cfg.caFile, der, 0o600); err != nil {
				return cfg, err
			}
		}
	}
	if err := runMachineSetup(exeDir, &cfg); err != nil {
		return cfg, err
	}
	credentialServer = choice.key()
	currentServerName = choice.Name
	return cfg, nil
}

// fetchServerCA asks a server's gateway for its CA. The connection cannot be
// verified yet -- that CA is what would verify it -- so the check is that the
// CA offered really issued the certificate presented; the player then
// compares the fingerprint on the certificate screen before installing.
func fetchServerCA(ip, webPort string) ([]byte, error) {
	port := webPort
	if port == "" {
		port = "443"
	}
	var leaf *x509.Certificate
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		//nolint:gosec // see above: checked against the CA below, fingerprint confirmed by the player.
		InsecureSkipVerify: true,
		MinVersion:         tls.VersionTLS12,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) > 0 {
				leaf = cs.PeerCertificates[0]
			}
			return nil
		},
	}}
	client := &http.Client{Transport: transport, Timeout: 15 * time.Second}
	resp, err := client.Get("https://" + net.JoinHostPort(ip, port) + "/dn/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("cannot reach the server at %s: %w", net.JoinHostPort(ip, port), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("the server at %s does not offer its certificate (HTTP %d) -- is it a Dreadnought private server, and up to date?",
			net.JoinHostPort(ip, port), resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return nil, err
	}
	der, err := parseCACert(raw)
	if err != nil {
		return nil, fmt.Errorf("the server's certificate: %w", err)
	}
	ca, _ := x509.ParseCertificate(der)
	if leaf == nil || leaf.CheckSignatureFrom(ca) != nil {
		return nil, errors.New("the server's connection is not signed by the certificate it offers; not trusting it")
	}
	return der, nil
}

// applySavedServer runs at start-up, after loadConfig: loads the server list,
// then points the launcher at the window's choice -- or, without one, the
// built-in server, else the first listed one. A choice that cannot be set up
// right now falls back the same way.
func applySavedServer(exeDir string, cfg Config) (Config, error) {
	baseConfig = cfg
	servers = loadServerList(cfg.ServerListURL)
	settings := loadSettings()
	if settings.Server != "" {
		if choice, ok := findServer(settings, settings.Server); ok {
			picked, err := selectServer(exeDir, choice)
			if err == nil {
				return picked, nil
			}
			fmt.Printf("[!] %s: %v -- using the default server for now.\n", choice.Name, err)
		} else {
			fmt.Printf("[!] The chosen server %q is no longer offered -- using the default server.\n", settings.Server)
		}
	}
	return applyFallbackServer(exeDir, cfg)
}

// applyFallbackServer is the server used without a (working) choice.
func applyFallbackServer(exeDir string, cfg Config) (Config, error) {
	if defaultUnset() && len(servers.Servers) > 0 {
		picked, err := selectServer(exeDir, listedChoice(servers.Servers[0]))
		if err == nil {
			return picked, nil
		}
		fmt.Printf("[!] %s: %v\n", servers.Servers[0].Name, err)
	}
	credentialServer, currentServerName = "", "Default server"
	return cfg, runMachineSetup(exeDir, &cfg)
}

// currentServerName is the picker name of the server in use.
var currentServerName = "Default server"

// ---- launcher window actions ---------------------------------------------------

func (a *launcherAPI) serverInfo() map[string]any {
	settings := loadSettings()
	current := credentialServer // key of the server in use; "" = default
	list := []map[string]any{}
	for _, s := range serverChoices(settings) {
		addr := s.displayAddress()
		if s.Kind == serverKindDefault {
			addr = baseConfig.Server
			if addr == "" {
				addr = baseConfig.GatewayIP
			}
		}
		list = append(list, map[string]any{"id": s.id(), "name": s.Name, "description": s.Description,
			"address": addr, "kind": s.Kind, "selected": s.key() == current})
	}
	return map[string]any{"list": list, "current": currentServerName,
		"unset": current == "" && defaultUnset(), "source": servers.Source}
}

// defaultUnset: no server built in or in dn-launcher.json, only the old
// built-in gateway address.
func defaultUnset() bool {
	return baseConfig.Server == "" && baseConfig.GatewayIP == defaultConfig().GatewayIP
}

// UseServer switches to a listed server (id "" = the default).
func (a *launcherAPI) UseServer(id string) map[string]any {
	a.netMu.Lock()
	defer a.netMu.Unlock()
	settings := loadSettings()
	choice, ok := findServer(settings, id)
	if !ok {
		return map[string]any{"ok": false, "error": "That server is not in the list."}
	}
	prevCfg, prevKey, prevName := a.config(), credentialServer, currentServerName
	cfg, err := selectServer(a.exeDir, choice)
	if err != nil {
		restore := prevCfg
		credentialServer, currentServerName = prevKey, prevName
		_ = runMachineSetup(a.exeDir, &restore)
		return map[string]any{"ok": false, "error": capitalise(err.Error())}
	}
	settings.Server = choice.id()
	if err := saveSettings(settings); err != nil {
		return map[string]any{"ok": false, "error": "Could not save the setting: " + err.Error()}
	}
	a.mu.Lock()
	a.cfg = cfg
	a.token, a.username = "", ""
	if creds, ok := loadCredentials(); ok && !launcherTokenExpired(creds.Token) {
		a.token, a.username = creds.Token, creds.Username
	}
	a.mu.Unlock()
	out := a.Init()
	out["ok"] = true
	return out
}

// AddServer saves a server typed by the player and switches to it. A server
// that cannot be reached is not kept.
func (a *launcherAPI) AddServer(name, address string) map[string]any {
	s, err := parseServerAddress(address)
	if err != nil {
		return map[string]any{"ok": false, "error": capitalise(err.Error())}
	}
	s.Name = strings.TrimSpace(name)
	if len([]rune(s.Name)) > 40 {
		s.Name = string([]rune(s.Name)[:40])
	}
	if s.Name == "" {
		s.Name = s.Address
	}
	settings := loadSettings()
	id := s.choice().id()
	existing, exists := findServer(settings, id)
	if !exists {
		// Typed the address of a listed server: that is the one meant.
		for _, c := range serverChoices(settings) {
			if c.Kind == serverKindListed && c.displayAddress() == s.choice().displayAddress() {
				existing, exists = c, true
			}
		}
	}
	if exists {
		return a.UseServer(existing.id())
	}
	settings.Servers = append(settings.Servers, s)
	if err := saveSettings(settings); err != nil {
		return map[string]any{"ok": false, "error": "Could not save the server: " + err.Error()}
	}
	r := a.UseServer(id)
	if r["ok"] != true {
		forgetServer(id)
	}
	return r
}

// RemoveServer drops a server the player added and its CA file; removing the
// one in use switches back to the fallback server.
func (a *launcherAPI) RemoveServer(id string) map[string]any {
	settings := loadSettings()
	s, ok := findServer(settings, id)
	if !ok || s.Kind != serverKindCustom {
		return map[string]any{"ok": false, "error": "That server cannot be removed."}
	}
	inUse := s.key() == credentialServer
	forgetServer(id)
	_ = os.Remove(s.caFile())
	if inUse {
		settings = loadSettings()
		settings.Server = ""
		_ = saveSettings(settings)
		a.netMu.Lock()
		cfg, err := applyFallbackServer(a.exeDir, baseConfig)
		a.netMu.Unlock()
		if err != nil {
			return map[string]any{"ok": false, "error": capitalise(err.Error())}
		}
		a.mu.Lock()
		a.cfg = cfg
		a.token, a.username = "", ""
		if creds, ok := loadCredentials(); ok && !launcherTokenExpired(creds.Token) {
			a.token, a.username = creds.Token, creds.Username
		}
		a.mu.Unlock()
	}
	out := a.Init()
	out["ok"] = true
	return out
}

func forgetServer(id string) {
	settings := loadSettings()
	kept := settings.Servers[:0]
	for _, s := range settings.Servers {
		if s.choice().id() != id {
			kept = append(kept, s)
		}
	}
	settings.Servers = kept
	_ = saveSettings(settings)
}
