//go:build windows

package main

// Game log upload. When the game closes, the launcher sends the session's
// DreadGame.log to the server (mmogbrain POST /launcher/log, through the
// gateway like /launcher/clock), so a bug report no longer depends on the
// player finding the file. On by default; "Send my game log" in the settings
// turns it off (settings.json no_log_upload).
//
// Before anything leaves the PC the log loses (redactGameLog):
//   - the sign-in token: the client prints its own JWT
//     ("LogWebServicesPlugin: Authorization: Bearer eyJ...");
//   - the Windows computer and user names ("LogInit: Computer:",
//     "LogInit: User:", C:\Users\<name>\ in paths, and this PC's
//     COMPUTERNAME and profile folder wherever they appear).
//
// The session id and account name the client prints stay in: the server
// knows both already. The server runs the same filter again.
//
// A log that could not be sent when the game closed (launcher closed first,
// "close the launcher when the game starts", server unreachable) is sent on
// the next Play, BEFORE the game starts: starting the game renames
// DreadGame.log to a backup and begins a new one.

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// logUploadURL is mmogbrain's /launcher/log, on the same host as clockURL.
const logUploadURL = "https://mmog.greybox.sixfoot.live/launcher/log"

const (
	// logUploadMaxRaw is the most of a log that is sent; a longer one (a long
	// session with the detailed log) keeps its start and its end. The server
	// takes up to 48 MB.
	logUploadMaxRaw  = 40 << 20
	logUploadKeepTop = 4 << 20
	// logUploadMaxAge: a pending log older than this is not sent any more.
	logUploadMaxAge = 7 * 24 * time.Hour
)

var (
	gameLogJWT     = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}(?:\.[A-Za-z0-9_-]*){0,2}`)
	gameLogMachine = regexp.MustCompile(`(LogInit: (?:Computer|User): )[^\r\n]*`)
	gameLogUserDir = regexp.MustCompile(`(?i)([A-Z]:[\\/]+Users[\\/]+)[^\\/\r\n"'<>|?*]+`)
)

