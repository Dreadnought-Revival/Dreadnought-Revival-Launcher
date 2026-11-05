//go:build windows

package main

// Server certificates the launcher put in (or found in) the current user's
// Trusted Root store, and when each was last used. A server's CA can vouch
// for any web site, so one the player no longer plays on should not stay
// trusted forever: a certificate not used for certUnusedAfter is removed the
// next time the launcher starts (CleanCerts), and Settings lists them all
// with a Remove button. "Used" means the launcher reached that server with it
// (news loaded, or the game started).
//
// Only certificates in this record are ever removed -- never one the launcher
// does not know about. A certificate that was already trusted when the
// launcher first needed it (installed by an older launcher, or by hand) is
// recorded too, since it is a game server's CA all the same.
//
// The limit is that this runs only when the launcher does: a player who stops
// playing and never opens it again keeps the certificate until they remove it
// (certmgr.msc) or run "Dreadnought-Revival.exe --remove-certs" (removes every one in
// the record; meant for an uninstaller).

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// certUnusedAfter is how long a server certificate may go unused.
const certUnusedAfter = 30 * 24 * time.Hour

// certAskAgainAfter: a removal the player declined is not asked again for
// this long.
const certAskAgainAfter = 7 * 24 * time.Hour

type certRecord struct {
	SHA256   string    `json:"sha256"` // hex of the DER
	Name     string    `json:"name"`   // the certificate's subject CN
	Server   string    `json:"server"` // picker name of the server it was used for
	LastUsed time.Time `json:"last_used"`
	// Declined is when the player said No to Windows' removal question.
	Declined time.Time `json:"declined,omitempty"`
}

func certHash(der []byte) string {
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// noteCertUsed records that the launcher reached serverName with der, if der
// is in the store.
func noteCertUsed(der []byte, serverName string) {
	if len(der) == 0 || !caTrusted(der) {
		return
	}
	name, _ := caSummary(der)
	h := certHash(der)
	settings := loadSettings()
	found := false
	for i := range settings.Certs {
		if settings.Certs[i].SHA256 == h {
			settings.Certs[i].LastUsed = time.Now().UTC()
			settings.Certs[i].Server = serverName
			found = true
		}
	}
	if !found {
		settings.Certs = append(settings.Certs, certRecord{SHA256: h, Name: name, Server: serverName,
			LastUsed: time.Now().UTC()})
	}
	if err := saveSettings(settings); err != nil {
		fmt.Printf("[!] Could not record the certificate's use (%v)\n", err)
	}
}

// removeCertFromStore deletes the certificate with this hash from the current
// user's Trusted Root store. Windows asks the player to confirm; declining
// comes back as an error. Not in the store is not an error.
//
// Through the store programs see (openUserRootStore) first: on Windows that
// is the path with the confirmation. Wine reports that delete as done and
// keeps the certificate (VERIFIED under Wine 2026-11-05: eight "successful"
// deletes in a row, still trusted): its Root view is a copy, and what
// installCA added lives in the user's registry store. So when the
// certificate is still there, it is deleted from that registry store
// directly. NOT verified on Windows, where the first path is expected to do
// it (it is the call certutil -user -delstore makes).
func removeCertFromStore(hash string) (removed bool, err error) {
	found, err := deleteFromStore(openUserRootStore, hash)
	if err != nil || !found {
		return false, err
	}
	if !trustedHash(hash) {
		return true, nil
	}
	if _, err := deleteFromStore(openUserRootRegistryStore, hash); err != nil {
		return false, err
	}
	if trustedHash(hash) {
		return false, errors.New("Windows kept the certificate")
	}
	return true, nil
}

// openUserRootRegistryStore is the current user's Root store as it is kept
// in the registry, without the system-store layers on top.
func openUserRootRegistryStore() (windows.Handle, error) {
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_REGISTRY, 0, 0,
		windows.CERT_SYSTEM_STORE_CURRENT_USER, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return 0, fmt.Errorf("open the Trusted Root registry store: %w", err)
	}
	return store, nil
}

