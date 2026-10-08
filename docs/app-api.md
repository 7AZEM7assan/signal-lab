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
| `PUT /app/api/replay/dataset` | import a file to replay: the CSV, NDJSON or JSON file is the request body (up to 32 MB, 200,000 rows), `?name=` is shown as its name. Replaces the previous file; kept in memory only. `400 invalid_dataset` says what is wrong. Returns `{"dataset": summary}` |
| `GET /app/api/replay/dataset` | the imported file's summary (`{"dataset": null}` when none): `name`, `format`, `rows`, `devices`, `columns`, `mapped`, `ignored`, `derived`, `first_time`, `last_time`, and `problems` (rows the Signal Lab schema would reject, by reason) |
| `DELETE /app/api/replay/dataset` | forget the imported file |
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

To send to a service of your own, add:

| Field | Meaning |
|---|---|
| `target_url` | `http` or `https` address that accepts `POST`. Empty (default) = the app's built-in service. No credentials in the address |
| `target_headers` | object of extra request headers, for example `{"Authorization":"Bearer ..."}`; at most 20; `Host`, `Content-Length`, `Connection` and similar are refused. Needs `target_url` |
| `payload_format` | `batch` (default, `{"events":[...]}`), `array`, `ndjson` or `single` (one record per request; the batch size becomes 1). Needs `target_url` |
| `target_confirmed` | must be `true` unless the host is `localhost`/`127.0.0.1`/`::1`: "I own this service or may test it". Outside this computer the rate must also be 1-2,000 records/s |

To send your own data, import a file first, then add `"use_dataset": true` (and optionally
`"rebase_time": true` to shift the timestamps so the newest reading is now). The generator fields
are then ignored; faults, rate, batching and retries still apply.

Sending to your own service never includes the app's token. The replay state and the start
response contain only the host (`target`, e.g. `https://api.example.com`), never header values, a
path or a query string; `config.target_headers` shows `(hidden)`. Extra state fields: `external`,
`source` (generated data or the file name), `status_counts` (responses by HTTP status) and
`first_error` (`batch`, `status`, `message`, and the start of the response `body`). For a service of
your own, any `2xx` counts as accepted, `429`/`503` are retried, other statuses are errors,
redirects are not followed, and nothing is stored in the app. `GET /app/api/state` also returns
`dataset` and `replay_limits`.

The generator is deterministic for a given configuration but is not byte-identical to the Python
tool's output (`sim/`): the shapes, units and fault semantics match.

### Settings fields

`temp_alert_c`, `vib_alert_mm_s`, `queue_capacity`, `workers`, `worker_batch_size`,
`lab_worker_delay_ms`. Values are validated by the same code the PostgreSQL service uses for its
environment configuration.
