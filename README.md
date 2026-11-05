# Dreadnought Revival Launcher

The replacement launcher for the community-run **Dreadnought** private server
([Dreadnought-Revival-server](https://github.com/Dreadnought-Revival/Dreadnought-Revival-server)).
The game client itself stays unmodified; this launcher is the only thing a
player installs. It signs in to the server, makes the game trust the server's
certificate, and starts the game pointed at the server.

Moved here from the server repository's `dn-launcher/` folder (with its git
history) on 2026-11-05.

## Build

Windows (cross-compiled from any OS):

```bash
GOOS=windows GOARCH=amd64 go build -o dn-launcher.exe .
```

Settings can be built into a distributed launcher, so testers need no
`dn-launcher.json` at all. `ca.crt` is the server's `certs/ca.crt`:

```bash
GOOS=windows go build -ldflags "-H windowsgui \
  -X main.defaultServer=play.example.org -X main.defaultWebPort=8443 \
  -X main.defaultCA=$(base64 -w0 /path/to/server/certs/ca.crt)" -o dn-launcher.exe .
```

`-H windowsgui` opens the desktop window with no console flashing up (the
console flow opens its own console when it needs one). `defaultCA` builds the
server's CA into the exe, so testers download **only `dn-launcher.exe`**. On
first start the window shows an **Install the public testing certificate**
screen with the CA's SHA-256 fingerprint (publish it so testers can compare,
`openssl x509 -in certs/ca.crt -noout -fingerprint -sha256`); Play stays
blocked until it is installed. A `ca.crt` beside the exe overrides the built-in
one. Rebuild after the server regenerates its CA.

The exe's icon, version information and manifest (runs without admin rights)
come from `rsrc_windows_amd64.syso`, which `go build` links in automatically.
It is generated from `winres/` (`winres.json` and `icon.png`); after changing
either, regenerate it:
`go run github.com/tc-hib/go-winres@v0.3.3 make --in winres/winres.json --arch amd64`.

Tests: `go test ./...` (most tests are Windows-only; run them with
`GOOS=windows go test -c` or on Windows).

## Configure

Put the server's public hostname or IP in `dn-launcher.json` beside
`dn-launcher.exe`, and ship the server's `ca.crt` in the same folder (unless it
is built in, see above):

```json
{ "server": "play.example.org" }
```

The launcher then:

- looks the name up when it starts, for the news, and again right before it
  starts the game, and gives the game the answer. A home server whose outside
  IP changes only needs its DNS record to follow (a dynamic-DNS name). When
  Windows' own lookup fails it asks 1.1.1.1, 8.8.8.8 and 9.9.9.9 directly. With
  a `server` setting, `gateway_ip` and `firmament_host` from an older
  `dn-launcher.json` are ignored;
- passes the server's address to the game (`-GatewayAddress`,
  `-YFirmamentAddress`). The game resolves no backend by name, so it needs no
  DNS or hosts-file change;
- sends its own sign-in straight to that address while keeping the original
  host name, so the server's gateway still routes it, and verifies the server
  certificate against the shipped `ca.crt`;
- installs `ca.crt` into the **current user's** Trusted Root store, once. No
  admin; Windows shows its own confirmation. The game needs it: it verifies the
  gateway and Firmament certificates.

If 443 is already taken on the server's router, map another outside port to
the server's 443 and add `"web_port": "8443"` (only the launcher's sign-in and
news use 443; the game's own ports are unaffected).

## Use

Run `dn-launcher.exe`. It opens a **desktop window** (Microsoft Edge WebView2,
part of Windows 11 and installed with Edge on most Windows 10 PCs) with
**Sign in** / **Create account**, then a home screen with the server's news
tiles (the server's `/v2/dreadnought/launcher/dn/tiles/`), a server status
light and **Play**. The sign-in is kept, DPAPI-protected for that Windows user,
so the next start goes straight to the home screen. Because the account lives
on the server rather than being derived from the machine, the same login works
from any PC.

The window is laid out like the original Dreadnought launcher: borderless
1105x620, the logo and a menu (Settings, Game logs, Sign out) top left, PLAY
GAME top right, the news carousel beside a news panel, and sign-in / create
account as two boxes side by side. Its background, logo and fonts are the
original ones, read at start from `DreadnoughtLauncher.exe` in the player's own
game folder (`origassets_windows.go`; the file is a Solid State Networks skin
archive the code decrypts with a password it finds in that same exe). Nothing
of the original launcher is copied into this repository or into
`dn-launcher.exe`; without that file the window keeps the same layout in plain
colours.

Display scaling: the exe is per-monitor DPI aware, so the window is 1105x620
times the display's scale (e.g. 2486x1395 pixels at 225%), shrunk to fit above
the taskbar when that is too tall, and the page scales its layout to the window
(sharp text, nothing cut off). Moving it to a monitor with another scale
resizes it for that monitor.

Without WebView2 (detected before anything is created), or with `--console`,
it falls back to the older flow: a console window and the same sign-in page in
the default browser. `dn-launcher.exe --sign-out` clears the stored
credentials.

## Linux

The game runs through Proton or Wine, and the certificate, the sign-in token
and the game's arguments must be set up inside that same Wine/Proton prefix.
`linux/dn-launcher-linux.sh` therefore runs the Windows launcher there: it
finds the game in the Steam libraries (or `DN_GAME_DIR`), uses the game's
Proton prefix when Steam created one (else Wine with
`~/.local/share/dreadnought-wine`), and passes the game path as
`DN_GAME_PATH`. WebView2 is not available under Wine, so the launcher opens in
the Linux browser instead: the same page as the desktop window (sign-in, the
certificate, the game folder, options, Play), served on a random loopback port
(`browser_windows.go`). Ship the script with `dn-launcher.exe` and `icon.png`
beside it; `--install` adds a menu entry.

## The rule for this repository

The launcher is the one client-side piece of the project, and it is meant to
stay small: sign in, trust the server's certificate, point the game at the
server. Anything beyond that needs a real justification. The game executable
itself is never patched.

## Credits

- **Legojedi** — the launcher icon.

## License

Apache License 2.0, see [LICENSE](LICENSE).
