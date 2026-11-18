// Package logupload prepares and sends the game's log to the server: it
// removes what must not leave the PC, cuts the log into line-aligned gzip
// parts on disk (never holding the whole log in memory), and uploads them to
// mmogbrain's chunked endpoint with per-part retries. It has no Windows
// dependency, so its tests run anywhere; the launcher (logupload_windows.go)
// supplies the file, the token and the settings.
package logupload

import (
	"bytes"
	"regexp"
	"strings"
)

var (
	// A JWT: base64url JSON header, always starting "eyJ" ({"). A cut-off one
	// (no dots) is removed as well.
	jwtRE = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{6,}(?:\.[A-Za-z0-9_-]*){0,2}`)
	// UE4's start-up lines naming the machine and the Windows account.
	machineRE = regexp.MustCompile(`(LogInit: (?:Computer|User): )[^\r\n]*`)
	// The Windows user folder in a path, either slash.
	userDirRE = regexp.MustCompile(`(?i)([A-Z]:[\\/]+Users[\\/]+)[^\\/\r\n"'<>|?*]+`)
)

// Redactor removes the sign-in token and the Windows computer and user names
// from game-log text. Every rule works within one line (a token is made of
// characters that never include a line break), so text cut at line breaks can
// be redacted piece by piece.
type Redactor struct {
	// Computer and Profile are this PC's COMPUTERNAME and USERPROFILE,
	// removed wherever they appear ("" skips them).
	Computer, Profile string

	computerRE, profileRE *regexp.Regexp
	computerLow           string
	profiles              []string
}

// NewRedactor is a Redactor for this PC's names.
func NewRedactor(computer, profile string) *Redactor {
	r := &Redactor{Computer: computer, Profile: profile}
	// The computer name as a whole word only, and not a short one: a PC named
	// "GAME" must not blank every "/Game/" path.
	if len(computer) >= 5 {
		r.computerRE = regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(computer) + `\b`)
		r.computerLow = asciiLower(computer)
	}
	// A profile outside C:\Users (D:\Profiles\name), either slash.
	if len(profile) > 3 {
		r.profiles = []string{profile, strings.ReplaceAll(profile, `\`, "/")}
		r.profileRE = regexp.MustCompile(`(?i)` + regexp.QuoteMeta(r.profiles[0]) + `|` + regexp.QuoteMeta(r.profiles[1]))
	}
	return r
}

// Apply returns b with the private parts removed (it may change b).
func (r *Redactor) Apply(b []byte) []byte {
	if bytes.Contains(b, []byte("eyJ")) {
		b = jwtRE.ReplaceAll(b, []byte("<token removed>"))
	}
	if bytes.Contains(b, []byte("LogInit: ")) {
		b = machineRE.ReplaceAll(b, []byte("${1}<removed>"))
	}
	// The case-insensitive expressions have no literal to jump to, so they are
	// slow (about 20 MB/s) and are run only on the lines that hold a candidate
	// (a few percent of a log). Every rule works within one line.
	needles := [][]byte{[]byte("users")}
	if r.profileRE != nil {
		for _, p := range r.profiles {
			needles = append(needles, []byte(asciiLower(p)))
		}
	}
	if r.computerRE != nil {
		needles = append(needles, []byte(r.computerLow))
	}
	always := (r.profileRE != nil && !isASCII(r.Profile)) || (r.computerRE != nil && !isASCII(r.Computer))
	return replaceInLines(b, needles, always, func(line []byte) []byte {
		line = userDirRE.ReplaceAll(line, []byte("${1}<user>"))
		if r.profileRE != nil {
			line = r.profileRE.ReplaceAllLiteral(line, []byte("<user folder>"))
		}
		if r.computerRE != nil {
			line = r.computerRE.ReplaceAllLiteral(line, []byte("<computer>"))
		}
		return line
	})
}

// replaceInLines runs fn on each line of b that contains one of the lower-case
// needles (ASCII case-insensitively), or on every line when all is set, and
// returns the text with those lines replaced.
func replaceInLines(b []byte, needles [][]byte, all bool, fn func(line []byte) []byte) []byte {
	low := []byte(asciiLower(string(b))) // same length as b, byte for byte
	var out []byte
	done := 0 // b[:done] is already in out (or untouched, when out is nil)
	pos := 0
	// found[k] is where needle k occurs next (-1: nowhere in the rest of b),
	// looked up again only once it is behind pos.
	found := make([]int, len(needles))
	for k := range found {
		found[k] = -2
	}
	for pos < len(b) {
		next := -1
		for k, n := range needles {
			if found[k] == -2 || (found[k] >= 0 && found[k] < pos) {
				found[k] = -1
				if i := bytes.Index(low[pos:], n); i >= 0 {
					found[k] = pos + i
				}
			}
			if found[k] >= 0 && (next < 0 || found[k] < next) {
				next = found[k]
			}
		}
		if all {
			next = pos
		}
		if next < 0 {
			break
		}
		start := bytes.LastIndexByte(b[:next], '\n') + 1
		end := len(b)
		if i := bytes.IndexByte(b[next:], '\n'); i >= 0 {
			end = next + i + 1
		}
		if start < done {
			start = done
		}
		line := fn(b[start:end])
		if out == nil {
			out = make([]byte, 0, len(b))
		}
		out = append(out, b[done:start]...)
		out = append(out, line...)
		done = end
		pos = end
	}
	if out == nil {
		return b
	}
	return append(out, b[done:]...)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// asciiLower lower-cases A-Z only, byte for byte (bytes.ToLower would turn
// invalid UTF-8 into U+FFFD and move the offsets).
func asciiLower(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 'A' && s[i] <= 'Z' {
			b := []byte(s)
			for j := i; j < len(b); j++ {
				if b[j] >= 'A' && b[j] <= 'Z' {
					b[j] += 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return s
}
