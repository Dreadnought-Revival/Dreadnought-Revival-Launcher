//go:build windows

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The clock goes out with the token, as the server's /launcher/clock reads it.
func TestReportClock(t *testing.T) {
	var got struct {
		Offset  *int   `json:"utc_offset"`
		Pattern string `json:"pattern"`
		AM      string `json:"am"`
		PM      string `json:"pm"`
	}
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	if err := reportClock(srv.Client(), srv.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if auth != "Bearer tok" || got.Offset == nil || *got.Offset < -840 || *got.Offset > 840 || got.Pattern == "" {
		t.Errorf("sent auth %q, clock %+v", auth, got)
	}
	t.Logf("offset %d, pattern %q, am %q, pm %q", *got.Offset, got.Pattern, got.AM, got.PM)
}
