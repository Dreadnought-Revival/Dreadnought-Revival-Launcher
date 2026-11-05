//go:build windows

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// Default first, then the listed servers.
func TestServerChoices(t *testing.T) {
	defer func(c Config, l serverList) { baseConfig, servers = c, l }(baseConfig, servers)
	baseConfig = defaultConfig()
	baseConfig.Server = "play.example.org"
	servers = serverList{Servers: []listedServer{
		{ID: "eu", Name: "EU", Host: "eu.example", WebPort: 8443, GatewayPort: 1234, FirmamentPort: 5678, caDER: []byte{1}},
		{ID: "us", Name: "US", Host: "us.example"},
	}}
	got := serverChoices()
	if len(got) != 3 || got[0].id() != "" || got[1].id() != "list:eu" || got[2].id() != "list:us" {
		t.Fatalf("choices = %+v", got)
	}
	cfg := configFor(got[1])
	if cfg.Server != "eu.example" || cfg.WebPort != "8443" || cfg.GatewayPort != "1234" || cfg.FirmamentPort != "5678" ||
		cfg.caFile != "" || cfg.caDER == nil || cfg.caSource != caSourceList || cfg.FirmamentHost != "" {
		t.Fatalf("configFor(listed with CA) = %+v", cfg)
	}
	// Listed without ca_cert: the CA is asked from the server and kept.
	cfg = configFor(got[2])
	if cfg.caFile == "" || cfg.caDER != nil || cfg.caSource != caSourceServer || cfg.GatewayPort != "65443" {
		t.Fatalf("configFor(listed without CA) = %+v", cfg)
	}
	if k := got[2].key(); k != "list_us" {
		t.Errorf("key = %q", k)
	}
	// No server built in: no default entry, the list leads.
	baseConfig.Server = ""
	if got := serverChoices(); got[0].id() != "list:eu" {
		t.Fatalf("without a default, first = %+v", got[0])
	}
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(der)
	return ca, key, der
}

// serveCA starts a TLS server whose certificate is issued by signer and which
// offers offered at /dn/ca.crt, like gateway/main.go.
func serveCA(t *testing.T, signer *x509.Certificate, signerKey *ecdsa.PrivateKey, offered []byte) (ip, port string) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dn/ca.crt" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: offered}))
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return u.Hostname(), u.Port()
}

func TestFetchServerCA(t *testing.T) {
	ca, caKey, caDER := testCA(t)
	ip, port := serveCA(t, ca, caKey, caDER)
	got, err := fetchServerCA(ip, port)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(caDER) {
		t.Fatal("fetched CA differs from the served one")
	}

	// A CA that did not issue the server's certificate is refused.
	_, _, otherDER := testCA(t)
	ip, port = serveCA(t, ca, caKey, otherDER)
	if _, err := fetchServerCA(ip, port); err == nil {
		t.Fatal("accepted a CA that did not sign the connection")
	}
}

// Each server remembers its own sign-in; the default keeps account.json.
func TestCredentialPathPerServer(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	defer func() { credentialServer = "" }()
	def, _ := credentialPath()
	credentialServer = "friend.example"
	other, _ := credentialPath()
	if def == other || !(len(def) > 0 && def[len(def)-len("account.json"):] == "account.json") {
		t.Fatalf("default %q, server %q", def, other)
	}
}
