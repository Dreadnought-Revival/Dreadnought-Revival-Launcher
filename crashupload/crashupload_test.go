package crashupload

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/logupload"
)

func wide(s string) []byte {
	var b []byte
	for _, u := range utf16.Encode([]rune(s)) {
		b = append(b, byte(u), byte(u>>8))
	}
	return b
}

func write(t *testing.T, path string, data []byte, mod time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mod, mod); err != nil {
		t.Fatal(err)
	}
}

// Same-length: every byte stays where it was; names and tokens are gone in
// both encodings; the launcher's own needles go too.
func TestScrub(t *testing.T) {
	tok := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4In0.c2lnbmF0dXJl"
	in := append([]byte("cmd -token="+tok+" C:\\Users\\Jane\\x \x00"), wide("COMPUTERNAME=GAMER-PC\x00D:\\Profiles\\jane7\\y\x00")...)
	in = append(in, "LogInit: User: jane\r\nfree text gamer-pc \xff\xfe\x00"...)
	out := append([]byte(nil), in...)
	Scrub(out, []string{"GAMER-PC", "D:\\Profiles\\jane7"})
	if len(out) != len(in) {
		t.Fatalf("length %d -> %d", len(in), len(out))
	}
	for _, gone := range [][]byte{[]byte(tok), []byte("Jane"), wide("GAMER-PC"), wide("jane7"), []byte("gamer-pc"), []byte("User: jane")} {
		if bytes.Contains(out, gone) {
			t.Errorf("still holds %q", gone)
		}
	}
	if !bytes.Contains(out, []byte("C:\\Users\\****\\x")) || !bytes.HasSuffix(out, []byte(" \xff\xfe\x00")) {
		t.Errorf("scrubbed too much: %q", out)
	}
}

// Find: only the game's report folders (by name) under WER roots, any folder
// under a root without a filter, only those written since the game started,
// newest first.
func TestFind(t *testing.T) {
	base := t.TempDir()
	start := time.Now().Add(-10 * time.Minute)
	queue := filepath.Join(base, "WER", "ReportQueue")
	write(t, filepath.Join(queue, "AppCrash_UE4-DreadGame_abc_123", "Report.wer"), []byte("x"), start.Add(time.Minute))
	write(t, filepath.Join(queue, "AppCrash_DreadGame-Win64-Ship_def", "Report.wer"), []byte("x"), start.Add(2*time.Minute))
	write(t, filepath.Join(queue, "AppCrash_DREADGAME-WIN64-SHIPPING_ghi", "Report.wer"), []byte("x"), start.Add(3*time.Minute))
	write(t, filepath.Join(queue, "AppCrash_notepad.exe_1", "Report.wer"), []byte("x"), start.Add(time.Minute))
	write(t, filepath.Join(queue, "AppCrash_UE4-DreadGame_old", "Report.wer"), []byte("x"), start.Add(-time.Hour))
	if err := os.MkdirAll(filepath.Join(queue, "AppCrash_UE4-DreadGame_empty"), 0o700); err != nil {
		t.Fatal(err)
	}
	crashes := filepath.Join(base, "Saved", "Crashes")
	write(t, filepath.Join(crashes, "UE4CC-Windows-1234", "UE4Minidump.dmp"), []byte("MDMP"), start.Add(30*time.Second))

	got := Find([]Root{{Dir: queue, Match: GameReportName}, {Dir: crashes}, {Dir: filepath.Join(base, "missing")}}, start)
	var names []string
	for _, r := range got {
		names = append(names, r.Name)
	}
	want := "AppCrash_DREADGAME-WIN64-SHIPPING_ghi,AppCrash_UE4-DreadGame_abc_123,UE4CC-Windows-1234"
	if strings.Join(names, ",") != want {
		t.Errorf("found %v, want %s", names, want)
	}
}

