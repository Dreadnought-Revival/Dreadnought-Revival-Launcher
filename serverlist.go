package main

// The server list: serverlist.yaml in this repository, which the launcher
// downloads from GitHub at every start, so a server can be listed, moved or
// given a new certificate without a new launcher. The download goes over
// GitHub's ordinary, fully verified HTTPS, which is what lets the list carry
// each server's CA: a listed server's certificate is trusted because the list
// says so, not because the server offered it (servers_windows.go).
//
// Order: the download, else the last good download (cached under
// LOCALAPPDATA), else the copy built into this exe. A list that does not parse
// never replaces a good one.

import (
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// serverListURL is where the list is downloaded from. Override at build time
// (-X main.serverListURL=...), or per install with dn-launcher.json
// "server_list_url" ("none" turns the download off).
var serverListURL = "https://raw.githubusercontent.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/main/serverlist.yaml"

//go:embed serverlist.yaml
var builtinServerList []byte

// serverListTimeout bounds the download at start; the window opens after it.
const serverListTimeout = 6 * time.Second

// serverListMaxBytes is far above any real list (one entry with its CA is ~2 KB).
const serverListMaxBytes = 1 << 20

type listedServer struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	Description   string `yaml:"description"`
	Host          string `yaml:"host"`
	WebPort       int    `yaml:"web_port"`
	GatewayPort   int    `yaml:"gateway_port"`
	FirmamentPort int    `yaml:"firmament_port"`
	CACert        string `yaml:"ca_cert"`

	// caDER is CACert parsed (nil without one).
	caDER []byte
}

type serverListDoc struct {
	Servers []listedServer `yaml:"servers"`
}

// serverList is the list in use and where it came from ("online", "cached",
// "built-in"), for the window and the console.
type serverList struct {
	Servers []listedServer
	Source  string
}

var listIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// parseServerList reads a list. Entries that are not usable are left out with
// a reason each, so one bad entry cannot hide the others; a document that is
// not a list at all is an error.
func parseServerList(data []byte) ([]listedServer, []string, error) {
	var doc serverListDoc
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, nil, fmt.Errorf("serverlist.yaml: %w", err)
	}
	var out []listedServer
	var skipped []string
	seen := map[string]bool{}
	for i, s := range doc.Servers {
		s.ID = strings.TrimSpace(s.ID)
		s.Name = strings.TrimSpace(s.Name)
		s.Description = strings.TrimSpace(s.Description)
		s.Host = strings.ToLower(strings.TrimSpace(s.Host))
		why := ""
		switch {
		case !listIDPattern.MatchString(s.ID):
			why = fmt.Sprintf("id %q is not lowercase letters, digits and dashes", s.ID)
		case seen[s.ID]:
			why = "id " + s.ID + " is listed twice"
		case s.Host == "" || strings.ContainsAny(s.Host, " /\\?#@[]:"):
			why = fmt.Sprintf("host %q is not a DNS name or IPv4 address", s.Host)
		case !portOK(s.WebPort) || !portOK(s.GatewayPort) || !portOK(s.FirmamentPort):
			why = "a port is outside 1-65535"
		}
		if why == "" && strings.TrimSpace(s.CACert) != "" {
			der, err := parseListCA(s.CACert)
			if err != nil {
				why = "ca_cert: " + err.Error()
			}
			s.caDER = der
		}
		if why != "" {
			skipped = append(skipped, fmt.Sprintf("entry %d: %s", i+1, why))
			continue
		}
		if s.Name == "" {
			s.Name = s.Host
		}
		if len([]rune(s.Name)) > 40 {
			s.Name = string([]rune(s.Name)[:40])
		}
		seen[s.ID] = true
		out = append(out, s)
	}
	if len(doc.Servers) > 0 && len(out) == 0 {
		return nil, skipped, errors.New("serverlist.yaml: no usable entry")
	}
	return out, skipped, nil
}

// portOK: 0 means "the default".
func portOK(p int) bool { return p >= 0 && p <= 65535 }

// parseListCA takes a PEM certificate and insists on a CA.
func parseListCA(text string) ([]byte, error) {
	block, _ := pem.Decode([]byte(strings.TrimSpace(text)))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, err
	}
	if !cert.IsCA {
		return nil, errors.New("not a CA certificate")
	}
	return block.Bytes, nil
}

// portString is a listed port for Config: "" for the default.
func portString(p int) string {
	if p == 0 {
		return ""
	}
	return strconv.Itoa(p)
}

func serverListCachePath() string {
	return filepath.Join(os.Getenv("LOCALAPPDATA"), "DreadnoughtPrivateServer", "serverlist.yaml")
}

// loadServerList is the list for this run (see the top of the file).
func loadServerList(url string) serverList {
	if url = strings.TrimSpace(url); url == "" {
		url = serverListURL
	}
	if url != "none" {
		data, err := downloadServerList(url)
		if err == nil {
			servers, skipped, perr := parseServerList(data)
			reportSkipped("downloaded", skipped)
			if perr == nil {
				if werr := writeServerListCache(data); werr != nil {
					fmt.Printf("[!] Could not keep the server list (%v)\n", werr)
				}
				fmt.Printf("[*] Server list: %d server(s) from %s\n", len(servers), url)
				return serverList{Servers: servers, Source: "online"}
			}
			err = perr
		}
		fmt.Printf("[!] Server list not downloaded (%v); using the last one\n", err)
	}
	//nolint:gosec // the launcher's own cache file under LOCALAPPDATA.
	if data, err := os.ReadFile(serverListCachePath()); err == nil {
		servers, skipped, perr := parseServerList(data)
		reportSkipped("cached", skipped)
		if perr == nil {
			fmt.Printf("[*] Server list: %d server(s), last download\n", len(servers))
			return serverList{Servers: servers, Source: "cached"}
		}
	}
	servers, skipped, err := parseServerList(builtinServerList)
	reportSkipped("built-in", skipped)
	if err != nil {
		fmt.Printf("[!] Built-in server list: %v\n", err)
	}
	fmt.Printf("[*] Server list: %d server(s), built in\n", len(servers))
	return serverList{Servers: servers, Source: "built-in"}
}

func reportSkipped(which string, skipped []string) {
	for _, s := range skipped {
		fmt.Printf("[!] %s server list, %s -- left out\n", which, s)
	}
}

// downloadServerList fetches the list with the system's own certificate
// checks -- NOT launcherHTTPClient, which sends every connection to the game
// server's IP.
func downloadServerList(url string) ([]byte, error) {
	client := &http.Client{Timeout: serverListTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, serverListMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > serverListMaxBytes {
		return nil, errors.New("larger than 1 MB")
	}
	return data, nil
}

func writeServerListCache(data []byte) error {
	path := serverListCachePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
