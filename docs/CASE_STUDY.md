# Signal Lab: case study

A **simulated telemetry replay and validation lab**. This is a portfolio write-up of what the project demonstrates and, just as importantly, what it does not.

## What problem the lab demonstrates

Telemetry services rarely fail on the happy path. They fail when a burst arrives faster than the database can absorb it, when a client resends a batch it is not sure landed, when one malformed record sits in the middle of a good batch, when a dashboard client stops reading, or when the database disappears for half a minute. Signal Lab is a small, runnable system built to make those situations **repeatable and checkable**: a Go service ingests simulated temperature and vibration readings from a fictional factory, and a Python tool replays a known dataset against it while injecting specific faults. The lab then verifies, with automated tests, that the service behaved the way its documentation says it should.

The emphasis is on explicit contracts: what "accepted" means, what happens when the queue is full, which failures lose data and how that loss is made visible.

## Why replay and fault injection are useful

- **Replay** turns "it seemed to work" into "the same input gives the same output". A seeded generator and a committed 30-event sample with hand-checkable alerts (including exact threshold boundary values) make expectations small enough to audit.
- **Fault injection** with a recorded seed makes failure scenarios reproducible instead of anecdotal. Malformed records, duplicates, late timestamps, bursts and request jitter are opt-in flags, and the expected service outcome is derived from the seeded plan, not from a previous run of the service, so the test is not circular.
- Together they let one command answer a concrete question: *at this offered load, with these faults, does the service stay up, answer predictably, and persist exactly what it acknowledged?*

## Architecture and key engineering decisions

```text
simulator/replayer -> HTTP ingest -> validate -> bounded queue -> workers -> PostgreSQL
                                                                   \-> WebSocket hub -> monitor
Prometheus <- /metrics        NGINX in front of the Go service
```

Key decisions, each documented in the README:

1. **Acknowledgement contract.** A 202 means "validated and queued in memory", not "stored". That keeps the hot path fast and the contract easy to state; later failures are reported through metrics and logs. The cost, stated plainly, is that acknowledged events can be lost on a crash or a long database outage.
2. **Bounded queue with all-or-nothing enqueue.** Memory is bounded by construction. When a request's valid records do not fit, the service returns 429 with `Retry-After` and accepts none of them, so clients retry the whole batch and never have to reconcile partial acceptance. The concurrency argument is small enough to read in one comment: only enqueuers add to the channel and they hold a mutex, so free space computed under the lock can only grow.
3. **Backpressure through retries.** Workers retry failed persists with capped exponential backoff and take nothing new while retrying, so a slow or briefly unavailable database shows up as a filling queue and then 429s rather than silent loss. After the retry window, data is discarded **and counted**.
4. **Idempotent storage instead of exactly-once.** `event_id` is the primary key, `(device_id, sequence)` is unique, and inserts use `ON CONFLICT DO NOTHING`. Replays and client retries are safe. Alerts have deterministic IDs (`<event_id>:<rule>`) and are written in the same transaction as their events, inside a savepoint so an alert-write failure cannot lose the events.
5. **Slow WebSocket clients are disconnected, not buffered or silently dropped.** Broadcast is non-blocking with a bounded per-client buffer, so a stuck client cannot stall ingestion or other clients, and a client never sees a gap without being told.
6. **Observability with bounded cardinality.** Metrics are labelled only by closed sets (outcome, validation reason, rule, route, DB operation); device and event IDs never become labels. Request IDs double as batch IDs across the HTTP response, the logs and the worker logs.
7. **Small surface, boring tools.** Standard `net/http`, PostgreSQL only (no second database without a need), an embedded migration runner, a standard-library Python tool.

## Testing strategy, including SIL

- **Unit tests** cover validation and bounds, threshold boundary values (below, equal, above), the duplicate policy, queue-full and all-or-nothing behaviour (including a concurrency test that checks depth never exceeds capacity), drain on shutdown, retry and give-up accounting, hub slow-client handling, config validation, and time calculations.
- **Integration tests** run the real migrations and queries against PostgreSQL in a uniquely named schema per test, so they never depend on existing data, plus the full ingest → database → HTTP → WebSocket path.
- **Software-in-the-loop (SIL)** tests start the actual service image and PostgreSQL with Docker Compose in a disposable project, replay the deterministic dataset, and verify the persisted events and alerts exactly, then exercise seeded faults, overload (small queue plus an artificially slow worker, checking 429s, bounded depth, and that stored equals acknowledged), a stuck WebSocket client, a database stop/start, and SIGTERM draining.
- **Race detection**: the Go suite is meant to be run with `-race` (it is part of the Make targets and CI workflow).

"Software-in-the-loop" here means exactly that: the software under test is the real service binary, driven by simulated inputs. It is **not** hardware-in-the-loop.

## Measured results

**None are recorded.** The repository defines a reproducible load exercise (`make load`: a seeded 50,000-event dataset replayed with 8 connections in batches of 100) and says what to capture (throughput, p50/p95/p99 request latency, 429 and error counts, plus CPU, RAM, OS, Docker/Go/Python versions, commit, parameters and several runs). Until someone runs it and records the environment, this section intentionally contains no numbers, and any numbers recorded later should be described as local results, not compared with other systems.

The same applies to test outcomes: the test suites are written and documented, and their output is not pasted anywhere in the repository.

## What remains simulated, and what was not tested

Simulated:

- All sensor data. The "factory" is a seeded generator; the devices do not exist.
- The consumers: the monitor page is a minimal client, not an operations product.

Not built or not tested:

- No real hardware, no hardware-in-the-loop, no physical or protocol-level sources (MQTT, OPC UA, serial).
- No production or industrial deployment, no real users, no uptime or scale claims.
- Single instance only; no clustering, no durable queue, no soak or long-duration testing, no multi-region or failover testing.
- No authentication, TLS, or hardened edge; the stack is for local use on loopback.
- The disconnect of a slow WebSocket client is verified at the hub level, not forced through real kernel socket buffers end to end.
- Database outage is simulated by stopping a container; other failure modes (network partitions, disk full, slow storage) are not exercised.

Future work, listed in the README, includes an MQTT/OPC UA adapter, a ClickHouse analytics sink, a hardware test adapter (with the conditions under which a test could honestly be called HIL), authentication and deployment hardening, and a durable queue.
