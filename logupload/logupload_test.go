package logupload

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"

// A few lines as the client writes them (names and token made up).
const testLog = "LogInit: Command line: /Game/Maps/Launch_P -noeac\r\n" +
	"[2026.11.17-07.59.31:453][  0]LogInit: Computer: DESKTOP-TEST01\r\n" +
	"[2026.11.17-07.59.31:453][  0]LogInit: User: testwinuser\r\n" +
	"[2026.11.17-07.59.31:500][  0]LogInit: Base directory: C:\\Users\\testwinuser\\Games\\Dreadnought\\\r\n" +
	"LogPaths: d:/profiles/tw/AppData/Local/DreadGame/Saved/ and D:\\Profiles\\tw\\x\r\n" +
	"[2026.11.17-07.59.42:735][ 51]LogWebServicesPlugin: Authorization: Bearer " + testToken + "\r\n" +
	"[2026.11.17-07.59.42:736][ 51]LogWebServicesPlugin: cut eyJhbGciOiJIUzI1\r\n" +
	"[2026.11.17-07.59.43:487][ 96]LogNet: DESKTOP-TEST01 and /Game/Maps/x GAME\r\n" +
	"[2026.11.17-07.59.44:000][ 96]LogWebServicesPlugin: Authorization: Session a0c2e098-0000-0000-0000-000000000000, TestPilot\r\n"

func TestRedactor(t *testing.T) {
	got := string(NewRedactor("DESKTOP-TEST01", `D:\Profiles\tw`).Apply([]byte(testLog)))
	for _, gone := range []string{"DESKTOP-TEST01", "testwinuser", "eyJ", "c2lnbmF0dXJl", "profiles/tw", `Profiles\tw`} {
		if strings.Contains(strings.ToLower(got), strings.ToLower(gone)) {
			t.Errorf("%q is still in the log:\n%s", gone, got)
		}
	}
	for _, kept := range []string{"Computer: <removed>\r\n", "User: <removed>\r\n", `C:\Users\<user>\Games`, "Bearer <token removed>\r\n",
		"<computer> and /Game/Maps/x GAME", "Session a0c2e098-0000-0000-0000-000000000000, TestPilot", "-noeac"} {
		if !strings.Contains(got, kept) {
			t.Errorf("want %q in the log:\n%s", kept, got)
		}
	}
	// A PC named like a game word does not blank every "/Game/".
	if got := string(NewRedactor("GAME", "").Apply([]byte("/Game/Maps/x GAME"))); got != "/Game/Maps/x GAME" {
		t.Errorf("short computer name changed the log: %q", got)
	}
	// The same text through the fast look-ups and without them.
	plain := "[2026.11.17-07.59.31:453][  0]LogNet: nothing private here\r\n"
	if got := string(NewRedactor("DESKTOP-TEST01", `D:\Profiles\tw`).Apply([]byte(plain))); got != plain {
		t.Errorf("a clean line changed: %q", got)
	}
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "DreadGame.log")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// joined is the parts' text, one after the other, and checks that the files
// are valid gzip and that every part but a marker ends at a line break.
func joined(t *testing.T, m *Manifest) string {
	t.Helper()
	var all strings.Builder
	for i, p := range m.Parts {
		f, err := os.Open(p.Path)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(zr)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if int64(len(b)) != p.Raw {
			t.Errorf("part %d: Raw says %d, the text is %d", i, p.Raw, len(b))
		}
		if !bytes.HasSuffix(b, []byte("\n")) {
			t.Errorf("part %d does not end at a line break: ...%q", i, b[max(0, len(b)-30):])
		}
		all.Write(b)
	}
	return all.String()
}

