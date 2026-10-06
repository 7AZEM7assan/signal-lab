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

Recorded in [`BENCHMARKS.md`](BENCHMARKS.md), with the environment, exact command, method and raw outputs. They are **local results from one machine (a 4 vCPU sandbox VM running the client, NGINX, the service and PostgreSQL together), three runs per configuration**, and should not be compared with other systems.

- **Documented command (`make load`, queue capacity 1000):** all 50,000 seeded events were accepted in every run with no request errors and no lost accepted events (50,000 stored, 3,000 alerts). The run took 9.4 s, 27.9 s and 13.5 s. That spread is the backpressure policy at work: 71 to 215 batches were refused with 429 and each retry waited the 1 s `Retry-After`. Request latency was p50 4–9 ms, p99 26–43 ms. A persist transaction took about 15 ms for roughly 88 events.
- **Supplementary (queue capacity 10,000, nothing else changed):** about 3.7 s per run (~13,000 records/s attempted, ~11,400 events/s counting the drain after the replay), still with 24 refused-then-retried batches per run, and a queue wait of about 0.2 s.

What this supports: the bounded queue, the all-or-nothing 429 contract and the idempotent pipeline behave as documented under a burst. What it does not support: a maximum-throughput or capacity claim (the client and stack share four CPUs and retry sleeps dominate the default runs).

Test outcomes are not pasted into the repository; run the suites to see them.

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
