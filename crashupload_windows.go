//go:build windows

package main

// Crash report upload. When the game crashes, it queues a Windows Error
// Report with a minidump (verified in the exe; package crashupload has the
// details) for a crash server that is gone. The launcher sends that report to
// our server instead (mmogbrain POST /launcher/crash, chunked like the game
// log), so a crash can be opened in a debugger rather than guessed from the
// log. The same "Send my game log and crash reports" setting turns it off.
//
// Before anything leaves the PC every file is scrubbed in place, byte for
// byte (crashupload.Scrub): the sign-in token, the Windows computer and user
// names, the profile folder. The server scrubs again.
//
// Which reports, and when:
//   - after a crash during play: every new report written since the game
//     started (at most crashPerSession);
//   - after the crash on exit every quit ends with (gameLogClosedCleanly):
//     one report, at most once every crashOnExitEvery -- one dump is enough to
//     find that bug, and each quit would send another;
//   - at the next Play, reports from the last crashUploadMaxAge not sent yet
//     (the launcher was closed, the server unreachable): one, under the same
//     weekly limit, since most are the crash on exit.
// A report is sent once: its folder name is remembered (settings.json
// crashes_sent), and the server stores a report once by that name anyway.

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/crashupload"
	"github.com/Dreadnought-Revival/Dreadnought-Revival-Launcher/logupload"
)

// crashUploadURL is mmogbrain's /launcher/crash (parts under /part, /commit).
const crashUploadURL = "https://mmog.greybox.sixfoot.live/launcher/crash"

const (
	crashUploadMaxAge = 7 * 24 * time.Hour
	crashOnExitEvery  = 7 * 24 * time.Hour
	crashPerSession   = 3
	// crashSentKeep: how many sent report names settings.json remembers.
	crashSentKeep = 64
	// crashSettle: Windows finishes writing a queued report after the game's
	// process is gone; wait this long before looking.
	crashSettle = 5 * time.Second
)

// crashRoots are the folders crash reports may be in: Windows' report
// folders (only the game's reports), and the game's own Saved folders (the
// places later UE4 versions use; this build does not, see newestCrashFolder).
func (a *launcherAPI) crashRoots() []crashupload.Root {
	var roots []crashupload.Root
	for _, env := range []string{"LOCALAPPDATA", "ProgramData"} {
		base := os.Getenv(env)
		if base == "" {
			continue
		}
		for _, sub := range []string{"ReportQueue", "ReportArchive"} {
			roots = append(roots, crashupload.Root{Dir: filepath.Join(base, "Microsoft", "Windows", "WER", sub), Match: crashupload.GameReportName})
		}
	}
	for _, saved := range a.gameSavedFolders() {
		roots = append(roots, crashupload.Root{Dir: filepath.Join(saved, "Crashes")},
			crashupload.Root{Dir: filepath.Join(saved, "Logs"), Match: func(n string) bool { return strings.HasPrefix(strings.ToUpper(n), "UE4CC") }})
	}
	return roots
}

// crashNeedles are this PC's own private strings, removed from every file
// besides what Scrub's rules find.
func crashNeedles(token string) []string {
	var out []string
	add := func(s string, min int) {
		if s = strings.TrimSpace(s); len(s) >= min {
			out = append(out, s)
		}
	}
	add(os.Getenv("COMPUTERNAME"), 3)
	add(os.Getenv("USERNAME"), 3)
	add(os.Getenv("USERDOMAIN"), 3)
	add(os.Getenv("USERPROFILE"), 4)
	add(token, 16)
	for _, piece := range strings.Split(token, ".") {
		add(piece, 16)
	}
	return out
}

func markCrashSent(names ...string) {
	_ = updateSettings(func(s *launcherSettings) bool {
		for _, n := range names {
			s.CrashesSent = append(s.CrashesSent, n)
		}
		if len(s.CrashesSent) > crashSentKeep {
			s.CrashesSent = s.CrashesSent[len(s.CrashesSent)-crashSentKeep:]
		}
		return true
	})
}

// uploadCrashReportsAfterGame sends the reports of the session that just
// ended. Runs in the background.
func (a *launcherAPI) uploadCrashReportsAfterGame(s gameSession, started time.Time, onExit bool) {
	time.Sleep(crashSettle)
	a.uploadCrashReports(s, started, onExit)
}