// manyLines is n numbered lines with a token and a computer name sprinkled in.
func manyLines(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "[2026.11.17-08.00.%02d:%03d][%3d]LogNet: line %d of the test log, padding padding padding padding\r\n", i%60, i%1000, i%1000, i)
		if i%7 == 3 {
			fmt.Fprintf(&b, "[2026.11.17-08.00.00:000][  0]LogWebServicesPlugin: Authorization: Bearer %s\r\n", testToken)
		}
		if i%11 == 5 {
			b.WriteString("[2026.11.17-08.00.00:000][  0]LogInit: Computer: DESKTOP-TEST01\r\n")
		}
	}
	return b.String()
}

// A log under the cap comes out whole, cut at line breaks into several parts,
// with nothing private left -- also where a token sits on a part boundary.
func TestSpoolWholeLog(t *testing.T) {
	red := NewRedactor("DESKTOP-TEST01", "")
	for _, partSize := range []int64{1 << 10, 4097, 64 << 10} {
		src := manyLines(3000)
		m, err := Spool(writeFile(t, src), filepath.Join(t.TempDir(), "spool"), red, Options{PartSize: partSize})
		if err == nil {
			t.Fatal("Spool into a folder that does not exist must fail")
		}
		dir := t.TempDir()
		m, err = Spool(writeFile(t, src), dir, red, Options{PartSize: partSize})
		if err != nil {
			t.Fatal(err)
		}
		got := joined(t, m)
		want := string(red.Apply([]byte(src)))
		if got != want {
			t.Fatalf("part size %d: joined parts differ from the redacted log (%d vs %d bytes)", partSize, len(got), len(want))
		}
		if strings.Contains(got, "eyJ") || strings.Contains(got, "DESKTOP-TEST01") {
			t.Errorf("part size %d: something private is left", partSize)
		}
		if partSize == 1<<10 && len(m.Parts) < 50 {
			t.Errorf("part size %d: only %d parts", partSize, len(m.Parts))
		}
		if m.Cut != 0 || m.Source != int64(len(src)) {
			t.Errorf("Cut %d Source %d", m.Cut, m.Source)
		}
		m.Remove()
		if _, err := os.Stat(dir); err == nil {
			t.Errorf("Remove left the folder")
		}
	}
}

// Redaction does not depend on where the parts end: every cut of a log with a
// token on each line.
func TestSpoolTokenNeverSplit(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "x%d Bearer %s tail\n", i, testToken)
	}
	for ps := int64(90); ps < 220; ps += 7 {
		dir := t.TempDir()
		m, err := Spool(writeFile(t, b.String()), dir, NewRedactor("", ""), Options{PartSize: ps})
		if err != nil {
			t.Fatal(err)
		}
		if got := joined(t, m); strings.Contains(got, "eyJ") || strings.Contains(got, "c2lnbmF0dXJl") || strings.Count(got, "<token removed>") != 400 {
			t.Fatalf("part size %d: tokens left or lost (%d markers)", ps, strings.Count(got, "<token removed>"))
		}
	}
}

