package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func testCAPEM(t *testing.T, isCA bool) string {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "List CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func indent(s string) string {
	return "      " + strings.ReplaceAll(strings.TrimSpace(s), "\n", "\n      ")
}

// Good entries survive bad ones; each bad one is reported.
func TestParseServerList(t *testing.T) {
	doc := `servers:
  - id: eu
    name: Europe
    host: EU.Example.org
    web_port: 8443
    gateway_port: 65443
    firmament_port: 48843
    ca_cert: |
` + indent(testCAPEM(t, true)) + `
  - id: plain
    host: 203.0.113.9
  - id: Bad_ID
    host: x.example
  - id: eu
    host: twice.example
  - id: noport
    host: y.example
    web_port: 70000
  - id: path
    host: z.example/x
  - id: leaf
    host: leaf.example
    ca_cert: |
` + indent(testCAPEM(t, false)) + `
`
	got, skipped, err := parseServerList([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "eu" || got[0].Host != "eu.example.org" || got[0].caDER == nil ||
		got[1].ID != "plain" || got[1].Name != "203.0.113.9" || got[1].caDER != nil {
		t.Fatalf("servers = %+v", got)
	}
	if len(skipped) != 5 {
		t.Fatalf("skipped = %q", skipped)
	}
	c := listedChoice(got[0])
	if c.id() != "list:eu" || c.WebPort != "8443" || c.GatewayPort != "65443" {
		t.Fatalf("choice = %+v", c)
	}
	if _, _, err := parseServerList([]byte("servers: [{id: Bad}]")); err == nil {
		t.Fatal("a list with no usable entry was accepted")
	}
	if _, _, err := parseServerList([]byte("servers: {")); err == nil {
		t.Fatal("broken YAML was accepted")
	}
}

// The copy built into the launcher must itself be a usable list.
func TestBuiltinServerList(t *testing.T) {
	got, skipped, err := parseServerList(builtinServerList)
	if err != nil || len(skipped) != 0 || len(got) == 0 {
		t.Fatalf("built-in list: %d servers, skipped %q, %v", len(got), skipped, err)
	}
	for _, s := range got {
		if s.caDER == nil {
			t.Errorf("%s: no ca_cert", s.ID)
		}
	}
}
