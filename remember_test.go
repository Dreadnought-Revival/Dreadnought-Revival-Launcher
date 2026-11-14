//go:build windows

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// authStub answers the password grant: a token for the right password, 401
// otherwise.
func authStub(t *testing.T, password string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("password") != password {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"Invalid username or password"}`))
			return
		}
		_, _ = w.Write([]byte(`{"jwt":"new-token","username":"Captain","user_id":"u1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// "Remember me": an expired token is renewed with the stored password, and
// the password stays stored for the next time.
func TestRenewCredentialsKeepsThePassword(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := authStub(t, "secret")
	creds, err := renewCredentials(srv.URL+"/auth/", storedCredentials{Identifier: "cap", Password: "secret", Token: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if creds.Token != "new-token" || creds.Password != "secret" || creds.Username != "Captain" {
		t.Errorf("renewed = %+v", creds)
	}
	saved, ok := loadCredentials()
	if !ok || saved.Token != "new-token" || saved.Password != "secret" {
		t.Errorf("saved = %+v, %v", saved, ok)
	}
	if _, err := renewCredentials(srv.URL+"/auth/", storedCredentials{Identifier: "cap", Token: "old"}); err == nil {
		t.Error("renewing without a remembered password must fail")
	}
}

// A refused password is forgotten (the player signs in again); a server that
// cannot be reached keeps it, so the next try can still work.
func TestRenewForgetsARefusedPasswordOnly(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	srv := authStub(t, "new-password")
	saved := storedCredentials{Identifier: "cap", Username: "Captain", Password: "old-password", Token: "old"}
	a := &launcherAPI{cfg: Config{AuthURL: srv.URL + "/auth/"}, saved: saved, username: "Captain"}
	if _, _, err := a.renew(); err == nil || unreachable(err) {
		t.Fatalf("refused renew: err = %v", err)
	}
	if a.saved.Password != "" {
		t.Error("a refused password must be forgotten")
	}
	if stored, ok := loadCredentials(); !ok || stored.Password != "" || stored.Identifier != "cap" {
		t.Errorf("stored after refusal = %+v, %v (identifier kept, password gone)", stored, ok)
	}

	srv.Close()
	a.saved = saved
	if _, _, err := a.renew(); err == nil || !unreachable(err) {
		t.Fatalf("unreachable renew: err = %v", err)
	}
	if a.saved.Password != "old-password" {
		t.Error("an unreachable server must not cost the remembered password")
	}
}

// Without "Remember me" an expired token signs the player out (the form keeps
// the identifier); with it the player stays signed in.
func TestLoadSaved(t *testing.T) {
	t.Setenv("LOCALAPPDATA", t.TempDir())
	expired := "x.eyJleHAiOjF9.y" // exp 1970
	for _, c := range []struct {
		name     string
		creds    storedCredentials
		signedIn bool
	}{
		{"expired, no password", storedCredentials{Identifier: "cap", Username: "Captain", Token: expired}, false},
		{"expired, remembered", storedCredentials{Identifier: "cap", Username: "Captain", Token: expired, Password: "p"}, true},
	} {
		if err := saveCredentials(c.creds); err != nil {
			t.Fatal(err)
		}
		a := &launcherAPI{}
		a.loadSaved()
		if got := a.username != ""; got != c.signedIn {
			t.Errorf("%s: signed in = %v, want %v", c.name, got, c.signedIn)
		}
		if a.saved.Identifier != "cap" {
			t.Errorf("%s: identifier = %q", c.name, a.saved.Identifier)
		}
	}
}