// One endless line (no line break at all) is still cut, at a safe place, and
// nothing is lost.
func TestSpoolNoLineBreaks(t *testing.T) {
	src := strings.Repeat("abc def ", 5000)
	dir := t.TempDir()
	m, err := Spool(writeFile(t, src), dir, nil, Options{PartSize: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var all strings.Builder
	for _, p := range m.Parts {
		f, _ := os.Open(p.Path)
		zr, _ := gzip.NewReader(f)
		b, _ := io.ReadAll(zr)
		_ = f.Close()
		all.Write(b)
	}
	if all.String() != src || len(m.Parts) < 10 {
		t.Fatalf("%d parts, %d of %d bytes", len(m.Parts), all.Len(), len(src))
	}
}

// A log past the cap keeps its start and its end, whole lines, with the marker
// in between that says how much is missing.
func TestSpoolCutsTheMiddle(t *testing.T) {
	src := manyLines(5000)
	o := Options{MaxRaw: int64(len(src) / 3), KeepTop: int64(len(src) / 10), PartSize: 2000}
	m, err := Spool(writeFile(t, src), t.TempDir(), nil, o)
	if err != nil {
		t.Fatal(err)
	}
	got := joined(t, m)
	i := strings.Index(got, "[launcher: ")
	if i < 0 {
		t.Fatal("no cut marker")
	}
	var cut int64
	if _, err := fmt.Sscanf(got[i:], "[launcher: %d bytes of the log cut here]", &cut); err != nil {
		t.Fatal(err)
	}
	head := got[:i-2] // before the marker's own "\r\n"
	tail := got[i+len(CutMarker(cut))-2:]
	if !strings.HasPrefix(src, head) || !strings.HasSuffix(src, tail) {
		t.Fatalf("head/tail are not the start/end of the log")
	}
	if !strings.HasSuffix(head, "\n") || !strings.HasPrefix(tail, "[2026.") {
		t.Errorf("a line was cut in two: head ends %q, tail starts %q", head[max(0, len(head)-20):], tail[:20])
	}
	if int64(len(head)+len(tail))+cut != int64(len(src)) || cut != m.Cut {
		t.Errorf("head %d + tail %d + cut %d != source %d (m.Cut %d)", len(head), len(tail), cut, len(src), m.Cut)
	}
	if int64(len(head)) > o.KeepTop || int64(len(head)+len(tail)) > o.MaxRaw {
		t.Errorf("sent %d (head %d), caps %d / %d", len(head)+len(tail), len(head), o.MaxRaw, o.KeepTop)
	}
	if int64(len(got)) > o.MaxRaw+int64(len(CutMarker(cut))) {
		t.Errorf("over the cap: %d", len(got))
	}
}

// fakeServer is the server side of the chunked protocol, as mmogbrain does it.
type fakeServer struct {
	mu       sync.Mutex
	parts    map[string]map[int][]byte
	commits  int
	dupOn    string // "part" or "commit": answer duplicate there
	failPart int    // answer 500 to this many part requests first
	dropPart int    // forget this part after it arrived (once)
	dropped  bool
	status   int // fixed status for everything, if set
	got      []byte
	query    string
}

func (f *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer tok" {
		http.Error(w, `{"error":"sign in first"}`, 401)
		return
	}
	if f.status != 0 {
		http.Error(w, `{"error":"x"}`, f.status)
		return
	}
	q := r.URL.Query()
	id := q.Get("id")
	switch r.URL.Path {
	case "/launcher/log/part":
		if f.failPart > 0 {
			f.failPart--
			http.Error(w, `{"error":"boom"}`, 500)
			return
		}
		if f.dupOn == "part" {
			fmt.Fprint(w, `{"ok":true,"id":7,"duplicate":true}`)
			return
		}
		n, _ := strconv.Atoi(q.Get("n"))
		body, _ := io.ReadAll(r.Body)
		if f.parts == nil {
			f.parts = map[string]map[int][]byte{}
		}
		if f.parts[id] == nil {
			f.parts[id] = map[int][]byte{}
		}
		f.parts[id][n] = body
		fmt.Fprintf(w, `{"ok":true,"part":%d}`, n)
	case "/launcher/log/commit":
		f.commits++
		f.query = r.URL.RawQuery
		count, _ := strconv.Atoi(q.Get("parts"))
		if f.dropPart > 0 && !f.dropped {
			f.dropped = true
			delete(f.parts[id], f.dropPart)
		}
		var missing []int
		for i := 0; i < count; i++ {
			if _, ok := f.parts[id][i]; !ok {
				missing = append(missing, i)
			}
		}
		if len(missing) > 0 {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "parts missing", "missing": missing})
			return
		}
		for i := 0; i < count; i++ {
			f.got = append(f.got, f.parts[id][i]...)
		}
		fmt.Fprint(w, `{"ok":true,"id":1,"duplicate":false}`)
	case "/launcher/log":
		f.got, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"ok":true,"id":2,"duplicate":false}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeServer) text(t *testing.T) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(f.got))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func newUploader(srv *httptest.Server) *Uploader {
	return &Uploader{Client: srv.Client(), Base: srv.URL + "/launcher/log", Token: "tok", Backoff: time.Millisecond}
}

