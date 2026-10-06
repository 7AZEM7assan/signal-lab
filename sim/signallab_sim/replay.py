"""Replay a JSONL dataset to a Signal Lab service in batches, with pacing and faults."""

from __future__ import annotations

import http.client
import json
import random
import threading
import time
from dataclasses import asdict, dataclass, field
from datetime import UTC, datetime, timedelta
from urllib.parse import urlsplit

from .faults import FaultConfig, FaultCounts, Planned, apply_bursts, plan_records
from .generate import format_time, parse_time
from .stats import latency_summary

MAX_RETRY_AFTER_S = 5.0


@dataclass(frozen=True)
class ReplayConfig:
    url: str = "http://localhost:8088"
    rate: float | None = None  # target events per second (mutually exclusive with speed)
    speed: float | None = None  # multiple of recorded time; 1.0 = real time
    batch_size: int = 50
    concurrency: int = 1
    retries: int = 0  # extra attempts after a 429/503 (honours Retry-After, capped)
    timeout_s: float = 10.0
    rebase_time: bool = False  # shift event_time so the first record is "now"
    faults: FaultConfig = field(default_factory=FaultConfig)

    def validate(self) -> None:
        if self.rate is not None and self.speed is not None:
            raise ValueError("use either rate or speed, not both")
        if (self.rate is not None and self.rate <= 0) or (self.speed is not None and self.speed <= 0):
            raise ValueError("rate and speed must be positive")
        if self.batch_size < 1 or self.concurrency < 1 or self.retries < 0:
            raise ValueError("batch_size and concurrency must be >= 1, retries >= 0")
        self.faults.validate()


@dataclass
class Summary:
    seed: int
    url: str
    attempted: int = 0  # records planned (including injected duplicates/malformed), each counted once
    batches: int = 0
    accepted: int = 0  # records the service acknowledged with 202 (queued, not necessarily stored)
    rejected: int = 0  # records rejected by validation (from the final response of each batch)
    rejection_counts: dict[str, int] = field(default_factory=dict)
    overloaded_records: int = 0  # valid records in batches finally refused with 429/503
    throttled_responses: int = 0  # number of 429 responses seen, including retried ones
    retries_used: int = 0
    request_errors: int = 0  # transport failures and unexpected statuses
    errored_records: int = 0
    skipped_unreadable_lines: int = 0
    elapsed_s: float = 0.0
    throughput_events_per_s: float = 0.0
    latency: dict[str, float] = field(default_factory=dict)
    injected: dict[str, int] = field(default_factory=dict)
    faults: dict = field(default_factory=dict)

    def to_dict(self) -> dict:
        return asdict(self)

    def format(self) -> str:
        lat = self.latency
        lines = [
            f"target            {self.url}",
            f"seed              {self.seed}",
            f"attempted         {self.attempted} records in {self.batches} batches",
            f"accepted          {self.accepted}  (queued by the service; not a durability guarantee)",
            f"rejected          {self.rejected}  {self.rejection_counts or ''}".rstrip(),
            f"overloaded        {self.overloaded_records} records "
            f"({self.throttled_responses} x 429, {self.retries_used} retries)",
            f"request errors    {self.request_errors} ({self.errored_records} records)",
            f"elapsed           {self.elapsed_s:.2f} s "
            f"({self.throughput_events_per_s:.1f} records/s attempted)",
            f"latency (ms)      p50={lat.get('p50_ms', 0)} p95={lat.get('p95_ms', 0)} "
            f"p99={lat.get('p99_ms', 0)} max={lat.get('max_ms', 0)}  over {lat.get('count', 0)} requests",
            f"injected faults   {self.injected}",
        ]
        if self.skipped_unreadable_lines:
            lines.append(
                f"skipped lines     {self.skipped_unreadable_lines} unreadable lines in the input file"
            )
        return "\n".join(lines)


def rebase(records: list[dict], now: datetime) -> list[dict]:
    """Shift all event_time values by one constant so the first record lands on `now`."""
    if not records:
        return records
    offset = now - parse_time(records[0]["event_time"])
    out = []
    for rec in records:
        rec = dict(rec)
        rec["event_time"] = format_time(parse_time(rec["event_time"]) + offset)
        out.append(rec)
    return out


def build_schedule(planned: list[Planned], cfg: ReplayConfig) -> tuple[list[list[Planned]], list[float]]:
    """Group records into batches and compute each batch's send offset in seconds."""
    batches = [planned[i : i + cfg.batch_size] for i in range(0, len(planned), cfg.batch_size)]
    sent_before = 0
    schedule: list[float] = []
    for batch in batches:
        if cfg.rate is not None:
            schedule.append(sent_before / cfg.rate)
        elif cfg.speed is not None:
            schedule.append(batch[0].t_rel / cfg.speed)
        else:
            schedule.append(0.0)
        sent_before += len(batch)
    return batches, apply_bursts(schedule, cfg.faults)


