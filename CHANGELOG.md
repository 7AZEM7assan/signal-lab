# Changelog

All notable changes to Signal Lab. Installers for every version are on the
[releases page](https://github.com/7AZEM7assan/signal-lab/releases).

## 0.2.3 - 2026-10-10

- **Check your data before you send it.** An imported file is checked as a whole and the app says in plain words what a replay would run into: rows Signal Lab would reject (with row numbers and why), repeated rows, rows out of time order, long silences, stuck sensors and the range of the values, with a table per device and a **Copy report** button. Nothing is changed or removed.

## 0.2.2 - 2026-10-09

- **Choose which column is which.** A CSV, NDJSON or JSON file whose columns are not recognised is no longer
  refused: the app shows the file's columns and lets you pick one for each field; **Change columns** reopens
  the choice after importing.
- **A short welcome** on the first run, and **the window remembers its size and position**.
- **Help > Check for updates…** opens the releases page in your browser (the app makes no network request).
- **Fixed:** in 0.2.1 the Appearance choice (Auto, Light, Dark) was forgotten every time the app was closed.
  It and the welcome are now remembered across launches (they are kept in `ui-prefs.json`
  in the data folder; the panel's address changes on every launch, so the browser could not keep them).

## 0.2.1 - 2026-10-09

- A native-Mac look: a sidebar with icons, summary tiles, a card for each machine with trend lines, light and
  dark appearance that follows the system, an Auto / Light / Dark switch.
- A new icon: black by default; on macOS 26 and later it switches between a light and a dark look.
- **Send one test record** to check the address and headers of your own service before a full run.
- Response codes as chips, request latency as bars, and **Copy results**.
- Keyboard shortcuts: Command+1 to 5 (Ctrl+1 to 5 on Windows and Linux).

## 0.2.0 - 2026-10-09

- The Replay tab can send to **a service of your own** (address, headers, payload shape) and replay **your own
  CSV, NDJSON or JSON file**, with faults, bursts and a rate.
- A control panel rework, a clearer error screen when the engine cannot start, optional macOS signing.
- The desktop app: Electron shell, embedded SQLite, in-process generator and replayer.

## Before 0.2.0

The Go ingest service (bounded queue, `429` with `Retry-After`, idempotent storage, threshold alerts), the
Python simulator, replayer and fault injector, PostgreSQL, NGINX and Prometheus in Docker Compose, and the
Python unit and SIL test suites. These were not published as installers.