func spooled(t *testing.T, n int, o Options) (*Manifest, string) {
	t.Helper()
	src := manyLines(n)
	m, err := Spool(writeFile(t, src), t.TempDir(), NewRedactor("DESKTOP-TEST01", ""), o)
	if err != nil {
		t.Fatal(err)
	}
	return m, joined(t, m)
}

func TestUpload(t *testing.T) {
	fake := &fakeServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	m, want := spooled(t, 2000, Options{PartSize: 20 << 10})
	if len(m.Parts) < 5 {
		t.Fatalf("%d parts", len(m.Parts))
	}
	u := newUploader(srv)
	var lastSent int
	u.Progress = func(sent, total int) { lastSent = sent }
	fake.failPart = 2 // two 500s on the way: retried
	id := NewUploadID()
	if len(id) != 32 || id == NewUploadID() {
		t.Fatalf("upload id %q", id)
	}
	q := map[string][]string{"modified": {"1763366371"}, "exit": {"0"}, "version": {"1.0.11"}}
	if err := u.Upload(m, id, q); err != nil {
		t.Fatal(err)
	}
	if lastSent != len(m.Parts) || fake.commits != 1 {
		t.Errorf("progress %d, commits %d", lastSent, fake.commits)
	}
	if got := fake.text(t); got != want {
		t.Fatalf("the server joined %d bytes, want %d", len(got), len(want))
	}
	for _, k := range []string{"id=" + id, "parts=" + strconv.Itoa(len(m.Parts)), "modified=1763366371", "version=1.0.11"} {
		if !strings.Contains(fake.query, k) {
			t.Errorf("commit query %q lacks %q", fake.query, k)
		}
	}
}

// The server lost a part: the commit says so and only that part is sent again.
func TestUploadResendsMissingPart(t *testing.T) {
	fake := &fakeServer{dropPart: 2}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	m, want := spooled(t, 1500, Options{PartSize: 20 << 10})
	if err := newUploader(srv).Upload(m, NewUploadID(), map[string][]string{"modified": {"5"}}); err != nil {
		t.Fatal(err)
	}
	if fake.commits != 2 || fake.text(t) != want {
		t.Fatalf("commits %d, log ok: %v", fake.commits, fake.text(t) == want)
	}
}

// A server that has the log already ends the upload at once; refusals and
// old servers are told apart from hiccups.
func TestUploadAnswers(t *testing.T) {
	fake := &fakeServer{dupOn: "part"}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	m, _ := spooled(t, 500, Options{PartSize: 10 << 10})
	u := newUploader(srv)
	if err := u.Upload(m, NewUploadID(), map[string][]string{"modified": {"5"}}); err != nil || fake.commits != 0 {
		t.Fatalf("duplicate: err %v, commits %d", err, fake.commits)
	}
	for status, check := range map[int]func(error) bool{
		400: func(err error) bool { var r *RefusedError; return asRefused(err, &r) && r.Status == 400 },
		413: func(err error) bool { var r *RefusedError; return asRefused(err, &r) && r.Status == 413 },
		404: func(err error) bool { return err == ErrNotSupported },
		405: func(err error) bool { return err == ErrNotSupported },
		429: func(err error) bool {
			return err != nil && err != ErrNotSupported && !strings.Contains(err.Error(), "refused")
		},
		500: func(err error) bool { return err != nil && strings.Contains(err.Error(), "500") },
	} {
		fake.status, fake.dupOn = status, ""
		u.Attempts = 2
		if err := u.Upload(m, NewUploadID(), map[string][]string{}); !check(err) {
			t.Errorf("HTTP %d: err %v", status, err)
		}
	}
	// A wrong token is not retried into a ban: 401 comes back at once.
	fake.status = 0
	u.Token = "wrong"
	if err := u.Upload(m, NewUploadID(), map[string][]string{}); err == nil || !strings.Contains(err.Error(), "401") {
		t.Errorf("wrong token: %v", err)
	}
}

