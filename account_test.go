//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The config's auth_url points at the login route itself (".../auth/"), so the
// register endpoint has to be built from a trimmed base rather than appended to
// it -- otherwise it becomes ".../auth//register".
func TestAuthBaseURLTrimsTheTrailingSlash(t *testing.T) {
	for _, tc := range []struct{ in, wantRegister, wantLogin string }{
		{"https://host/auth/", "https://host/auth/register", "https://host/auth/"},
		{"https://host/auth", "https://host/auth/register", "https://host/auth/"},
		{"  https://host/auth/  ", "https://host/auth/register", "https://host/auth/"},
	} {
		base := authBaseURL(tc.in)
		if got := base + "/register"; got != tc.wantRegister {
			t.Errorf("register endpoint for %q = %q, want %q", tc.in, got, tc.wantRegister)
		}
		if got := base + "/"; got != tc.wantLogin {
			t.Errorf("login endpoint for %q = %q, want %q", tc.in, got, tc.wantLogin)
		}
	}
}

func TestFirstNonEmptyPicksTheFirstPopulatedToken(t *testing.T) {
	if got := firstNonEmpty("", "", "jwt-value", "later"); got != "jwt-value" {
		t.Errorf("firstNonEmpty = %q, want jwt-value", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("firstNonEmpty with nothing set = %q, want empty", got)
	}
}

// Sign-in failures are shown to the player, so the server's own wording is
// preferred over a generic message when it sends one.
func TestServerMessagePrefersTheServersWording(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"greybox error envelope", `{"error":{"code":-32006,"message":"Invalid username or password"}}`, "Invalid username or password"},
		{"plain message", `{"message":"username or email already taken"}`, "username or email already taken"},
		{"unfamiliar shape", `{"weird":true}`, "fallback text"},
		{"not json at all", `<html>502</html>`, "fallback text"},
	} {
		if got := serverMessage([]byte(tc.body), "fallback text"); got != tc.want {
			t.Errorf("%s: serverMessage = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestCapitaliseLeavesEmptyAlone(t *testing.T) {
	if got := capitalise("incorrect email or password"); got != "Incorrect email or password" {
		t.Errorf("capitalise = %q", got)
	}
	if got := capitalise(""); got != "" {
		t.Errorf("capitalise(empty) = %q, want empty", got)
	}
}

// "Forgot password?": both calls reach the server's routes under the auth
// base and show its message; a refusal shows the server's reason.
func TestPasswordResetCallsTheServer(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/auth/password/forgot" && body["email"] == "pilot@example.com":
			_, _ = w.Write([]byte(`{"ok":true,"message":"A code is on its way."}`))
		case r.URL.Path == "/auth/password/reset" && body["code"] == "12345678" && body["password"] == "newpass1":
			_, _ = w.Write([]byte(`{"ok":true,"message":"Changed."}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"code":-32011,"message":"That code is wrong or has expired."}}`))
		}
	}))
	defer srv.Close()
	auth := srv.URL + "/auth/"
	if msg, err := requestPasswordResetCode(auth, "pilot@example.com"); err != nil || msg != "A code is on its way." {
		t.Errorf("send: %q %v", msg, err)
	}
	if msg, err := resetPasswordWithCode(auth, "pilot@example.com", "12345678", "newpass1"); err != nil || msg != "Changed." {
		t.Errorf("set: %q %v", msg, err)
	}
	if _, err := resetPasswordWithCode(auth, "pilot@example.com", "99999999", "newpass1"); err == nil || err.Error() != "That code is wrong or has expired." {
		t.Errorf("wrong code: %v", err)
	}
	if len(paths) != 3 || paths[0] != "/auth/password/forgot" || paths[1] != "/auth/password/reset" {
		t.Errorf("paths %v", paths)
	}
}
