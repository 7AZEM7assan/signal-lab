# Signal Lab desktop app

A native window around Signal Lab. Download it, open it, and manage everything from the app: run
replays with fault controls, watch the live feed, browse and export what was stored, change
settings, and clear the data. There is no Docker, no PostgreSQL and no terminal involved; the app
stores everything in one local SQLite file.

## Install

Installers are attached to each [GitHub release](https://github.com/7AZEM7assan/signal-lab/releases)
(built by `.github/workflows/desktop.yml`). They are **not code-signed**, because signing needs
paid developer certificates, so each operating system shows a one-time warning:

| System | Download | First launch |
|---|---|---|
| **macOS** (Apple Silicon or Intel) | `Signal-Lab-<version>-mac-arm64.dmg` / `-x64.dmg` | Drag *Signal Lab* to Applications. macOS says "Signal Lab was blocked to protect your Mac": open **System Settings > Privacy & Security** and click **Open Anyway** (if a dialog offers *Move to Trash*, choose **Done**), or run `xattr -dr com.apple.quarantine "/Applications/Signal Lab.app"` once. |
| **Windows** (x64 or ARM64) | `Signal-Lab-<version>-win-x64.exe` or `-win-arm64.exe` (installer; a combined `Signal-Lab-<version>-win.exe` is attached too), or the `.zip` (no install) | SmartScreen may say "Windows protected your PC": click **More info > Run anyway**. |
| **Linux** (x64) | `Signal-Lab-<version>-linux-x86_64.AppImage` or `.tar.gz` | `chmod +x Signal-Lab-*.AppImage && ./Signal-Lab-*.AppImage` |

The download is about 130-160 MB because it includes the Electron runtime; the Signal Lab engine
itself is an 18 MB program. Tested by the author so far: macOS on Apple silicon (the 0.2.0 download)
and Linux; the Intel Mac and Windows builds are made by CI and not yet run on a real machine.

**Building it yourself is the most reliable route**, and it avoids the warnings above on macOS:

```bash
# needs Go 1.25+ and Node.js 22+
git clone https://github.com/7AZEM7assan/signal-lab && cd signal-lab
make desktop-dist          # installer / app for THIS operating system in desktop/dist/
```

## Your data

| What | Where |
|---|---|
| Events, alerts, settings | the **data folder**: `<app folder>/data/` (see below), one `signallab.db` plus `settings.json` |
| Log | `<app folder>/logs/signal-lab.log` |

`<app folder>` is `~/Library/Application Support/Signal Lab` on macOS, `%APPDATA%\Signal Lab` on
Windows and `~/.config/Signal Lab` on Linux. In the app, **Storage > Open folder** opens it and
**Storage > Change folder...** moves the app to another folder (existing data is not moved or
deleted). To remove everything, uninstall the app and delete that folder. Nothing is sent anywhere,
except a replay that you aim at a service of your own (see below).

## What the app contains

- **Live**: the real-time feed, queue health, latest reading per device and alerts.
- **Replay**: generate simulated readings and send them to the service. Presets: quick demo, fault
  storm, backpressure demo (small queue plus slow worker so the service answers 429), and an exact
  repeat that shows idempotency. Start, stop, and watch progress and results.
  **Your own service and data**: instead of the built-in service, a replay can go to an HTTP address
  you enter (with headers such as an API key, and a payload shape), and it can replay a CSV, NDJSON
  or JSON file you import. The results show responses by status, `429` handling, the first error
  and latency. Readings sent to your service are not stored in the app. See
  [`docs/test-your-service.md`](../docs/test-your-service.md).
- **Data**: filter events and alerts by device and time, page through them, and export everything
  that matches as CSV, NDJSON or JSON.
- **Settings**: alert thresholds (applied immediately), queue size, workers, batch size and an
  artificial worker delay (these rebuild the ingest engine; queued events are saved first).
- **Storage**: what is stored, database size and location, and "Delete all data" behind a typed
  confirmation.

## How it works

```text
Electron window  --loads-->  http://127.0.0.1:<random port>/   (control panel, same-origin)
      |                                    ^
      | spawns, owns the lifetime          | HTTP + WebSocket, token required
      v                                    |
signallab app  (Go engine: ingest pipeline, SQLite, replay runner, control API)
```

- The shell (`main.js`) picks no port itself: it starts `signallab app --addr 127.0.0.1:0`, reads
  the chosen port from the engine's first output line, and opens a window on it.
- On quit it asks the engine to shut down (`POST /app/api/shutdown`), which drains the queue to
  the database, and only kills it if that takes longer than 12 seconds.
- If the engine dies later, the app offers to restart it or open the log.
- If the engine cannot start (for example the data folder is not writable, or the port is in use), the
  app shows an error screen with the reason in plain words, a **Copy details** button (version,
  platform, folders and the engine's own output, with no token), **Open log folder**, **Try again**
  and **Quit**, instead of an empty window.

### Security model

This is a local app, but a program listening on `127.0.0.1` can still be reached by web pages in
your browser, so it is locked down:

- the engine only accepts loopback addresses and refuses `Host` headers it does not own (blocks DNS
  rebinding);
- every request needs a random 256-bit token created at each launch: the window receives it as a
  strict, HTTP-only cookie, and scripts may send it as `X-SignalLab-Token`. Writes made with the
  cookie must also be same-origin and carry `X-Requested-With: signallab`;
- the token is handed to the shell on a pipe (the engine's `--ready-json` line) and is never written to
  the log file: the log shows only the address and port, and the shell redacts anything that looks like
  a token before logging engine output;
- the panel runs with a strict content security policy (no inline or third-party code);
- the Electron window uses context isolation, the sandbox, no Node.js access, no pop-ups, no
  navigation away, no browser permissions, and exposes only two folder actions to the page.

## Develop

```bash
make desktop-dev                       # run from source (builds the Go engine for your machine first)
make desktop-smoke                     # end-to-end test: real app + engine, replay, quit, restart
xvfb-run -a make desktop-smoke         # the same on a headless Linux machine
```

`SMOKE_PACKAGED=/path/to/the/built/executable` runs the same test against a built app. To try the engine
without Electron: `make app` prints a link to open in any browser.

## Signing and notarization (optional, macOS)

By default the macOS app is **ad-hoc signed** (`scripts/adhoc-sign.js`): it runs on Apple Silicon, but a
copy downloaded from the internet still triggers Gatekeeper's first-launch warning. With an Apple
Developer account (paid) you can ship a signed, notarized app that opens without the warning.

The build switches to Developer ID signing and notarization **only when both groups of environment
variables are set**; otherwise nothing changes (`scripts/signing.js` decides, and prints which mode it
chose; a partial set falls back to ad-hoc and names what is missing).

| Group | Variables |
|---|---|
| Certificate (a *Developer ID Application* certificate exported as `.p12`) | `CSC_LINK` (path, URL or base64 of the `.p12`) and `CSC_KEY_PASSWORD`; or `CSC_NAME` to use a certificate already in your keychain |
| Notarization, option A (Apple ID) | `APPLE_ID`, `APPLE_APP_SPECIFIC_PASSWORD` (created at appleid.apple.com), `APPLE_TEAM_ID` |
| Notarization, option B (App Store Connect API key) | `APPLE_API_KEY` (path to the `.p8`), `APPLE_API_KEY_ID`, `APPLE_API_ISSUER` |

Locally:

```bash
export CSC_NAME="Developer ID Application: Your Name (ABCDE12345)"   # or CSC_LINK + CSC_KEY_PASSWORD
export APPLE_ID=you@example.com APPLE_APP_SPECIFIC_PASSWORD=abcd-efgh-ijkl-mnop APPLE_TEAM_ID=ABCDE12345
make desktop-dist
```

In GitHub Actions, add the repository secrets `MAC_CERT_P12_BASE64` (`base64 -i cert.p12 | pbcopy`),
`MAC_CERT_PASSWORD`, `APPLE_ID`, `APPLE_APP_SPECIFIC_PASSWORD` and `APPLE_TEAM_ID`; the macOS job passes
them through (empty when a secret is missing, which keeps the ad-hoc build). Notarization uploads the
app to Apple and waits for the result, which usually takes a few minutes.

Check a signed build with `codesign --verify --deep --strict "Signal Lab.app"`,
`spctl -a -vv "Signal Lab.app"` and `xcrun stapler validate "Signal Lab.app"`.

> Status: the switch itself is unit-tested (`test/config.test.mjs`), but notarization needs an Apple
> account and a Mac, so it has **not been run end to end**. Treat the first signed build as a test.

## Licences

Signal Lab is MIT licensed. The app also contains the Electron runtime and Chromium (their licences are
shipped next to the app) and the Go modules listed in `THIRD_PARTY_NOTICES.txt` (regenerate with
`node scripts/gen-notices.js` after changing Go dependencies).

## Status

- Tested: the Linux x64 build (source tree and packaged), with the smoke test above, and the
  macOS Apple silicon build from the 0.2.0 release page (downloaded in a browser: first-launch
  warning, quick demo, a replay to the toy receiver, a file import, quit).
- Built by the release workflow but **not tested by the author**: the Intel Mac and Windows
  installers. Please open an issue if one does not start.
- Not included: code signing, notarization and automatic updates. Updating means installing the
  newer version over the old one; your data folder is untouched.
