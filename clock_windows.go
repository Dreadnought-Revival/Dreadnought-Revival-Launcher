//go:build windows

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// clockURL is mmogbrain's /launcher/clock, through the gateway like every
// other launcher call. The server stamps its automatic chat lines (players
// online, "a player is searching for a match") with each player's own
// local time; the game never tells it the player's time zone, so the
// launcher does, on Play.
const clockURL = "https://mmog.greybox.sixfoot.live/launcher/clock"

// localClock is this PC's UTC offset in minutes and its short-time format
// (Control Panel > Region: "HH:mm", "h:mm tt", ...) with the AM/PM words.
func localClock() (offset int, pattern, am, pm string) {
	_, secs := time.Now().Zone()
	return secs / 60, localeInfo(0x79 /* LOCALE_SSHORTTIME */), localeInfo(0x28 /* LOCALE_S1159 */),
		localeInfo(0x29 /* LOCALE_S2359 */)
}

var procGetLocaleInfoEx = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetLocaleInfoEx")

// localeInfo reads one value of the user's locale ("" if it cannot).
func localeInfo(lctype uint32) string {
	buf := make([]uint16, 64)
	// A nil locale name is LOCALE_NAME_USER_DEFAULT.
	n, _, _ := procGetLocaleInfoEx.Call(0, uintptr(lctype), uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}

// reportClock sends the clock to the server. Best effort: without it the
// server shows UTC times, so a failure is only printed.
func reportClock(client *http.Client, url, token string) error {
	offset, pattern, am, pm := localClock()
	body, err := json.Marshal(map[string]any{"utc_offset": offset, "pattern": pattern, "am": am, "pm": pm})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}