// Pack: dumps first, heap dumps and too-large files left out, every file
// scrubbed; Split's parts, unpacked and joined, are the zip.
func TestPackAndSplit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "AppCrash_UE4-DreadGame_x")
	mod := time.Now()
	dump := append([]byte("MDMP"), wide("C:\\Users\\Jane\\Game\x00")...)
	write(t, filepath.Join(dir, "UE4Minidump.dmp"), dump, mod)
	write(t, filepath.Join(dir, "Report.wer"), wide("AppPath=C:\\Users\\Jane\\DreadGame.exe\r\n"), mod)
	write(t, filepath.Join(dir, "memory.hdmp"), []byte("huge"), mod)
	write(t, filepath.Join(dir, "DreadGame.log"), []byte("LogInit: Computer: GAMER-PC\r\n"), mod)
	write(t, filepath.Join(dir, "CrashReportClient.ini"), []byte("[CrashReportClient]\r\n"), mod)

	out := filepath.Join(t.TempDir(), "r.zip")
	p, err := Pack(Report{Dir: dir, Name: "AppCrash_UE4-DreadGame_x", Modified: mod}, out, []string{"GAMER-PC"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Files != 3 || len(p.Skipped) != 2 {
		t.Fatalf("packed %+v", p)
	}
	zr, err := zip.OpenReader(out)
	if err != nil {
		t.Fatal(err)
	}
	if zr.File[0].Name != "UE4Minidump.dmp" {
		t.Errorf("first file %s, want the dump", zr.File[0].Name)
	}
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		if bytes.Contains(data, wide("Jane")) || bytes.Contains(data, []byte("GAMER-PC")) {
			t.Errorf("%s: %q", f.Name, data)
		}
		if f.Name == "UE4Minidump.dmp" && len(data) != len(dump) {
			t.Errorf("dump changed size")
		}
	}
	_ = zr.Close()

	parts := t.TempDir()
	m, err := Split(out, parts)
	if err != nil {
		t.Fatal(err)
	}
	var joined bytes.Buffer
	for _, part := range m.Parts {
		f, _ := os.Open(part.Path)
		gz, err := gzip.NewReader(f)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(&joined, gz)
		_ = f.Close()
	}
	orig, _ := os.ReadFile(out)
	if !bytes.Equal(joined.Bytes(), orig) {
		t.Error("parts do not join into the zip")
	}
}

// The uploader names the report on every part and on the commit.
func TestUploadNamesTheReport(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path+" report="+r.URL.Query().Get("report"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	zipPath := filepath.Join(t.TempDir(), "r.zip")
	if err := os.WriteFile(zipPath, bytes.Repeat([]byte{7}, PartSize+10), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := Split(zipPath, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	q := url.Values{"report": {"AppCrash_UE4-DreadGame_x"}}
	u := &logupload.Uploader{Client: srv.Client(), Base: srv.URL + "/launcher/crash", Token: "t", PartQuery: q}
	if err := u.Upload(m, logupload.NewUploadID(), q); err != nil {
		t.Fatal(err)
	}
	want := "/launcher/crash/part report=AppCrash_UE4-DreadGame_x|/launcher/crash/part report=AppCrash_UE4-DreadGame_x|/launcher/crash/commit report=AppCrash_UE4-DreadGame_x"
	if strings.Join(seen, "|") != want {
		t.Errorf("requests %v", seen)
	}
}

// A UE4CC folder holding only the crash reporter's settings is no report.
func TestPackRefusesASettingsOnlyFolder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "UE4CC-Windows-ABC")
	write(t, filepath.Join(dir, "CrashReportClient.ini"), []byte("[CrashReportClient]\r\n"), time.Now())
	if _, err := Pack(Report{Dir: dir, Name: "UE4CC-Windows-ABC"}, filepath.Join(t.TempDir(), "r.zip"), nil); err == nil {
		t.Error("packed a folder with only CrashReportClient.ini")
	}
}

func TestReportName(t *testing.T) {
	for in, want := range map[string]string{
		"AppCrash_UE4-DreadGame_a1b2": "AppCrash_UE4-DreadGame_a1b2",
		"a/../b":                      "a____b",
		"Kritisch_Größe":              "Kritisch_Gr____e", // byte-wise: two per UTF-8 letter
	} {
		if got := ReportName(in); got != want {
			t.Errorf("ReportName(%q) = %q, want %q", in, got, want)
		}
	}
}
