package logupload

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Limits of one upload. The server enforces its own (a log of up to 512 MiB,
// 512 parts); these keep a player's PC and connection reasonable.
const (
	// DefaultMaxRaw is the most of a log that is sent; a longer one keeps its
	// start and its end. A whole session of the detailed log, or of the
	// script-warning flood some sessions make (14 KB/s), is far below it.
	DefaultMaxRaw = 256 << 20
	// DefaultKeepTop of a longer log is its start (the start-up lines: system,
	// command line, first connection).
	DefaultKeepTop = 8 << 20
	// DefaultPartSize is the text in one part before compression: a part is
	// about 250 KB to 1 MB on the wire, which fits the servers' 15 s request
	// time at a modest connection.
	DefaultPartSize = 4 << 20
)

// Options of Spool; zero fields take the defaults above.
type Options struct {
	MaxRaw, KeepTop, PartSize int64
}

func (o Options) withDefaults() Options {
	if o.MaxRaw <= 0 {
		o.MaxRaw = DefaultMaxRaw
	}
	if o.KeepTop <= 0 {
		o.KeepTop = DefaultKeepTop
	}
	if o.PartSize <= 0 {
		o.PartSize = DefaultPartSize
	}
	if o.KeepTop > o.MaxRaw/2 {
		o.KeepTop = o.MaxRaw / 2
	}
	return o
}

// Part is one gzip-compressed piece of the log on disk.
type Part struct {
	Path   string
	Raw    int64 // text bytes (after redaction)
	Stored int64 // compressed bytes
}

// Manifest is a log cut into parts in a folder. Joined in order and
// decompressed the parts are the log without the private parts; every part
// but a marker ends at a line break.
type Manifest struct {
	Dir   string
	Parts []Part
	// Source is the size of the log file; Cut how much of it is not in the
	// parts (the middle of a log past MaxRaw, and the line cut on each side).
	Source, Cut int64
}

// Raw is the text of all parts; Stored their size on disk.
func (m *Manifest) Raw() (n int64) {
	for _, p := range m.Parts {
		n += p.Raw
	}
	return n
}

// Stored is the compressed size of all parts.
func (m *Manifest) Stored() (n int64) {
	for _, p := range m.Parts {
		n += p.Stored
	}
	return n
}

// Remove deletes the parts' folder.
func (m *Manifest) Remove() { _ = os.RemoveAll(m.Dir) }

// CutMarker is the line that stands where the middle of a long log was left out.
func CutMarker(cut int64) string {
	return fmt.Sprintf("\r\n[launcher: %d bytes of the log cut here]\r\n", cut)
}

// Spool reads the log file in blocks, removes the private parts, and writes the
// gzip parts to dir (which must exist and be empty). Memory use is a few
// parts, whatever the size of the log. A log longer than MaxRaw keeps its first
// KeepTop bytes and its end, with CutMarker between.
func Spool(src, dir string, red *Redactor, o Options) (*Manifest, error) {
	o = o.withDefaults()
	//nolint:gosec // the game's own log, in a folder the launcher found.
	f, err := os.Open(src)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	m := &Manifest{Dir: dir, Source: size}
	w := &partWriter{m: m, red: red}

	if size <= o.MaxRaw {
		err = w.segment(f, 0, size, o.PartSize)
	} else {
		tail := o.MaxRaw - o.KeepTop
		headEnd, tailStart := o.KeepTop, size-tail
		// The start ends at its last complete line, the end begins at its
		// first, so no line is cut in two.
		var sent int64
		if sent, err = w.segmentTrim(f, 0, headEnd, o.PartSize, false, true); err == nil {
			if err = w.marker(size - sent - tail); err == nil {
				var tailSent int64
				if tailSent, err = w.segmentTrim(f, tailStart, size, o.PartSize, true, false); err == nil {
					// The marker's number is only known now: rewrite it.
					m.Cut = size - sent - tailSent
					err = w.rewriteMarker(m.Cut)
				}
			}
		}
	}
	if err != nil {
		m.Remove()
		return nil, err
	}
	if len(m.Parts) == 0 {
		m.Remove()
		return nil, io.ErrUnexpectedEOF
	}
	return m, nil
}

// partWriter appends parts to a manifest.
type partWriter struct {
	m   *Manifest
	red *Redactor
	// markerIdx is the part holding the cut marker (rewritten once the cut is known).
	markerIdx int
}

func (w *partWriter) add(text []byte) error {
	if len(text) == 0 {
		return nil
	}
	if w.red != nil {
		text = w.red.Apply(text)
	}
	path := filepath.Join(w.m.Dir, fmt.Sprintf("%06d.gz", len(w.m.Parts)))
	n, err := writeGzipFile(path, text)
	if err != nil {
		return err
	}
	w.m.Parts = append(w.m.Parts, Part{Path: path, Raw: int64(len(text)), Stored: n})
	return nil
}

func (w *partWriter) marker(cut int64) error {
	w.markerIdx = len(w.m.Parts)
	return w.add([]byte(CutMarker(cut)))
}

func (w *partWriter) rewriteMarker(cut int64) error {
	p := &w.m.Parts[w.markerIdx]
	text := []byte(CutMarker(cut))
	n, err := writeGzipFile(p.Path, text)
	if err != nil {
		return err
	}
	p.Raw, p.Stored = int64(len(text)), n
	return nil
}

func writeGzipFile(path string, text []byte) (int64, error) {
	//nolint:gosec // inside the launcher's own spool folder.
	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return 0, err
	}
	zw, _ := gzip.NewWriterLevel(out, gzip.BestSpeed)
	_, err = zw.Write(text)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return st.Size(), nil
}