// deleteFromStore deletes the certificate with this hash from the store open
// returns; found is false when it is not there.
func deleteFromStore(open func() (windows.Handle, error), hash string) (found bool, err error) {
	store, err := open()
	if err != nil {
		return false, err
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	var ctx *windows.CertContext
	for {
		ctx, err = windows.CertEnumCertificatesInStore(store, ctx)
		if err != nil || ctx == nil {
			return false, nil
		}
		if certHash(unsafe.Slice(ctx.EncodedCert, ctx.Length)) == hash {
			break
		}
	}
	// CertDeleteCertificateFromStore frees ctx, whatever it returns.
	if err := windows.CertDeleteCertificateFromStore(ctx); err != nil {
		return true, fmt.Errorf("remove the certificate (declined?): %w", err)
	}
	return true, nil
}

// trustedHash reports whether a certificate with this hash is in the
// current user's Trusted Root store.
func trustedHash(hash string) bool {
	store, err := openUserRootStore()
	if err != nil {
		return false
	}
	defer func() { _ = windows.CertCloseStore(store, 0) }()
	var ctx *windows.CertContext
	for {
		ctx, err = windows.CertEnumCertificatesInStore(store, ctx)
		if err != nil || ctx == nil {
			return false
		}
		if certHash(unsafe.Slice(ctx.EncodedCert, ctx.Length)) == hash {
			_ = windows.CertFreeCertificateContext(ctx)
			return true
		}
	}
}

// staleCerts are the recorded certificates due for removal at now: unused for
// certUnusedAfter, not declined recently, and not keep (the CA of the server
// in use -- removing it would only ask the player to install it again).
func staleCerts(records []certRecord, keep string, now time.Time) []certRecord {
	var out []certRecord
	for _, r := range records {
		if r.SHA256 == keep || now.Sub(r.LastUsed) < certUnusedAfter {
			continue
		}
		if !r.Declined.IsZero() && now.Sub(r.Declined) < certAskAgainAfter {
			continue
		}
		out = append(out, r)
	}
	return out
}

// removeCert removes one recorded certificate and its record. declined keeps
// the record and notes the refusal.
func removeCert(hash string) (removed bool, err error) {
	removed, err = removeCertFromStore(hash)
	settings := loadSettings()
	kept := settings.Certs[:0]
	for _, r := range settings.Certs {
		if r.SHA256 == hash {
			if err != nil {
				r.Declined = time.Now().UTC()
				kept = append(kept, r)
			}
			continue
		}
		kept = append(kept, r)
	}
	settings.Certs = kept
	if serr := saveSettings(settings); serr != nil && err == nil {
		err = serr
	}
	return removed, err
}

// CleanCerts removes the stale certificates (staleCerts). Called by the page
// once it is shown, so Windows' confirmation belongs to the window.
func (a *launcherAPI) CleanCerts() map[string]any {
	keep := ""
	machineMu.RLock()
	if pendingCA != nil {
		keep = certHash(pendingCA)
	}
	machineMu.RUnlock()
	var removed, declined []string
	for _, r := range staleCerts(loadSettings().Certs, keep, time.Now().UTC()) {
		label := certLabel(r)
		ok, err := removeCert(r.SHA256)
		switch {
		case err != nil:
			fmt.Printf("[!] %s, unused since %s: %v\n", label, r.LastUsed.Format("2006-01-02"), err)
			declined = append(declined, label)
		case ok:
			fmt.Printf("[*] Removed %s: unused since %s\n", label, r.LastUsed.Format("2006-01-02"))
			removed = append(removed, label)
		}
	}
	return map[string]any{"removed": removed, "declined": declined}
}

func certLabel(r certRecord) string {
	if r.Server != "" && r.Server != r.Name {
		return r.Name + " (" + r.Server + ")"
	}
	return r.Name
}

// Certs lists the recorded certificates for Settings.
func (a *launcherAPI) Certs() []map[string]any {
	machineMu.RLock()
	current := ""
	if pendingCA != nil {
		current = certHash(pendingCA)
	}
	machineMu.RUnlock()
	out := []map[string]any{}
	for _, r := range loadSettings().Certs {
		out = append(out, map[string]any{"sha256": r.SHA256, "name": r.Name, "server": r.Server,
			"lastUsed": r.LastUsed.Local().Format("2 Jan 2006"), "current": r.SHA256 == current})
	}
	return out
}

// RemoveCert removes one certificate from Settings.
func (a *launcherAPI) RemoveCert(hash string) map[string]any {
	hash = strings.ToLower(strings.TrimSpace(hash))
	known := false
	for _, r := range loadSettings().Certs {
		known = known || r.SHA256 == hash
	}
	if !known {
		return map[string]any{"ok": false, "error": "That certificate is not one the launcher installed."}
	}
	if _, err := removeCert(hash); err != nil {
		return map[string]any{"ok": false, "error": "The certificate was not removed. Windows asks you to confirm -- choose Yes."}
	}
	return map[string]any{"ok": true, "certs": a.Certs()}
}

// removeAllCerts is --remove-certs: every recorded certificate, for an
// uninstaller. Returns how many could not be removed.
func removeAllCerts() int {
	failed := 0
	for _, r := range loadSettings().Certs {
		if ok, err := removeCert(r.SHA256); err != nil {
			fmt.Printf("[!] %s: %v\n", certLabel(r), err)
			failed++
		} else if ok {
			fmt.Printf("[*] Removed %s\n", certLabel(r))
		}
	}
	return failed
}