func asRefused(err error, out **RefusedError) bool {
	r, ok := err.(*RefusedError)
	*out = r
	return ok
}

// The one-POST body for a server without the chunked endpoint: start, marker,
// end, within the cap, and a whole log when it fits.
func TestLegacyBody(t *testing.T) {
	m, whole := spooled(t, 3000, Options{PartSize: 8 << 10})
	body, err := Legacy(m, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(zr); string(b) != whole {
		t.Fatalf("a log that fits must go whole: %d vs %d bytes", len(b), len(whole))
	}

	cap := int64(len(whole) / 3)
	body, err = Legacy(m, cap)
	if err != nil {
		t.Fatal(err)
	}
	zr, err = gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(zr)
	got := string(b)
	if int64(len(got)) > cap || !strings.Contains(got, "bytes of the log cut here]") || !strings.HasPrefix(whole, got[:1000]) || !strings.HasSuffix(whole, got[len(got)-1000:]) {
		t.Fatalf("legacy cut: %d bytes (cap %d), marker %v", len(got), cap, strings.Contains(got, "cut here"))
	}

	fake := &fakeServer{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	u := newUploader(srv)
	if err := u.SendLegacy(srv.URL+"/launcher/log?modified=1", body); err != nil || fake.text(t) != got {
		t.Fatalf("SendLegacy: %v", err)
	}
}

// The line-by-line shortcuts give what the plain rules give on the whole text
// (the rules the launcher used up to v1.0.10).
func TestRedactorEqualsPlainRules(t *testing.T) {
	plain := func(b []byte, computer, profile string) []byte {
		b = jwtRE.ReplaceAll(b, []byte("<token removed>"))
		b = machineRE.ReplaceAll(b, []byte("${1}<removed>"))
		b = userDirRE.ReplaceAll(b, []byte("${1}<user>"))
		if len(profile) > 3 {
			for _, p := range []string{profile, strings.ReplaceAll(profile, `\`, "/")} {
				b = regexpFold(b, regexpQuote(p), "<user folder>")
			}
		}
		if len(computer) >= 5 {
			b = regexpFold(b, `\b`+regexpQuote(computer)+`\b`, "<computer>")
		}
		return b
	}
	lines := []string{"", "plain line", "C:\\USERS\\Bob\\x and c:/users/bob/y", "d:\\Profiles\\TW\\z", "D:/profiles/tw/z", "my pc desktop-test01 here",
		"DESKTOP-TEST01", "x DESKTOP-TEST011 y", "LogInit: User: bob", "tok " + testToken, "no newline at the end"}
	for seed := 0; seed < 200; seed++ {
		var b strings.Builder
		for i := 0; i < 1+seed%9; i++ {
			b.WriteString(lines[(seed*7+i*3)%len(lines)])
			if i < seed%9 || seed%2 == 0 {
				b.WriteString("\r\n")
			}
		}
		want := string(plain([]byte(b.String()), "DESKTOP-TEST01", `D:\Profiles\tw`))
		if got := string(NewRedactor("DESKTOP-TEST01", `D:\Profiles\tw`).Apply([]byte(b.String()))); got != want {
			t.Fatalf("seed %d:\n%q\ngot  %q\nwant %q", seed, b.String(), got, want)
		}
	}
}

func regexpQuote(s string) string { return regexp.QuoteMeta(s) }

func regexpFold(b []byte, pat, repl string) []byte {
	return regexp.MustCompile(`(?i)`+pat).ReplaceAllLiteral(b, []byte(repl))
}
