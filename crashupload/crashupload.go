package crashupload

// Where the game's crash report is, verified in DreadGame-Win64-Shipping.exe
// (2026-11-23; mmogbrain client_crashes.go has the addresses): the crash
// handler files a Windows Error Report -- WerReportCreate, the P0
// "Application Name" "UE4-DreadGame", five WerReportAddFile (the minidump
// UE4Minidump.dmp, four other files) and WerReportSubmit with
// WER_SUBMIT_QUEUE -- so Windows keeps it in a folder of its report queue,
// with the added files copied in. The game's CrashReportClient then looks for
// it under %LOCALAPPDATA%\Microsoft\Windows\WER\ReportQueue (a folder whose
// name holds "UE4-") and would send it to Greybox's crash server, which is
// gone.
//
// GUESS: ReportArchive, and the same two folders under %ProgramData%, are
// where Windows puts a report it has handled or queued for the whole machine;
// a report there is read the same way. Folders named after the exe
// (DreadGame-Win64-Shipping) are Windows' own report of a crash the game's
// handler did not catch.

import (
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/logupload"
)

const (
	// MaxFile: a larger file (a full-memory dump) is left out.
	MaxFile = 96 << 20
	// MaxTotal: files past this, smallest first, are left out.
	MaxTotal = 160 << 20
	// MaxFiles: the server takes at most 32.
	MaxFiles = 24
	// PartSize: the size of each upload part (of the zip).
	PartSize = 4 << 20
)

// Report is a crash report folder.
type Report struct {
	Dir      string
	Name     string // the folder's name; the server stores a report once by it
	Modified time.Time
}

// Root is a folder holding crash report folders, and which of them are the
// game's.
type Root struct {
	Dir   string
	Match func(name string) bool
}

// GameReportName reports whether a Windows Error Report folder is the game's:
// it names the game's application name or its exe.
func GameReportName(name string) bool {
	n := strings.ToLower(name)
	return strings.Contains(n, "ue4-dreadgame") || strings.Contains(n, "dreadgame-win64-shipping")
}

// Find lists the report folders under roots modified at or after since,
// newest first. A folder without a single file is no report.
func Find(roots []Root, since time.Time) []Report {
	var out []Report
	seen := map[string]bool{}
	for _, root := range roots {
		entries, err := os.ReadDir(root.Dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || (root.Match != nil && !root.Match(e.Name())) {
				continue
			}
			dir := filepath.Join(root.Dir, e.Name())
			mod, ok := newestFile(dir)
			if !ok || mod.Before(since) || seen[strings.ToLower(e.Name())] {
				continue
			}
			seen[strings.ToLower(e.Name())] = true
			out = append(out, Report{Dir: dir, Name: e.Name(), Modified: mod})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// newestFile is the newest modification time of the files directly in dir.
func newestFile(dir string) (time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var t time.Time
	found := false
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if info, err := e.Info(); err == nil {
			found = true
			if info.ModTime().After(t) {
				t = info.ModTime()
			}
		}
	}
	return t, found
}

// skipFile: heap dumps (a whole process's memory) and screen videos.
func skipFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".hdmp", ".avi", ".tmp":
		return true
	}
	return false
}

// Packed is a report packed into a zip.
type Packed struct {
	Zip     string
	Files   int
	Bytes   int64    // the files' size
	Skipped []string // files left out, and why
}

// Pack writes the report's files, scrubbed (Scrub with needles), into a zip
// at dst. Minidumps come first; files past the limits are left out.
func Pack(r Report, dst string, needles []string) (*Packed, error) {
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return nil, err
	}
	type file struct {
		name string
		size int64
	}
	var files []file
	p := &Packed{Zip: dst}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		switch {
		case skipFile(e.Name()):
			p.Skipped = append(p.Skipped, e.Name()+" (not sent)")
		case info.Size() > MaxFile:
			p.Skipped = append(p.Skipped, fmt.Sprintf("%s (%d MB, too large)", e.Name(), info.Size()>>20))
		case info.Size() > 0:
			files = append(files, file{e.Name(), info.Size()})
		}
	}
	// Dumps first, then the smallest: what fits is what helps most.
	isDump := func(n string) bool {
		x := strings.ToLower(filepath.Ext(n))
		return x == ".dmp" || x == ".mdmp"
	}
	sort.Slice(files, func(i, j int) bool {
		if isDump(files[i].name) != isDump(files[j].name) {
			return isDump(files[i].name)
		}
		return files[i].size < files[j].size
	})
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	zw := zip.NewWriter(out)
	err = func() error {
		for _, f := range files {
			if p.Files >= MaxFiles || p.Bytes+f.size > MaxTotal {
				p.Skipped = append(p.Skipped, f.name+" (over the report limit)")
				continue
			}
			//nolint:gosec // a file in the game's own crash report folder.
			data, err := os.ReadFile(filepath.Join(r.Dir, f.name))
			if err != nil {
				p.Skipped = append(p.Skipped, f.name+" (unreadable)")
				continue
			}
			Scrub(data, needles)
			w, err := zw.CreateHeader(&zip.FileHeader{Name: zipName(f.name), Method: zip.Deflate, Modified: r.Modified})
			if err != nil {
				return err
			}
			if _, err := w.Write(data); err != nil {
				return err
			}
			p.Files++
			p.Bytes += int64(len(data))
		}
		return zw.Close()
	}()
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && p.Files == 0 {
		err = errors.New("the crash report has no files to send")
	}
	if err != nil {
		_ = os.Remove(dst)
		return nil, err
	}
	return p, nil
}

// zipName keeps the characters the server takes in a file name.
func zipName(n string) string {
	b := []byte(n)
	for i, c := range b {
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("._ ()-", c) >= 0
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 120 {
		b = append(b[:60], b[len(b)-60:]...)
	}
	return string(b)
}

// ReportName is the folder name as the server takes it (letters, digits,
// ". _ -" and spaces, at most 160).
func ReportName(n string) string {
	b := []byte(n)
	for i, c := range b {
		ok := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || strings.IndexByte("._ -", c) >= 0
		if !ok {
			b[i] = '_'
		}
	}
	if len(b) > 160 {
		b = b[:160]
	}
	s := strings.ReplaceAll(string(b), "..", "__")
	if s == "" {
		s = "report"
	}
	return s
}

// Split cuts the zip into gzip parts of PartSize in dir (which must exist),
// for logupload.Uploader.
func Split(zipPath, dir string) (*logupload.Manifest, error) {
	//nolint:gosec // the zip Pack wrote.
	f, err := os.Open(zipPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	m := &logupload.Manifest{Dir: dir}
	buf := make([]byte, PartSize)
	for i := 0; ; i++ {
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			path := filepath.Join(dir, fmt.Sprintf("part-%04d.gz", i))
			stored, err := writeGzip(path, buf[:n])
			if err != nil {
				return nil, err
			}
			m.Parts = append(m.Parts, logupload.Part{Path: path, Raw: int64(n), Stored: stored})
			m.Source += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return nil, rerr
		}
	}
	if len(m.Parts) == 0 {
		return nil, errors.New("empty zip")
	}
	return m, nil
}

func writeGzip(path string, data []byte) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	// A zip is compressed already; the gzip layer is only the part format.
	zw, _ := gzip.NewWriterLevel(f, gzip.BestSpeed)
	_, err = zw.Write(data)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}
