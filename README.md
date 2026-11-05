# Dreadnought Revival Launcher

The replacement launcher for the community-run **Dreadnought** private server
([Dreadnought-Revival-server](https://github.com/Dreadnought-Revival/Dreadnought-Revival-server)).
The game client itself stays unmodified; this launcher is the only thing a
player installs. It signs in to the server, makes the game trust the server's
certificate, and starts the game pointed at the server.

Moved here from the server repository's `dn-launcher/` folder (with its git
history) on 2026-11-05.

The project is three repositories under
[Dreadnought-Revival](https://github.com/Dreadnought-Revival):
[Dreadnought-Revival-server](https://github.com/Dreadnought-Revival/Dreadnought-Revival-server)
(the backend), this launcher, and
[Dreadnought-Revival-Page](https://github.com/Dreadnought-Revival/Dreadnought-Revival-Page)
(the project website, in progress).

## Releases

Every push to `main` (and a manual run from the Actions tab) runs
`.github/workflows/release.yml`: `go vet` and `go test` on Windows, then a
build that is published as a release, marked latest. Releases are versioned:
`VERSION` holds major.minor (bump it by hand for a bigger change), and the
build number is the patch, so the tags go `v1.0.3`, `v1.0.4`, ...; the title
carries the build date, e.g. **Dreadnought Revival v1.0.3 (2026-11-05)**. The
version is built into the exe (shown in the console banner, the window's
footer, and Windows' Properties > Details). Each release carries:

- `Dreadnought-Revival.exe`, the one launcher for everybody: its servers come
  from the server list (below), so nothing about a server is built in;
- `Dreadnought-Revival-linux.tar.gz`: that exe with the Linux script, icon and
  README.txt;
- `SHA256SUMS.txt`, and in the notes each listed server's certificate
  fingerprint.

(Builds before 2026-11-05 were called `playtest-N` and shipped `dn-launcher.exe`.)

**Launcher updates** (`update_windows.go`, since v1.0.4). At start the window
asks GitHub for this repository's latest release. When it is newer, the top
of the window shows *LAUNCHER v1.0.N AVAILABLE -- UPDATE*. Update downloads
`Dreadnought-Revival.exe` from that release, checks it against the release's
`SHA256SUMS.txt`, puts it in place of the running exe (whatever it is called,
e.g. `DreadnoughtLauncher.exe`; the old one becomes `<exe>.old`, deleted at the
next start), and restarts. If the launcher's folder cannot be written, the
button opens the release page instead. Nothing is downloaded until the player
presses it. A local build (version `dev`) never checks, and
`"skip_update_check": true` in `dn-launcher.json` turns it off. Launchers
before v1.0.4 have no check: update those by hand once.

A change to `serverlist.yaml` alone starts no build: launchers download the
list themselves.

## Servers

The launcher window's **Server** menu (and *Change…* under the sign-in boxes
and in Settings) is a selection list, in this order:

1. the server the launcher was built or configured for, if any (`server` in
   `dn-launcher.json`, or `-X main.defaultServer`, see Build);
2. the servers in [`serverlist.yaml`](serverlist.yaml).

Players do not type in addresses: a server is offered by listing it.

The choice is remembered per Windows user (`settings.json` under
`%LOCALAPPDATA%\DreadnoughtPrivateServer`). With nothing chosen and nothing
built in, the first listed server is used, so a fresh download needs no setup.
Each server keeps its own remembered sign-in, since accounts live on a server.

**The server list.** `serverlist.yaml` names each server's DNS name, its ports
(`web_port` for sign-in and news, `gateway_port` and `firmament_port` for the
game) and its CA certificate; the file itself documents every field. The
launcher downloads it from this repository's `main` branch at every start
(`serverlist.go`; at most 6 s), keeps the last good download, and falls back to
the copy built into the exe. A list that does not parse never replaces a good
one, and a bad entry is left out without hiding the others. To list a
community server, open a pull request adding an entry. A server that makes a
new CA must update its `ca_cert`, or launchers keep refusing its new
certificate. `server_list_url` in `dn-launcher.json` points one install at
another list (`"none"` turns the download off).

**Certificates.** The certificate screen always shows the fingerprint before
anything is installed, and says where the certificate came from:

- a listed server's CA comes from the list, which GitHub's verified HTTPS
  vouches for;
- a server listed without `ca_cert` is asked for its CA (`GET /dn/ca.crt` on
  its gateway; the server repository's `gateway/main.go` serves it). The
  launcher checks that this CA really signed the connection and keeps it as
  `servers\list_<id>.crt`; the player compares the fingerprint with the one
  the operator publishes (trust on first use). Prefer `ca_cert`: a server that
  later makes a new CA then only updates its entry;
- the built-in server's CA is `ca.crt` beside the exe or the built-in one, as
  before.

**Removing unused certificates** (`certs_windows.go`). The launcher records
each server certificate it installed or found trusted, and when it last
reached that server with it (news loaded or the game started). At start, a
certificate unused for **30 days** is removed from the Trusted Root store
again, except the one of the server in use. Windows asks the player to
confirm; a refusal is asked again after 7 days. Settings lists the recorded
certificates with a *Remove* button. This only runs while the launcher does: a
player who stops playing keeps the certificate until they remove it
(`certmgr.msc`) or run `Dreadnought-Revival.exe --remove-certs`, which removes every
recorded one (meant for an uninstaller). Only certificates in that record are
ever removed.

## Build

Windows (cross-compiled from any OS):

```bash
GOOS=windows GOARCH=amd64 go build -o Dreadnought-Revival.exe .
```

Settings can be built into a distributed launcher, so testers need no
`dn-launcher.json` at all. `ca.crt` is the server's `certs/ca.crt`:

```bash
GOOS=windows go build -ldflags "-H windowsgui \
  -X main.defaultServer=play.example.org -X main.defaultWebPort=8443 \
  -X main.defaultCA=$(base64 -w0 /path/to/server/certs/ca.crt)" -o Dreadnought-Revival.exe .
```

`-H windowsgui` opens the desktop window with no console flashing up (the
console flow opens its own console when it needs one). `defaultCA` builds the
server's CA into the exe, so testers download **only `Dreadnought-Revival.exe`**. On
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
`Dreadnought-Revival.exe`, and ship the server's `ca.crt` in the same folder (unless it
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

Run `Dreadnought-Revival.exe`. It opens a **desktop window** (Microsoft Edge WebView2,
part of Windows 11 and installed with Edge on most Windows 10 PCs) with
**Sign in** / **Create account**, then a home screen with the server's news
tiles (the server's `/v2/dreadnought/launcher/dn/tiles/`), a server status
light and **Play**. The sign-in is kept, DPAPI-protected for that Windows user,
so the next start goes straight to the home screen. Because the account lives
on the server rather than being derived from the machine, the same login works
from any PC.

The window is laid out like the original Dreadnought launcher: borderless
1105x620, the logo and a menu (Server, Settings, Game logs, Sign out) top left, PLAY
GAME top right, the news carousel beside a news panel, and sign-in / create
account as two boxes side by side. Its background, logo and fonts are the
original ones, read at start from `DreadnoughtLauncher.exe` in the player's own
game folder (`origassets_windows.go`; the file is a Solid State Networks skin
archive the code decrypts with a password it finds in that same exe). Nothing
of the original launcher is copied into this repository or into
`Dreadnought-Revival.exe`; without that file the window keeps the same layout in plain
colours.

Display scaling: the exe is per-monitor DPI aware, so the window is 1105x620
times the display's scale (e.g. 2486x1395 pixels at 225%), shrunk to fit above
the taskbar when that is too tall, and the page scales its layout to the window
(sharp text, nothing cut off). Moving it to a monitor with another scale
resizes it for that monitor.

Without WebView2 (detected before anything is created), or with `--console`,
it falls back to the older flow: a console window and the same sign-in page in
the default browser. `Dreadnought-Revival.exe --sign-out` clears the stored
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
(`browser_windows.go`). Ship the script with `Dreadnought-Revival.exe` and `icon.png`
beside it; `--install` adds a menu entry.

## The rule for this repository

The launcher is the one client-side piece of the project, and it is meant to
stay small: sign in, trust the server's certificate, point the game at the
server. Anything beyond that needs a real justification. The game executable
itself is never patched.

## Credits

- **Legojedi** — the launcher icon.
- **starveil-bunny** — the server picker (PR #81 in the old repository).

## License

Apache License 2.0, see [LICENSE](LICENSE).
