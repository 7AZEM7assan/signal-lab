# Signal Lab

A **simulated telemetry replay and validation lab**. It replays recorded (simulated) factory sensor data through a Go telemetry service, injects controllable faults, shows accepted events and alerts on a live WebSocket feed, exposes health and Prometheus metrics, and validates the behaviour with automated Python tests.

It is a focused educational/reference project for backend and systems engineering topics: Go concurrency, bounded queues and backpressure, streaming APIs, persistence, observability, reproducible replay, and automated validation.

> **Honest scope.** Everything here is simulated. No real hardware, plant, or production deployment is involved, there is no hardware-in-the-loop (HIL) testing, and the only performance numbers in this repository are the local, single-machine results in [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md) (see [Load exercise](#load-exercise)). It is not a production industrial platform.

**Audience:** engineers reviewing backend/systems work, and anyone who wants a small, readable example of an ingest pipeline with explicit delivery semantics.

## Scope and non-goals

In scope: one Go service, one Python simulator/replayer/test harness, PostgreSQL, Docker Compose, NGINX, Prometheus, a deliberately small web monitor.

Non-goals (kept out on purpose): Kubernetes, authentication and multi-tenancy, cloud deployment, real hardware integrations, ClickHouse, a rules engine, elaborate dashboards, exactly-once delivery. Extension points are listed [at the end](#extension-points).

## Quick start

Prerequisites: Docker with the Compose plugin, Python 3.11+, GNU make. (Go 1.25 is only needed to run Go tests or build outside Docker.)

```bash
make up              # build and start app, postgres, nginx, prometheus
make replay-sample   # replay the 30-event sample dataset
```

Then open:

| URL | What |
|---|---|
| <http://localhost:8088/> | Live monitor (connection state, latest readings, alert feed, queue health) |
| <http://localhost:8088/api/v1/events?device_id=press-01&from=2025-01-15T08:00:00Z&to=2025-01-15T09:00:00Z> | Stored events |
| <http://localhost:8088/api/v1/alerts?from=2025-01-15T08:00:00Z&to=2025-01-15T09:00:00Z> | Stored alerts |
| <http://localhost:8088/metrics> | Prometheus metrics (also scraped by Prometheus) |
| <http://localhost:9090/> | Prometheus UI |

Open the monitor first, then run `make replay-sample` to watch events and alerts arrive. Without a browser: `python -m websockets ws://localhost:8088/ws` (from `pip install websockets`).

Stop with `make down`. Delete the local database with `make reset`. Run `make help` for every target.

### Walkthrough: one event, one alert, metrics moving

```bash
curl -s localhost:8088/metrics | grep 'signallab_ingest_events_total{outcome="accepted"}'   # note the value

curl -s -X POST localhost:8088/api/v1/events -H 'Content-Type: application/json' -d '{"events":[{
  "schema_version":1,"event_id":"demo-1","device_id":"press-01","event_time":"2025-01-15T09:00:00Z",
  "sequence":1000,"temperature_c":91.2,"vibration_mm_s":2.0}]}'
# -> 202 {"batch_id":"...","received":1,"accepted":1,"rejected":0,"queue":{"depth":0,"capacity":1000}}

curl -s 'localhost:8088/api/v1/alerts?device_id=press-01&from=2025-01-15T09:00:00Z&to=2025-01-15T09:01:00Z'
# -> one alert: rule "temperature_high", threshold 85, observed 91.2, alert_id "demo-1:temperature_high"

curl -s localhost:8088/metrics | grep -E 'ingest_events_total\{outcome="accepted"|alerts_created_total\{rule="temperature'
```

`accepted` rose by 1 and `signallab_alerts_created_total{rule="temperature_high"}` rose by 1. The same event shows up on any open `/ws` connection as an `event` frame followed by an `alert` frame.

## Architecture

```text
Python simulator / JSONL replay
              |
              | HTTP batch ingestion  (POST /api/v1/events)
              v
       Go ingestion API  ---- validate each record; reject with a reason
              |
              |  all-or-nothing enqueue (bounded; full => 429 + Retry-After)
              v
    bounded in-memory queue  --> N workers (batched, retried)
        |               |
        |               +------> threshold alert evaluation (inside the same DB transaction)
        |
        +------> PostgreSQL persistence (idempotent: ON CONFLICT DO NOTHING)
        |
        +------> WebSocket hub: broadcast committed events/alerts (best effort)
                           |
                           v
                    live monitor client

Go service ---- /metrics ----> Prometheus
Browser / curl / simulator ---- NGINX reverse proxy ----> Go service
```

What each stage guarantees, and what it does not:

| Stage | Guarantees | Does **not** guarantee |
|---|---|---|
| **Ingest API** | Every record is validated. The response says, per record index, what was rejected and why. A 202 means all valid records of the request were queued; a 429 means *none* were. Memory use is bounded. | Durability. A 202 is not a persistence acknowledgement. |
| **Queue** | Bounded capacity (default 1000 events). Enqueue never blocks and never drops silently. | Survives restarts. It is plain process memory: events on it are lost if the process dies. |
| **Workers + PostgreSQL** | Events and their alerts commit in one transaction. Inserts are idempotent (duplicate event IDs and device/sequence pairs are skipped, not duplicated). Transient database errors are retried. | Ordering across events (workers run concurrently), exactly-once, or survival of an outage longer than the retry window (~26 s by default). |
| **WebSocket hub** | Only committed, new events are broadcast. A slow client never stalls ingestion or other clients. | Delivery. It is live-only: no replay, no buffering for absent clients, slow clients are disconnected. Use the HTTP query API to catch up. |
| **NGINX** | Proxies HTTP and WebSocket upgrades. | Anything about delivery; it is a convenience front door for the demo. |

## Event schema

One versioned record, schema version 1. Units: `temperature_c` is degrees Celsius; `vibration_mm_s` is RMS vibration velocity in millimetres per second.

```json
{
  "schema_version": 1,
  "event_id": "press-01-000006",
  "device_id": "press-01",
  "event_time": "2025-01-15T08:00:05.000Z",
  "sequence": 6,
  "temperature_c": 85.0,
  "vibration_mm_s": 2.6,
  "site_id": "plant-a"
}
```

| Field | Rules |
|---|---|
| `schema_version` | required, must be `1` |
| `event_id` | required, `[A-Za-z0-9][A-Za-z0-9._:-]{0,63}`, globally unique, the idempotency key |
| `device_id` | required, same pattern |
| `event_time` | required, RFC 3339 source timestamp, normalised to UTC with microsecond precision; may be arbitrarily old (replays), but not more than `MAX_FUTURE_SKEW` (5 min) ahead of the server |
| `sequence` | optional integer ≥ 0, per-device counter |
| `temperature_c` | required, finite, within `[-50, 250]` by default |
| `vibration_mm_s` | required, finite, within `[0, 100]` by default |
| `site_id` | optional, same pattern as IDs |
| `received_at` | **server-assigned** and stored; if a client sends it the record is rejected (`unknown_field`) rather than trusted |

Source time and receive time are kept separate so latency can be measured: `signallab_event_source_lag_seconds` observes `received_at − event_time`. For a historical replay that lag is huge unless you pass `--rebase-time` to the replayer, which shifts timestamps so the first record is "now".

Validation failures never crash the service. Each rejection carries a bounded `reason` and a fixed `detail` string that never echoes payload content; untrusted payloads are not logged.

| Reason | Meaning |
|---|---|
| `malformed_json` | record is not a JSON object of the expected shape, or a field has the wrong JSON type |
| `unknown_field` | record has a field outside schema v1 (including `received_at`) |
| `unsupported_schema_version` | `schema_version` is not 1 |
| `missing_field` | a required field is absent |
| `invalid_event_id` / `invalid_device_id` / `invalid_site_id` | identifier does not match the pattern |
| `invalid_timestamp` | `event_time` is not RFC 3339 |
| `time_in_future` | `event_time` is beyond the allowed clock skew |
| `invalid_sequence` | negative `sequence` |
| `out_of_range` | temperature/vibration not finite or outside the configured bounds |
| `duplicate_in_batch` | repeats an earlier `event_id` or `(device_id, sequence)` in the same request |

**Duplicate policy.** Within one request, a repeat is rejected immediately (`duplicate_in_batch`). Across requests, the database decides: `event_id` is the primary key and `(device_id, sequence)` is unique where `sequence` is present; inserts use `ON CONFLICT DO NOTHING`. A cross-request duplicate is therefore *acknowledged* (202, because the ack happens before the database is consulted), then skipped: it is not stored again, not re-alerted, not re-broadcast, and counted in `signallab_processed_events_total{outcome="duplicate"}`.

Out-of-order and late events are accepted and stored under their source `event_time`; there is no watermark or lateness window.

## HTTP API

Full definitions: [`docs/openapi.yaml`](docs/openapi.yaml). Errors use `{"error":{"code":"...","message":"..."}}`.

| Endpoint | Purpose |
|---|---|
| `GET /healthz` | Liveness. 200 while the process runs; does not check the database. |
| `GET /readyz` | Readiness. 200 only if the database answers a ping **and** the service is not draining; otherwise 503. |
| `POST /api/v1/events` | Ingest `{"events":[...]}` (max 500 events, 1 MiB by default). |
| `GET /api/v1/events` | Query events. `from`, `to` required (RFC 3339, half-open `[from, to)`, at most 24 h apart); optional `device_id`, `limit` (1–1000, default 100), `cursor`. Ordered by `(event_time, event_id)`; a response with more rows includes `next_cursor`. |
| `GET /api/v1/alerts` | Same parameters; alerts ordered by `(event_time, alert_id)`. |
| `GET /metrics` | Prometheus text format. |
| `GET /ws` | WebSocket; optional `?device_id=`. |

Ingest status codes:

| Status | When |
|---|---|
| `202` | The valid records were queued. Invalid records are listed in `rejections` (index, reason, detail), with `rejection_counts` by reason. |
| `400` | Not a JSON object with a non-empty `events` array (`invalid_json`, `empty_batch`). |
| `413` | Body over the byte limit, or more than `MAX_BATCH_EVENTS` records. |
| `429` | The queue cannot take the batch's valid records. **None** were accepted; resend the whole batch after `Retry-After` (1 s). |
| `503` | The service is draining for shutdown (`Retry-After: 5`). |

Example 202 body from a batch with one bad record:

```json
{"batch_id":"3f9c1a0b2d4e6f80","received":3,"accepted":2,"rejected":1,
 "rejections":[{"index":1,"reason":"out_of_range","detail":"temperature_c must be finite and within [-50, 250]"}],
 "rejection_counts":{"out_of_range":1},"queue":{"depth":2,"capacity":1000}}
```

`batch_id` equals the `X-Request-ID` response header and appears in the service logs. A well-formed `X-Request-ID` request header is honoured; NGINX sets one otherwise.

### WebSocket

Text frames `{"type": "hello" | "event" | "alert", "data": {...}}`. `hello` arrives once the subscription is registered, so a client that waits for it cannot miss later frames. Events carry only `event_id, device_id, event_time, temperature_c, vibration_mm_s`; alerts carry `alert_id, device_id, event_id, rule, threshold, observed, event_time`.

Slow-client policy: each client has a bounded send buffer (`WS_CLIENT_BUFFER`, default 1024 frames). If it fills, or a single write exceeds `WS_WRITE_TIMEOUT` (5 s), the client is **disconnected** (close code 1008) and `signallab_ws_slow_disconnects_total` increments. Disconnecting was chosen over silently dropping frames so a client can never observe a gap without knowing; it should reconnect and resync through the query API. Broadcasting never blocks, so one slow client cannot hold up workers or other clients. Cross-origin browser connections are refused unless listed in `WS_ALLOWED_ORIGINS`; same-origin (through NGINX, which forwards `Host`) works.

## Delivery semantics and backpressure

- **"Accepted" means validated and queued in memory.** It does not mean stored. If the process dies, queued events are lost. If persistence later fails, that is reported through metrics and logs, not to the client who already got a 202.
- **Bounded queue, explicit overload policy.** Capacity is `SIGNALLAB_QUEUE_CAPACITY` (default 1000 events). When a request's valid records do not fit, the service answers `429` promptly with `Retry-After` instead of blocking or dropping. Enqueue is all-or-nothing per request, so a client never has to work out which half of a batch got in. Config validation requires `MAX_BATCH_EVENTS ≤ QUEUE_CAPACITY`, otherwise a full-size batch could never succeed.
- **Counters have fixed meanings.** `ingest_events_total{outcome}` counts attempts at the API: `accepted` (queued), `rejected_invalid`, `rejected_overload` (valid but refused for a full queue), `rejected_shutdown`. A client retry of a 429'd batch is counted again. `processed_events_total{outcome}` counts what workers did with accepted events: `stored`, `duplicate`, `failed`.
- **Persistence and retries.** Workers take up to `WORKER_BATCH_SIZE` events and write them in one transaction. A failed attempt is retried with exponential backoff (200 ms doubling, capped at 5 s, 10 attempts ≈ 26 s). While a worker retries it takes nothing new from the queue, so a database *slowdown or short outage* turns into a filling queue and then 429s. After the last attempt the batch is **discarded and counted** (`processed_events_total{outcome="failed"}`, `processing_failures_total{stage="persist"}`, an error log naming the batch ID). Accepted events can be lost this way; the system is honest about it rather than hiding it.
- **Not exactly-once.** The system is at-least-once from a client's point of view (resend until you get 202) and idempotent in storage (duplicates are skipped). A process stop mid-flight can lose queued events and can leave a client unsure whether a batch landed; resending is safe.
- **Ordering.** Workers run concurrently, so events are neither stored nor broadcast in arrival order. Queries order by source time.
- **Graceful shutdown (SIGTERM/SIGINT).** `/readyz` flips to 503; HTTP stops accepting and finishes in-flight requests; the queue is drained to the database while WebSocket clients are still connected (up to `SHUTDOWN_TIMEOUT`, default 15 s; anything left is discarded and counted as `shutdown_drop`); then WebSocket clients are closed and the pool is released. Ingest during the drain gets 503.

## Alerts

Two deterministic, inclusive threshold rules (defaults are illustrative values, not a standard):

| Rule | Fires when | Default |
|---|---|---|
| `temperature_high` | `temperature_c >= SIGNALLAB_TEMP_ALERT_C` | 85.0 °C |
| `vibration_high` | `vibration_mm_s >= SIGNALLAB_VIB_ALERT_MM_S` | 7.1 mm/s |

At most one alert per event per rule. The alert ID is deterministic, `<event_id>:<rule>`, so re-evaluating an event cannot create a second row. Each alert stores the alert ID, device, source event ID, rule, threshold, observed value, event time and creation time. Alerts are written in the **same transaction** as their events, inside a savepoint: if the alert insert fails, the events still commit and the failure is counted (`processing_failures_total{stage="alert_persist"}`) and logged. Only events that were newly inserted are evaluated, so replays do not re-alert.

## Persistence

PostgreSQL, schema in [`internal/store/migrations/`](internal/store/migrations) (embedded in the binary; applied in order, each in its own transaction, recorded in `schema_migrations`, serialised with an advisory lock).

- `events`: primary key `event_id`; unique `(device_id, sequence)` where `sequence IS NOT NULL`; indexes `(device_id, event_time, event_id)` and `(event_time, event_id)` serve the query endpoints and keyset pagination.
- `alerts`: primary key `alert_id`; same two index shapes.
- All queries are parameterised; every operation runs under `DB_QUERY_TIMEOUT` (5 s). Timestamps are `timestamptz` (microseconds).
- During a database outage: `/readyz` is 503, `/healthz` stays 200, queries return 503 `database_unavailable`, ingest keeps answering 202 until the queue fills (then 429), and workers retry as described above. The pool reconnects on its own when the database returns.

**Why PostgreSQL is enough here.** The demo handles a few thousand small rows per run, keyed lookups and time-range scans over indexed columns, and wants transactions, unique constraints for idempotency, and one familiar dependency. Running a second, analytical store would add operational surface with nothing in the scenario that needs it. A columnar store such as ClickHouse becomes interesting for long-retention aggregation across many devices; it is listed as an extension, not built.

Apply migrations manually with `make migrate` (the app also runs them on start when `SIGNALLAB_AUTO_MIGRATE=true`, which compose sets). Reset disposable data with `make reset`.

## Observability

Metrics (all labels come from small closed sets; device and event IDs are never labels):

| Metric | Labels | Meaning |
|---|---|---|
| `signallab_ingest_events_total` | `outcome` | accepted / rejected_invalid / rejected_overload / rejected_shutdown |
| `signallab_validation_failures_total` | `reason` | validation rejections by reason |
| `signallab_queue_depth`, `signallab_queue_capacity` | | current and configured queue size |
| `signallab_processed_events_total` | `outcome` | stored / duplicate / failed |
| `signallab_processing_failures_total` | `stage` | persist (each failed attempt) / alert_persist / shutdown_drop |
| `signallab_queue_wait_seconds` | | histogram: time from enqueue to a worker taking the event |
| `signallab_processing_duration_seconds` | | histogram: one persist-transaction attempt per worker batch |
| `signallab_event_source_lag_seconds` | | histogram: `received_at − event_time` |
| `signallab_alerts_created_total` | `rule` | alerts persisted |
| `signallab_ws_clients`, `signallab_ws_slow_disconnects_total` | | WebSocket clients; slow-client disconnects |
| `signallab_db_operation_duration_seconds`, `signallab_db_errors_total` | `op` | persist_batch / query_events / query_alerts / ping |
| `signallab_http_requests_total`, `signallab_http_request_duration_seconds` | `route`, `code` | by route pattern |

Plus Go runtime and process collectors. Logs are structured (JSON by default; `SIGNALLAB_LOG_FORMAT=text` for reading) with a request ID per request, which doubles as the ingest batch ID, and worker logs carry the same batch ID. Probe and scrape requests are not logged.

Profiling: `net/http/pprof` is **off by default**. Set `SIGNALLAB_PPROF_ADDR=127.0.0.1:6060` when running the binary directly (not through compose, where loopback inside the container is not reachable from the host) and use `go tool pprof http://127.0.0.1:6060/debug/pprof/profile`. Never bind it to a public address.

## Ports

| Port | Service | Exposure |
|---|---|---|
| 8088 (`SIGNALLAB_PORT`) | NGINX → web monitor, API, `/ws`, `/metrics` | `127.0.0.1` only |
| 9090 | Prometheus UI | `127.0.0.1` only |
| 8080 | Go service | compose network only |
| 5432 | PostgreSQL | compose network only |
| 55432 | PostgreSQL of the *test* stack (`deploy/compose.test.yml`) | `127.0.0.1` only, while tests run |
| ephemeral | Go service of the test stack | `127.0.0.1` only, while tests run |
| 6060 | pprof, only if `SIGNALLAB_PPROF_ADDR` is set | wherever you bind it |

## Configuration

All settings are `SIGNALLAB_*` environment variables, validated at startup (all problems are reported together and the process exits non-zero). Compose reads overrides from a `.env` file; copy [`.env.example`](.env.example).

| Variable | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | *(required)* | PostgreSQL connection URL |
| `HTTP_ADDR` | `:8080` | listen address |
| `QUEUE_CAPACITY` | `1000` | bounded queue size, in events |
| `WORKERS` / `WORKER_BATCH_SIZE` | `4` / `100` | worker goroutines; max events per transaction |
| `PERSIST_ATTEMPTS` / `PERSIST_BACKOFF` | `10` / `200ms` | tries per worker batch; first retry delay (doubles, capped at 5 s) |
| `LAB_WORKER_DELAY` | `0s` | **lab-only** artificial delay per worker batch, to make overload reproducible |
| `MAX_BODY_BYTES` / `MAX_BATCH_EVENTS` | `1048576` / `500` | request limits (`MAX_BATCH_EVENTS ≤ QUEUE_CAPACITY`) |
| `MAX_FUTURE_SKEW` | `5m` | how far ahead of the server `event_time` may be |
| `TEMP_MIN_C` / `TEMP_MAX_C` / `VIB_MAX_MM_S` | `-50` / `250` / `100` | plausibility bounds |
| `TEMP_ALERT_C` / `VIB_ALERT_MM_S` | `85` / `7.1` | alert thresholds (inclusive) |
| `QUERY_DEFAULT_LIMIT` / `QUERY_MAX_LIMIT` / `QUERY_MAX_RANGE` | `100` / `1000` / `24h` | query limits |
| `DB_MAX_CONNS` / `DB_QUERY_TIMEOUT` | `10` / `5s` | pool size; per-operation timeout |
| `WS_CLIENT_BUFFER` / `WS_MAX_CLIENTS` / `WS_WRITE_TIMEOUT` | `1024` / `256` / `5s` | WebSocket hub limits |
| `WS_ALLOWED_ORIGINS` | *(empty: same-origin only)* | comma-separated origin patterns |
| `SHUTDOWN_TIMEOUT` | `15s` | drain deadline |
| `AUTO_MIGRATE` | `false` (compose: `true`) | run migrations on start |
| `LOG_LEVEL` / `LOG_FORMAT` | `info` / `json` | logging |
| `PPROF_ADDR` | *(empty: off)* | pprof listener |

Running the binary without Docker:

```bash
export SIGNALLAB_DATABASE_URL='postgres://user:pass@localhost:5432/db?sslmode=disable'
go run ./cmd/signallab migrate
go run ./cmd/signallab            # serve
```

## Replay and fault injection

The Python tool is standard-library only. It runs without installing anything (`make` targets set `PYTHONPATH=sim`), or install it with `pip install -e sim`.

```bash
# Generate a deterministic dataset: same seed + flags => byte-identical file.
python -m signallab_sim generate --out data/run.jsonl --seed 42 --devices 5 --duration 120

# Replay at a target rate, or at a multiple of recorded time.
python -m signallab_sim replay data/run.jsonl --rate 200 --batch-size 50
python -m signallab_sim replay data/run.jsonl --speed 10 --rebase-time
```

(Run from `sim/` or with `PYTHONPATH=sim` if the package is not installed.) Without `--rate`/`--speed` the replay is unpaced. `--concurrency N` opens N connections (1 keeps request order). `--retries N` retries a batch after 429/503, honouring `Retry-After` (capped at 5 s).

Fault injection is opt-in and seeded; the seed is printed in every summary and all fault choices derive from it:

| Flag | Effect |
|---|---|
| `--malformed-rate p` | fraction of records replaced by an invalid variant (missing field, wrong type, out of range, bad timestamp, bad schema version, bad ID) |
| `--duplicate-rate p` | fraction of records sent twice, back to back |
| `--late-rate p`, `--late-seconds s` | fraction of records whose `event_time` moves `s` seconds into the past (delayed/out-of-order timestamps) |
| `--burst-every N`, `--burst-size M` | every Nth batch starts a burst of M batches released together |
| `--jitter-ms J` | random extra delay in `[0, J]` ms before each request |
| `--seed S` | seeds all of the above |

The summary reports attempted, accepted, rejected (with reasons), overloaded, request errors, elapsed time, and request latency p50/p95/p99/max. Exit code: 0 normally, 1 if any request failed at the transport/HTTP level, 2 for usage errors. A 429 is reported as overload, not an error.

### Sample scenario and expected outcome

`data/sample.jsonl` is 30 events (3 devices × 10 seconds) small enough to inspect by hand. It deliberately includes boundary values: `press-01` sequence 5 is 84.99 °C (no alert) and sequence 6 is exactly 85.0 (alert); `pump-02` sequence 4 is exactly 7.1 mm/s (alert) and sequence 5 is 7.09 (no alert); `mill-03` sequence 9 crosses both thresholds. [`data/sample.expected.json`](data/sample.expected.json) lists the 6 expected alerts: 3 `temperature_high` and 3 `vibration_high`.

```bash
make replay-sample      # expect: attempted 30, accepted 30, rejected 0
```

`make replay-faults` replays the same file in 4 batches of 10 with `--seed 7 --malformed-rate 0.1 --duplicate-rate 0.1 --late-rate 0.1`. Because the plan is seeded, the expected outcome is deterministic (derived from the planner, not from a service run):

| Quantity | Expected |
|---|---|
| records sent | 33 (30 + 3 injected duplicates) |
| injected | 3 malformed, 3 duplicates, 6 late |
| rejected by the service | 5 = 3 malformed + 2 duplicates that landed in the same batch as their original (`duplicate_in_batch`) |
| accepted (queued) | 28, including 1 duplicate that landed in a different batch |
| events stored | 27 (that cross-batch duplicate is acknowledged, then skipped by the database) |
| alerts stored | 5 (one corrupted record carried an alert, so it is absent) |

The SIL test `test_sil_faults.py` asserts exactly this policy, deriving the expectations from the same seeded plan.

## Testing

| Level | What | Command | Needs |
|---|---|---|---|
| Unit (Go) | validation and bounds, threshold boundaries, duplicate policy, queue-full/all-or-nothing, drain and retry, hub slow-client behaviour, API contract with fakes, WebSocket delivery, time calculations, config | `make test-go` | Go |
| Race detection | all Go tests under the race detector | `make test-race` | Go + cgo |
| Integration (Go) | migrations, idempotent persistence, duplicate rules, alert-failure isolation, pagination, and the full ingest → DB → HTTP → WebSocket path, each test in its own freshly migrated PostgreSQL schema | `make test-integration` | Docker (starts a throwaway Postgres) |
| Unit (Python) | generator determinism, seeded fault planning, pacing schedules, replayer against a stub server | `make test-py` | Python |
| SIL | the real service image and PostgreSQL in Docker Compose: replay the deterministic dataset and verify persisted events and alerts, seeded faults, overload/backpressure, live WebSocket delivery, a stuck client, database outage and recovery, SIGTERM drain | `make test-sil` | Docker |
| Everything | lint + all of the above | `make test-all` | all |

Python test dependencies: `pip install -e "sim[dev]"` (pytest, websockets, ruff). SIL and integration tests use a separate compose project (`signallab-test`) with its own volume, so they never touch local development data; the Go integration tests create and drop a uniquely named schema per test, so they do not depend on pre-existing database contents either. In CI, `SIGNALLAB_REQUIRE_DB=1` turns a missing database into a failure instead of a skip.

The CI workflow (`../.github/workflows/signal-lab.yml`) runs gofmt, `go vet`, Go tests with `-race` against a PostgreSQL service, ruff, Python unit tests, the SIL suite, and a container build.

**What these tests do not cover.** The slow-WebSocket-client policy is verified deterministically at the hub level (unit tests). The SIL test shows a stuck TCP client does not stall ingestion or other clients, but it cannot force kernel socket buffers to fill on demand, so it does not assert the disconnect itself. Database outage is simulated by stopping the Postgres container. There is no long-running soak, no multi-instance test, and no TLS.

**Test results.** None are recorded in this repository. Example output is deliberately not pasted here; run the commands above on your machine.

## Load exercise

A reproducible command and dataset recipe. Local results from one machine (three runs per configuration, with environment and method) are recorded in [`docs/BENCHMARKS.md`](docs/BENCHMARKS.md); in short, on a 4 vCPU sandbox VM the documented command acknowledged all 50,000 events with no request errors and no lost events, and its run time (9.4–27.9 s) was dominated by 429 backpressure and `Retry-After` sleeps rather than by the database. Those numbers are not a capacity claim.

```bash
make up
make load        # generates data/load.jsonl (20 devices x 2500 s = 50,000 events, seed 1) and replays it
                 # with 8 connections, batches of 100, retries on 429, unpaced
```

Tune with `LOAD_DEVICES`, `LOAD_DURATION`, `LOAD_CONCURRENCY`, `LOAD_BATCH`. The summary prints throughput and request latency p50/p95/p99/max, 429 counts and errors. Note that these are *HTTP request* latencies for the acknowledgement, not time-to-persist; for that, watch `signallab_queue_wait_seconds` and `signallab_processing_duration_seconds` in Prometheus.

If you record numbers, record them as local results with the environment: CPU model and core count, RAM, OS, Docker version, Go and Python versions, the git commit, the exact command and parameters, whether the database was warm, and several runs rather than one. Do not compare them against other systems.

## Failure behaviour

| Situation | Behaviour |
|---|---|
| Malformed / out-of-range / unknown-field record | Rejected with an index, bounded reason and fixed detail; counted in `validation_failures_total`; valid siblings in the batch are unaffected |
| Request not a valid envelope | 400; body too large or too many events: 413; nothing enqueued |
| Duplicate in the same request | Rejected `duplicate_in_batch` |
| Duplicate across requests | Acknowledged, skipped by the database, counted as `duplicate`; not re-alerted or re-broadcast |
| Queue full | 429 + `Retry-After`; none of the request's valid records accepted |
| Database slow or briefly down | Workers retry; queue fills; ingest returns 429; `/readyz` 503; queries 503 |
| Database down longer than the retry window | Affected accepted events are discarded and counted (`failed`); service keeps running and recovers when the database returns |
| Alert insert fails | Events still commit; failure counted and logged |
| Slow WebSocket client | Disconnected (close 1008); counted; others unaffected |
| SIGTERM | `/readyz` 503, drain queue (≤ `SHUTDOWN_TIMEOUT`), close WebSockets, exit 0 |
| SIGKILL / crash | Queued events are lost; stored data is intact |
| Invalid configuration | Process refuses to start and lists every problem |

## Security notes (local demo)

- Everything binds to `127.0.0.1`; PostgreSQL and the Go service are not published at all. There is **no authentication or TLS**. Do not expose these ports.
- The default database password in compose (`signallab`) is a well-known local-demo value, not a secret; override it via `.env` if the machine is shared. `.env` is git-ignored.
- `/metrics` and `/readyz` are proxied by NGINX for convenience (the monitor page reads them). In any shared deployment, keep metrics on an internal network.
- The container runs as a non-root user on a distroless image. pprof is off by default.
- Inputs are size-limited, validated, parameterised in SQL, and never echoed back in error details or logs. WebSocket origin checks are on by default.
- This repository is a GitHub Pages site; Pages would serve these files statically, but the service itself only runs locally.

## Dependency choices

- **Standard `net/http`** (Go 1.22+ method/pattern routing) for the server; no web framework.
- **[`github.com/coder/websocket`](https://github.com/coder/websocket)** for WebSockets: actively maintained, context-aware API, built-in origin checking, small. (`gorilla/websocket` would also work; this one fits context-based cancellation more naturally.)
- **[`pgx/v5`](https://github.com/jackc/pgx)** with `pgxpool`: the standard modern PostgreSQL driver; native batching and arrays are used for multi-row inserts.
- **[`prometheus/client_golang`](https://github.com/prometheus/client_golang)** with a private registry (so tests can create many).
- **Migrations:** a ~90-line embedded runner instead of a migration framework, because there is one schema and the behaviour (ordered files, one transaction each, advisory lock) is easy to read.
- **Python:** standard library only for the simulator. `pytest` and `websockets` are dev/test dependencies; NumPy was not needed for nearest-rank percentiles.
- Images: `postgres:17-alpine`, `nginx:1.28-alpine`, `prom/prometheus:v3.5.0`, `golang:1.25-alpine` → `distroless/static`.

## Known limitations

- Queued events are memory-only; acknowledged events can be lost on a crash or a long database outage (counted for the latter).
- Single instance only: no clustering, no shared queue, WebSocket fan-out is in-process.
- No replay or resume for WebSocket clients; no ordering guarantee across events.
- Thresholds are static and global; there is no per-device configuration, hysteresis or alert de-duplication across events (one alert per event per rule, so a sustained fault produces an alert for every reading).
- `/readyz` checks database connectivity, not that migrations have been applied.
- Timestamps are stored at microsecond precision.
- NGINX is a demo front door, not a hardened edge.

## Extension points

- **MQTT / OPC UA adapter.** A small separate process that subscribes to a broker or server, maps messages onto schema v1, and posts batches to `POST /api/v1/events`. The ingest API is already the boundary; the adapter owns its own buffering and retry using the 202/429/503 contract.
- **ClickHouse analytics sink.** Add a second consumer of committed events (after the PostgreSQL commit) that batches into ClickHouse for long-range aggregation. Keep PostgreSQL as the idempotent system of record.
- **Hardware test adapter and what would make a test HIL.** The same adapter boundary can connect a hardware simulator or physical source. This lab would only deserve the name *hardware-in-the-loop* once real controller or sensor hardware (or a certified real-time simulator of it) is in a closed loop with the system under test, with timing and I/O fidelity requirements defined and measured, hardware fault injection, and test-bench safety and calibration procedures. None of that exists here.
- **Authentication and deployment hardening.** API keys or mTLS on ingest, authenticated WebSockets, TLS at the proxy, metrics on an internal listener, resource limits, and a real secrets mechanism.
- **Durability.** A write-ahead log or message broker in front of the workers if acknowledged-means-durable is needed.

## Repository layout

```text
cmd/signallab/          entry point: serve | migrate | healthcheck
internal/config/        env parsing and validation
internal/event/         schema, validation, in-batch duplicate check, time helpers
internal/alert/         threshold rules
internal/pipeline/      bounded queue, workers, retry, drain
internal/store/         PostgreSQL access, migrations (embedded SQL)
internal/hub/           WebSocket fan-out and slow-client policy
internal/api/           HTTP handlers, middleware, WebSocket endpoint
internal/metrics/       Prometheus instruments
internal/testdb/        isolated-schema helper for integration tests
internal/e2e/           ingest -> DB -> HTTP -> WebSocket integration tests
sim/                    Python simulator, replayer, fault injector, pytest suites (unit + SIL)
data/                   sample dataset and its expected alerts
deploy/                 nginx, prometheus, and the disposable test compose file
web/                    the small monitor page
docs/                   OpenAPI definition and the portfolio case study
```

See also: [`docs/CASE_STUDY.md`](docs/CASE_STUDY.md).
