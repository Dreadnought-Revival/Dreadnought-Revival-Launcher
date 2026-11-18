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
// The WHOLE log is sent (up to logUploadMaxRaw; the middle of a longer one is
// left out, marked). It is never held in memory: package logupload reads the
// file in blocks, redacts them at line breaks, and writes gzip parts of about
// 4 MiB to a spool folder; they are uploaded one by one (POST /launcher/log/part,
// then /launcher/log/commit) with a retry per part, so a flaky connection
// costs one part, and each request is small enough for the servers' 15-30 s
// request time. A server without that endpoint (older than this launcher) gets
// the log the old way, in one POST cut to 40 MB.
//
// Why a spool folder: the game replaces DreadGame.log when it starts, and a log
// that was not sent when the game closed is read before the next Play starts
// it. The parts hold the redacted copy until the upload is over.
//
// The game itself used to keep only the first 256 KiB and the last 1.75 MiB of
// its log, in memory, and write them out in ring order when it shut down
// (its default log device; see gameLogArgs in main.go). The launcher starts it
// with -NOLOGTOMEMORY, so the file is the whole, ordered log, written as it
// goes.
//
// A log that could not be sent when the game closed (launcher closed first,
// "close the launcher when the game starts", server unreachable) is sent on
// the next Play, BEFORE the game starts: starting the game renames
// DreadGame.log to a backup and begins a new one.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/logupload"
)

// logUploadURL is mmogbrain's /launcher/log, on the same host as clockURL; the
// chunked upload uses /part and /commit under it.
const logUploadURL = "https://mmog.greybox.sixfoot.live/launcher/log"

const (
	// logUploadMaxRaw is the most of a log that is sent; a longer one keeps its
	// start (logUploadKeepTop) and its end. The server takes up to 512 MiB.
	logUploadMaxRaw  = logupload.DefaultMaxRaw
	logUploadKeepTop = logupload.DefaultKeepTop
	// logUploadLegacyRaw is the cut for a server that only has the one-POST
	// upload (it takes 48 MB of text, 8 MB compressed).
	logUploadLegacyRaw = 40 << 20
	// logUploadMaxAge: a pending log older than this is not sent any more.
	logUploadMaxAge = 7 * 24 * time.Hour
	// logSpoolMaxAge: a spool folder left behind (the launcher was closed
	// during an upload) is removed after this.
	logSpoolMaxAge = 2 * 24 * time.Hour
)

// gameLogArgs are the switches that make the game write its whole log to
// DreadGame.log as it goes.
//
// Without them the client's default log device is an in-memory one: the first
// 256 KiB of the log plus a ring of the last 1.75 MiB (2 MiB in all), written
// to Saved\Logs\DreadGame.log only when the game shuts down, in buffer order
// (the start, then the newest part of the ring, then the oldest, so even the
// part that survives is not in time order). Evidence, 2026-11-18:
//   - 15 of 51 uploaded logs were 2,09x,xxx bytes (2 MiB minus redaction), the
//     first 262,144 bytes followed by a ring whose time stamps jump back once;
//   - the exe's log-device factory (RVA 0xBC7230) builds that device with the
//     arguments (0x40000, 0x200000) unless the command line has NOLOGTOMEMORY,
//     and then builds the ordinary file device (a UE4 FOutputDeviceFile with
//     an async writer);
//   - a client started under Wine with -NOLOGTOMEMORY wrote DreadGame.log from
//     the first seconds (180 KB after 25 s, one line per log line, "Log file
//     open" first); without the switch the same run wrote no file at all.
//
// -LOGTIMES keeps the absolute UTC time stamps ([2026.11.18-03.45.08:793]) the
// memory device printed; the file device prints seconds since start without it.
//
// DN_LOG_TO_MEMORY=1 starts the game the old way (a way back without a release).
func gameLogArgs() []string {
	if os.Getenv("DN_LOG_TO_MEMORY") == "1" {
		return nil
	}
	return []string{"-NOLOGTOMEMORY", "-LOGTIMES"}
}

// redactGameLog removes the sign-in token and the Windows computer and user
// names. computer and profile are this PC's COMPUTERNAME and USERPROFILE,
// removed wherever they appear ("" skips them).
func redactGameLog(b []byte, computer, profile string) []byte {
	return logupload.NewRedactor(computer, profile).Apply(b)
}

// logSpoolRoot is the folder the parts of logs being sent are kept in.
func logSpoolRoot() string { return filepath.Join(os.TempDir(), "DreadnoughtRevival-log-upload") }

// sweepLogSpool removes spool folders that nothing is using any more.
func sweepLogSpool() {
	entries, _ := os.ReadDir(logSpoolRoot())
	for _, e := range entries {
		if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > logSpoolMaxAge {
			_ = os.RemoveAll(filepath.Join(logSpoolRoot(), e.Name()))
		}
	}
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

// pendingLog is a log read, filtered and compressed into parts, ready to send.
type pendingLog struct {
	parts    *logupload.Manifest
	modified int64
	query    url.Values
}

var logUploadMu sync.Mutex

// prepareGameLog reads the newest DreadGame.log into a spool folder, unless it
// was sent already, uploads are off, or there is none. nil when there is
// nothing to send.
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
	sweepLogSpool()
	if err := os.MkdirAll(logSpoolRoot(), 0o700); err != nil {
		fmt.Printf("[!] Could not prepare the game log: %v\n", err)
		return nil
	}
	spool, err := os.MkdirTemp(logSpoolRoot(), "log-")
	if err != nil {
		fmt.Printf("[!] Could not prepare the game log: %v\n", err)
		return nil
	}
	red := logupload.NewRedactor(os.Getenv("COMPUTERNAME"), os.Getenv("USERPROFILE"))
	parts, err := logupload.Spool(path, spool, red, logupload.Options{MaxRaw: logUploadMaxRaw, KeepTop: logUploadKeepTop})
	if err != nil {
		_ = os.RemoveAll(spool)
		fmt.Printf("[!] Could not read the game log: %v\n", err)
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
	return &pendingLog{parts: parts, modified: modified, query: q}
}

// send uploads the log and, once the server has it, remembers it as sent. The
// spool folder is removed either way: a failed upload is made again, from the
// log file, at the next Play.
func (p *pendingLog) send(token string) error {
	defer p.parts.Remove()
	if token == "" {
		return errors.New("not signed in")
	}
	logUploadMu.Lock()
	defer logUploadMu.Unlock()
	if loadSettings().LogUploaded == p.modified {
		return nil
	}
	client := launcherHTTPClient()
	client.Timeout = 90 * time.Second // per request: a part is a few MB at most
	u := &logupload.Uploader{Client: client, Base: logUploadURL, Token: token}
	err := u.Upload(p.parts, logupload.NewUploadID(), p.query)
	if errors.Is(err, logupload.ErrNotSupported) {
		// A server from before the chunked upload: the old single POST.
		var body []byte
		if body, err = logupload.Legacy(p.parts, logUploadLegacyRaw); err == nil {
			err = u.SendLegacy(logUploadURL+"?"+p.query.Encode(), body)
		}
	}
	var refused *logupload.RefusedError
	switch {
	case err == nil:
	case errors.As(err, &refused):
		// Never accepted, so not tried again.
		markGameLogSent(p.modified)
		return err
	default:
		return err
	}
	markGameLogSent(p.modified)
	fmt.Printf("[+] Game log sent (%d MB of text, %d KB compressed, %d parts).\n", p.parts.Raw()>>20, p.parts.Stored()>>10, len(p.parts.Parts))
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