// segment sends bytes [from, to) of the file, all of them.
func (w *partWriter) segment(f *os.File, from, to, part int64) error {
	_, err := w.segmentTrim(f, from, to, part, false, false)
	return err
}

// segmentTrim sends bytes [from, to) of the file as parts that end at line
// breaks. skipFirst drops the first, probably partial, line; dropLast the
// last, probably partial, one. It returns how many bytes of the file it
// covered (dropped ones not counted).
func (w *partWriter) segmentTrim(f *os.File, from, to, part int64, skipFirst, dropLast bool) (int64, error) {
	buf := make([]byte, 0, part+part/2)
	pos := from
	var covered int64
	skipping := skipFirst
	for {
		if pos < to {
			n := int(part)
			if rest := to - pos; rest < int64(n) {
				n = int(rest)
			}
			start := len(buf)
			if cap(buf) < start+n {
				grown := make([]byte, start, start+n+int(part)/2)
				copy(grown, buf)
				buf = grown
			}
			buf = buf[:start+n]
			read, err := f.ReadAt(buf[start:], pos)
			if err != nil && err != io.EOF {
				return covered, err
			}
			buf = buf[:start+read]
			pos += int64(read)
			if read < n {
				to = pos // the file is shorter than it was
			}
		}
		final := pos >= to
		if skipping {
			i := bytes.IndexByte(buf, '\n')
			switch {
			case i >= 0:
				buf = append(buf[:0], buf[i+1:]...)
				skipping = false
			case final:
				return covered, nil // one long partial line: nothing to send
			default:
				continue // no line break yet: read on
			}
		}
		cut := splitPoint(buf, final)
		if final && dropLast {
			cut = bytes.LastIndexByte(buf, '\n') + 1
		}
		if cut == 0 && !final && len(buf) >= 2*int(part) {
			cut = len(buf) // a block of one endless "line": send it as it is
		}
		if cut > 0 {
			if err := w.add(buf[:cut]); err != nil {
				return covered, err
			}
			covered += int64(cut)
			buf = append(buf[:0], buf[cut:]...)
		}
		if final {
			return covered, nil
		}
	}
}

// splitPoint is where to cut buf into a part. At the end of the log all of it
// goes; before that, after the last line break, so a line (and a token in it)
// is never split. A block with no line break at all (a binary dump) is cut
// after its last character that cannot be part of a token, so even then a
// token is not split.
func splitPoint(buf []byte, final bool) int {
	if final {
		return len(buf)
	}
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		return i + 1
	}
	for i := len(buf) - 1; i > 0; i-- {
		if !tokenByte(buf[i]) {
			return i + 1
		}
	}
	return 0
}

func tokenByte(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.'
}
