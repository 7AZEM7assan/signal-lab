# Control API of the app

This is the small HTTP API behind the desktop app's control panel. It exists only in `signallab app`
(the desktop app and `make app`), not in the PostgreSQL service. The ingest, query, WebSocket and
metrics endpoints are the same as in [`openapi.yaml`](openapi.yaml).

It is meant for the bundled panel. It is stable enough to script against locally, but it is not a
public API and may change between versions.

## Access

The server listens on a loopback address only. Every request except `/healthz` and `/readyz` needs
the launch token, sent either way:

- `X-SignalLab-Token: <token>` header (for scripts), or
- the `sl_token` cookie, set when you open `/?token=<token>` in a browser. Writes with the cookie
  must also be same-origin and send `X-Requested-With: signallab`.

`make app` prints the token in its link. A wrong or missing token gives `401`, a foreign `Host`
header `403`.

```bash
TOKEN=...   # from the printed link
curl -H "X-SignalLab-Token: $TOKEN" http://127.0.0.1:PORT/app/api/state
```

Errors have the shape `{"error": {"code": "...", "message": "..."}}`.

## Endpoints

| Method and path | What it does |
|---|---|
| `GET /app/api/state` | version, data folder, engine status (queue depth/capacity, settings), storage statistics, default settings and replay defaults |
| `GET /app/api/replay` | progress of the current or last replay |
| `POST /app/api/replay/start` | start a replay; body is a JSON object with any of the fields below (omitted fields use the defaults); `409` if one is running |
| `POST /app/api/replay/stop` | stop a running replay and return its final state |
| `GET /app/api/settings` | current settings and defaults |
| `PUT /app/api/settings` | change settings (omitted fields keep their value). Thresholds apply at once; queue, workers, batch size and delay rebuild the engine (`409` while a replay runs). Response has `engine_rebuilt` |
| `POST /app/api/settings/reset` | restore the defaults |
| `GET /app/api/data` | browse stored rows: `kind` (events or alerts), `device_id`, `from`, `to` (RFC 3339), `limit` (1-1000), `cursor`. No time-range cap, unlike `/api/v1` |
| `GET /app/api/devices` | devices with stored events and their counts |
| `GET /app/api/export` | stream all matching rows: same filters, plus `format` = `csv` (default), `ndjson` or `json` |
| `POST /app/api/storage/clear` | delete all events and alerts; body must be `{"confirm":"DELETE"}`; `409` while a replay runs |
| `POST /app/api/shutdown` | drain the queue and exit |

### Replay fields

`seed`, `devices` (1-200), `duration_s`, `interval_s`, `anomaly_rate`, `site_id`, `start` (RFC 3339;
empty = the run ends now), `sequence_start` (0 = automatic), `rate_per_s` (0 = unpaced),
`batch_size`, `concurrency` (1-16), `retries` (0-100), `timeout_s`, `malformed_rate`,
`duplicate_rate`, `late_rate` (each 0-1), `late_seconds`, `burst_every`, `burst_size`, `jitter_ms`.
At most 500,000 readings per run. Setting `start`, `sequence_start` and `seed` to fixed values
resends exactly the same events, which the database skips as duplicates.

The generator is deterministic for a given configuration but is not byte-identical to the Python
tool's output (`sim/`): the shapes, units and fault semantics match.

### Settings fields

`temp_alert_c`, `vib_alert_mm_s`, `queue_capacity`, `workers`, `worker_batch_size`,
`lab_worker_delay_ms`. Values are validated by the same code the PostgreSQL service uses for its
environment configuration.