class _Sender:
    """One keep-alive HTTP connection per thread."""

    def __init__(self, url: str, timeout_s: float) -> None:
        parts = urlsplit(url)
        if parts.scheme not in ("http", "https") or not parts.hostname:
            raise ValueError(f"unsupported url: {url}")
        self._cls = http.client.HTTPSConnection if parts.scheme == "https" else http.client.HTTPConnection
        self._host, self._port = parts.hostname, parts.port
        self._base = parts.path.rstrip("/")
        self._timeout = timeout_s
        self._conn: http.client.HTTPConnection | None = None

    def post(self, body: bytes) -> tuple[int, dict, dict]:
        """POST a batch. Returns (status, parsed JSON body or {}, response headers)."""
        if self._conn is None:
            self._conn = self._cls(self._host, self._port, timeout=self._timeout)
        try:
            self._conn.request(
                "POST", self._base + "/api/v1/events", body, {"Content-Type": "application/json"}
            )
            resp = self._conn.getresponse()
            raw = resp.read()
        except (OSError, http.client.HTTPException):
            self.close()
            raise
        try:
            parsed = json.loads(raw) if raw else {}
        except json.JSONDecodeError:
            parsed = {}
        return resp.status, parsed if isinstance(parsed, dict) else {}, dict(resp.getheaders())

    def close(self) -> None:
        if self._conn is not None:
            self._conn.close()
            self._conn = None


def run(records: list[dict], cfg: ReplayConfig, skipped_lines: int = 0) -> Summary:
    """Replay records and return the run summary."""
    cfg.validate()
    if cfg.rebase_time:
        records = rebase(records, datetime.now(UTC) - timedelta(milliseconds=1))
    planned, fault_counts = plan_records(records, cfg.faults)
    batches, schedule = build_schedule(planned, cfg)

    summary = Summary(
        seed=cfg.faults.seed,
        url=cfg.url,
        attempted=len(planned),
        batches=len(batches),
        skipped_unreadable_lines=skipped_lines,
        faults=cfg.faults.describe(),
    )
    summary.injected = _injected(fault_counts)

    lock = threading.Lock()
    latencies: list[float] = []
    next_index = 0
    start = time.monotonic()

    def worker(worker_id: int) -> None:
        nonlocal next_index
        sender = _Sender(cfg.url, cfg.timeout_s)
        jitter_rng = random.Random(cfg.faults.seed * 1000 + worker_id)
        try:
            while True:
                with lock:
                    i = next_index
                    next_index += 1
                if i >= len(batches):
                    return
                delay = start + schedule[i] - time.monotonic()
                if cfg.faults.jitter_ms:
                    delay += jitter_rng.uniform(0, cfg.faults.jitter_ms) / 1000.0
                if delay > 0:
                    time.sleep(delay)
                _send_batch(sender, batches[i], cfg, summary, latencies, lock)
        finally:
            sender.close()

    threads = [threading.Thread(target=worker, args=(n,), daemon=True) for n in range(cfg.concurrency)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()

    summary.elapsed_s = time.monotonic() - start
    summary.throughput_events_per_s = summary.attempted / summary.elapsed_s if summary.elapsed_s > 0 else 0.0
    summary.latency = latency_summary(latencies)
    return summary


def _injected(c: FaultCounts) -> dict[str, int]:
    return {"malformed": c.malformed, "duplicates": c.duplicates, "late": c.late}


def _send_batch(
    sender: _Sender,
    batch: list[Planned],
    cfg: ReplayConfig,
    summary: Summary,
    latencies: list[float],
    lock: threading.Lock,
) -> None:
    body = ('{"events":[' + ",".join(p.body for p in batch) + "]}").encode()
    attempt = 0
    while True:
        t0 = time.monotonic()
        try:
            status, parsed, headers = sender.post(body)
        except (OSError, http.client.HTTPException):
            with lock:
                latencies.append(time.monotonic() - t0)
                summary.request_errors += 1
                summary.errored_records += len(batch)
            return
        elapsed = time.monotonic() - t0

        with lock:
            latencies.append(elapsed)
            if status == 202:
                _record_ack(summary, parsed)
                return
            if status in (429, 503):
                summary.throttled_responses += 1 if status == 429 else 0
                if attempt < cfg.retries:
                    attempt += 1
                    summary.retries_used += 1
                    wait = _retry_after(headers)
                else:
                    _record_rejections(summary, parsed)
                    summary.overloaded_records += len(batch) - int(parsed.get("rejected", 0))
                    return
            else:
                summary.request_errors += 1
                summary.errored_records += len(batch)
                return
        time.sleep(wait)


def _record_ack(summary: Summary, parsed: dict) -> None:
    summary.accepted += int(parsed.get("accepted", 0))
    _record_rejections(summary, parsed)


def _record_rejections(summary: Summary, parsed: dict) -> None:
    summary.rejected += int(parsed.get("rejected", 0))
    for reason, n in (parsed.get("rejection_counts") or {}).items():
        summary.rejection_counts[reason] = summary.rejection_counts.get(reason, 0) + int(n)


def _retry_after(headers: dict) -> float:
    for k, v in headers.items():
        if k.lower() == "retry-after":
            try:
                return min(max(float(v), 0.0), MAX_RETRY_AFTER_S)
            except ValueError:
                break
    return 1.0