// redactGameLog removes the sign-in token and the Windows computer and user
// names. computer and profile are this PC's COMPUTERNAME and USERPROFILE,
// removed wherever they appear ("" skips them).
func redactGameLog(b []byte, computer, profile string) []byte {
	b = gameLogJWT.ReplaceAll(b, []byte("<token removed>"))
	b = gameLogMachine.ReplaceAll(b, []byte("${1}<removed>"))
	b = gameLogUserDir.ReplaceAll(b, []byte("${1}<user>"))
	// A profile outside C:\Users (D:\Profiles\name), either slash.
	if len(profile) > 3 {
		for _, p := range []string{profile, strings.ReplaceAll(profile, `\`, "/")} {
			b = replaceFold(b, regexp.QuoteMeta(p), "<user folder>")
		}
	}
	// The computer name as a whole word only, and not a short one: a PC named
	// "GAME" must not blank every "/Game/" path.
	if len(computer) >= 5 {
		b = replaceFold(b, `\b`+regexp.QuoteMeta(computer)+`\b`, "<computer>")
	}
	return b
}

// replaceFold replaces every case-insensitive match of the pattern pat.
func replaceFold(b []byte, pat, repl string) []byte {
	re, err := regexp.Compile(`(?i)` + pat)
	if err != nil {
		return b
	}
	return re.ReplaceAllLiteral(b, []byte(repl))
}

// trimGameLog keeps a too-long log's first logUploadKeepTop bytes and its
// end, with a line saying how much was cut.
func trimGameLog(b []byte) []byte {
	if len(b) <= logUploadMaxRaw {
		return b
	}
	tail := logUploadMaxRaw - logUploadKeepTop
	return joinCut(b[:logUploadKeepTop], b[len(b)-tail:], int64(len(b)-logUploadKeepTop-tail))
}

// readGameLog reads the log, but of one longer than logUploadMaxRaw only its
// first logUploadKeepTop bytes and its end (trimGameLog's cut), without
// reading the middle: a detailed log can run to hundreds of MB (audit
// 2026-11-17: the whole file was read first).
func readGameLog(path string) ([]byte, error) {
	//nolint:gosec // the game's own log, in a folder found by gameLogFolders.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size <= logUploadMaxRaw {
		return io.ReadAll(io.LimitReader(f, logUploadMaxRaw))
	}
	tail := int64(logUploadMaxRaw - logUploadKeepTop)
	head := make([]byte, logUploadKeepTop)
	if _, err := f.ReadAt(head, 0); err != nil {
		return nil, err
	}
	end := make([]byte, tail)
	if _, err := f.ReadAt(end, size-tail); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return joinCut(head, end, size-logUploadKeepTop-tail), nil
}

func joinCut(head, end []byte, cut int64) []byte {
	out := make([]byte, 0, len(head)+len(end)+64)
	out = append(out, head...)
	out = append(out, fmt.Sprintf("\r\n[launcher: %d bytes of the log cut here]\r\n", cut)...)
	return append(out, end...)
}

// gameLogClosedCleanly reports whether the game log at path ends with the
// engine's own last line, "Log file closed": the game shut down completely,
// whatever its exit code says. The client exits with 0xC0000005 AFTER that
// line on every quit (a background thread returns to address 0 from
// NtWaitForMultipleObjects, 2026-11-17), which is no crash a player saw.
func gameLogClosedCleanly(path string) bool {
	//nolint:gosec // the game's own log.
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return false
	}
	const tailBytes = 4096
	off := info.Size() - tailBytes
	if off < 0 {
		off = 0
	}
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	return bytes.Contains(buf, []byte("Log file closed"))
}

// gameSession is what the launcher knows of the session a log is from.
// late: sent at the next Play, so exit code and length are unknown.
type gameSession struct {
	exitCode int
	crashed  bool
	seconds  int
	late     bool
}

// The server tells a crash on exit (crashed=0 with a non-zero exit code) from
// a crash during play (crashed=1).

// pendingLog is a log read, filtered and compressed, ready to send.
type pendingLog struct {
	gz       []byte
	modified int64
	query    url.Values
}

var logUploadMu sync.Mutex

// prepareGameLog reads the newest DreadGame.log, unless it was sent already,
// uploads are off, or there is none. nil when there is nothing to send.
func (a *launcherAPI) prepareGameLog(s gameSession) *pendingLog {
	settings := loadSettings()
	if settings.NoLogUpload {
		return nil
	}
	dir := a.newestLogFolder()
	if dir == "" {
		return nil
	}
	path := filepath.Join(dir, "DreadGame.log")
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return nil
	}
	modified := info.ModTime().Unix()
	if modified == settings.LogUploaded || (s.late && time.Since(info.ModTime()) > logUploadMaxAge) {
		return nil
	}
	raw, err := readGameLog(path)
	if err != nil {
		fmt.Printf("[!] Could not read the game log: %v\n", err)
		return nil
	}
	raw = redactGameLog(raw, os.Getenv("COMPUTERNAME"), os.Getenv("USERPROFILE"))
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write(raw)
	if zw.Close() != nil {
		return nil
	}
	q := url.Values{}
	q.Set("modified", strconv.FormatInt(modified, 10))
	q.Set("exit", strconv.Itoa(s.exitCode))
	q.Set("seconds", strconv.Itoa(s.seconds))
	q.Set("version", launcherVersion)
	if s.crashed {
		q.Set("crashed", "1")
	}
	if s.late {
		q.Set("late", "1")
	}
	return &pendingLog{gz: buf.Bytes(), modified: modified, query: q}
}

// send uploads the log and, once the server has it, remembers it as sent.
func (p *pendingLog) send(token string) error {
	if token == "" {
		return errors.New("not signed in")
	}
	logUploadMu.Lock()
	defer logUploadMu.Unlock()
	if loadSettings().LogUploaded == p.modified {
		return nil
	}
	req, err := http.NewRequest(http.MethodPost, logUploadURL+"?"+p.query.Encode(), bytes.NewReader(p.gz))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/gzip")
	req.Header.Set("Authorization", "Bearer "+token)
	client := launcherHTTPClient()
	client.Timeout = 60 * time.Second
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusRequestEntityTooLarge, http.StatusBadRequest:
		// Never accepted, so not tried again.
		markGameLogSent(p.modified)
		return fmt.Errorf("refused (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	markGameLogSent(p.modified)
	fmt.Printf("[+] Game log sent (%d KB compressed).\n", len(p.gz)>>10)
	return nil
}

func markGameLogSent(modified int64) {
	_ = updateSettings(func(s *launcherSettings) bool { s.LogUploaded = modified; return true })
}

// uploadGameLogAfterGame sends the log of the session that just ended. Runs
// in the background; a failure leaves it for the next Play.
func (a *launcherAPI) uploadGameLogAfterGame(s gameSession) {
	p := a.prepareGameLog(s)
	if p == nil {
		return
	}
	a.mu.Lock()
	token := a.token
	a.mu.Unlock()
	if token == "" || launcherTokenExpired(token) {
		var err error
		if token, _, err = a.renew(); err != nil {
			fmt.Printf("[!] Game log not sent (sign-in expired); it is sent at the next Play.\n")
			return
		}
	}
	if err := p.send(token); err != nil {
		fmt.Printf("[!] Game log not sent: %v; it is sent at the next Play.\n", err)
	}
}
