# Local load-exercise results

These are **local results from one machine, three runs per configuration**. They describe how this
code behaved in this environment on this date. They are not a performance claim, not a capacity
rating, and not comparable to any other system. Latencies are HTTP request latencies (the
acknowledgement), not time-to-persist.

Date: 2026-10-06. Code: commit `28a2b83` of the repository this project was developed in before it moved to its own repository, so that hash does not exist here. The service code (`cmd/`, `internal/`, the Dockerfile, Compose files and the Python simulator) is byte-for-byte unchanged since then; only the monitor page, CI scripts, demo and docs changed.

## Environment

| | |
|---|---|
| Machine | cloud sandbox VM: 4 vCPU Intel Xeon @ 2.80 GHz, 16 GiB RAM, Linux 6.18.44 |
| OS | Ubuntu 24.04.5 LTS |
| Docker | Engine 29.8.2, Compose v5.6.0 (overlayfs) |
| Images | `postgres:17-alpine`, `nginx:1.28-alpine`, `prom/prometheus:v3.5.0`; service built from the repo Dockerfile (Go 1.25) |
| Client | Python 3.13.16 (`signallab_sim`, standard library), run on the **same machine** as the stack |
| Stack | the full `docker-compose.yml` stack: client → NGINX → Go service → PostgreSQL, Prometheus scraping |
| Notes | the client, NGINX, the service and PostgreSQL all share the same 4 vCPUs. The image was built with host networking and a proxy CA so it could download modules inside this sandbox (build-time only; the runtime image is unchanged). |

## Method

Command (the documented one, defaults of the Makefile):

```bash
make load
# = generate data/load.jsonl : --seed 1 --devices 20 --duration 2500 --interval 1   (50,000 events)
# + replay  --url http://localhost:8088 --batch-size 100 --concurrency 8 --retries 5   (unpaced)
```

Per run: `TRUNCATE alerts, events`, restart the service container (so Prometheus counters start at zero),
wait for `/readyz`, then run `make load`, then poll PostgreSQL until all 50,000 events are visible
(that tail is "drain after replay"). PostgreSQL stays up between runs, so its caches are warm; the
cluster itself was freshly created for this session. Server-side percentiles are interpolated from the
service's own Prometheus histograms (same method as `histogram_quantile`), so they are only as fine as
the bucket boundaries.

Two configurations, three runs each:

1. **Default** (`SIGNALLAB_QUEUE_CAPACITY=1000`), exactly as documented.
2. **Supplementary, not the default**: the same command with `SIGNALLAB_QUEUE_CAPACITY=10000`, to separate
   the effect of the queue size from everything else.

Raw replayer summaries and timings are in [`benchmarks/raw/`](benchmarks/raw).

## Results: default configuration (queue capacity 1000)

| Run | Replay elapsed | Records/s attempted | 429 responses (all retried OK) | Request latency p50 / p95 / p99 / max (ms) | All 50,000 persisted after replay ends |
|---|---|---|---|---|---|
| 1 | 9.43 s | 5,302 | 71 | 4.2 / 19.5 / 42.9 / 52.1 | +0.22 s |
| 2 | 27.91 s | 1,791 | 215 | 8.9 / 21.0 / 27.4 / 35.2 | +0.18 s |
| 3 | 13.48 s | 3,709 | 103 | 5.9 / 17.9 / 25.8 / 35.5 | +0.19 s |

Every run: 50,000 accepted, 0 rejected, 0 overloaded after retries, 0 request errors, 50,000 events and
3,000 alerts (1,461 `temperature_high`, 1,539 `vibration_high`) in the database, 0 failed events, final
queue depth 0. Server side (per run): queue wait p50 ≈ 20–27 ms, p95 ≈ 48–50 ms, p99 ≈ 95–213 ms; one
persist transaction ≈ 15 ms mean (about 88 events per transaction).

**Reading these numbers.** The elapsed time is dominated by backpressure, not by the database. With 8
connections each sending 100-event batches against a 1,000-event queue, the queue regularly cannot take a
whole batch, the service answers 429 (as designed), and each retry then sleeps for `Retry-After`
(1 s). That is why the run time varies 3× (9.4 s to 27.9 s) with the number of 429s (71 to 215): the 1 s
sleeps synchronise the clients unpredictably. "Records/s attempted" here is therefore a property of this
client, queue size and retry policy, not the service's raw capacity.

## Results: supplementary (queue capacity 10,000, otherwise identical)

| Run | Replay elapsed | Records/s attempted | 429 responses (all retried OK) | Request latency p50 / p95 / p99 / max (ms) | All 50,000 persisted after replay ends |
|---|---|---|---|---|---|
| 1 | 3.75 s | 13,346 | 24 | 9.8 / 22.2 / 27.3 / 42.7 | +0.62 s |
| 2 | 3.78 s | 13,217 | 24 | 10.7 / 23.4 / 28.8 / 34.0 | +0.65 s |
| 3 | 3.72 s | 13,457 | 24 | 9.9 / 21.3 / 26.1 / 31.2 | +0.63 s |

Every run: 50,000 accepted, 0 request errors, 50,000 events and 3,000 alerts stored, 0 failed. Server
side: queue wait p50 ≈ 184–198 ms, p95 ≈ 442–462 ms, p99 ≈ 488–494 ms; persist transaction ≈ 16–17 ms mean
(about 98 events per transaction). Replay start to all-events-persisted is about 4.4 s, i.e. roughly
11.3–11.5 thousand events/s end to end in this setup.

Even with a 10,000-event queue the replay briefly outruns the four workers: 24 batches per run (2,400
events) were still refused once and then accepted on retry. The queue wait is larger here (about 0.2 s)
because the queue is allowed to fill, which is the trade the capacity setting makes: more absorbed burst,
more waiting.

## What this does and does not show

- It shows the documented behaviour under load on one machine: a bounded queue, prompt 429 backpressure
  with `Retry-After`, no request errors, no lost accepted events (`stored` equals `accepted` in all six
  runs), idempotent counts, and alerts matching the dataset.
- It does **not** show a maximum throughput: the client and the whole stack compete for 4 vCPUs, the client
  is single-process Python, and retry sleeps dominate the default runs.
- Only three runs per configuration, one machine, no isolation from other processes: treat the spread
  between runs as part of the result rather than quoting a single figure.
- No soak, no tuning of workers or batch size, no comparison with other systems or settings beyond the
  queue capacity above.

To reproduce: `make up && make load` (see the README), and record your own environment alongside the
numbers.