// uploadCrashReports sends the reports written since `since` that were not
// sent yet (see the file comment for which).
func (a *launcherAPI) uploadCrashReports(s gameSession, since time.Time, onExit bool) {
	settings := loadSettings()
	if settings.NoLogUpload {
		return
	}
	sent := map[string]bool{}
	for _, n := range settings.CrashesSent {
		sent[n] = true
	}
	var todo []crashupload.Report
	for _, r := range crashupload.Find(a.crashRoots(), since) {
		if !sent[r.Name] {
			todo = append(todo, r)
		}
	}
	if len(todo) == 0 {
		return
	}
	limit, weekly := crashPerSession, onExit || s.late
	if weekly {
		if time.Since(time.Unix(settings.ExitCrashSent, 0)) < crashOnExitEvery {
			var names []string
			for _, r := range todo {
				names = append(names, r.Name)
			}
			markCrashSent(names...)
			fmt.Printf("[*] %d crash report(s) not sent: one was sent in the last 7 days.\n", len(todo))
			return
		}
		limit = 1
	}
	a.mu.Lock()
	token := a.token
	a.mu.Unlock()
	if token == "" || launcherTokenExpired(token) {
		var err error
		if token, _, err = a.renew(); err != nil {
			fmt.Printf("[!] Crash report not sent (sign-in expired); it is sent at the next Play.\n")
			return
		}
	}
	for i, r := range todo {
		if i >= limit {
			markCrashSent(r.Name)
			continue
		}
		err := sendCrashReport(r, token, s, onExit)
		var refused *logupload.RefusedError
		switch {
		case err == nil:
			markCrashSent(r.Name)
			if weekly {
				_ = updateSettings(func(st *launcherSettings) bool { st.ExitCrashSent = time.Now().Unix(); return true })
			}
		case errors.As(err, &refused), errors.Is(err, logupload.ErrNotSupported), errors.Is(err, errNothingToSend):
			markCrashSent(r.Name) // never accepted: not tried again
			fmt.Printf("[!] Crash report %s not sent: %v\n", r.Name, err)
		default:
			fmt.Printf("[!] Crash report not sent: %v; it is sent at the next Play.\n", err)
		}
	}
}

var errNothingToSend = errors.New("nothing to send")

// sendCrashReport packs, scrubs and uploads one report.
func sendCrashReport(r crashupload.Report, token string, s gameSession, onExit bool) error {
	if err := os.MkdirAll(logSpoolRoot(), 0o700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(logSpoolRoot(), "crash-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	p, err := crashupload.Pack(r, filepath.Join(dir, "report.zip"), crashNeedles(token))
	if err != nil {
		return fmt.Errorf("%w: %v", errNothingToSend, err)
	}
	parts := filepath.Join(dir, "parts")
	if err := os.Mkdir(parts, 0o700); err != nil {
		return err
	}
	m, err := crashupload.Split(p.Zip, parts)
	if err != nil {
		return err
	}
	name := crashupload.ReportName(r.Name)
	q := url.Values{}
	q.Set("report", name)
	q.Set("modified", strconv.FormatInt(r.Modified.Unix(), 10))
	q.Set("exit", strconv.Itoa(s.exitCode))
	q.Set("seconds", strconv.Itoa(s.seconds))
	q.Set("version", launcherVersion)
	if onExit {
		q.Set("on_exit", "1")
	}
	if s.late {
		q.Set("late", "1")
	}
	client := launcherHTTPClient()
	client.Timeout = 90 * time.Second // per request: a part is 4 MB at most
	u := &logupload.Uploader{Client: client, Base: crashUploadURL, Token: token, PartQuery: url.Values{"report": {name}}}
	if err := u.Upload(m, logupload.NewUploadID(), q); err != nil {
		return err
	}
	fmt.Printf("[+] Crash report sent (%d files, %d MB, %d KB zipped).\n", p.Files, p.Bytes>>20, m.Source>>10)
	for _, sk := range p.Skipped {
		fmt.Printf("    left out: %s\n", sk)
	}
	return nil
}
