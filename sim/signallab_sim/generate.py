"""Deterministic multi-device temperature/vibration generator and JSONL I/O.

Given the same GenerateConfig the output is identical (all randomness comes from a
single seeded random.Random and values are rounded to 2 decimals). Units:
temperature_c is degrees Celsius, vibration_mm_s is RMS velocity in mm/s.
"""

from __future__ import annotations

import json
import math
import random
from collections.abc import Iterable, Iterator
from dataclasses import dataclass
from datetime import UTC, datetime, timedelta
from pathlib import Path

DEVICE_KINDS = ("press", "pump", "mill", "lathe", "conveyor")
DEFAULT_START = "2025-01-15T08:00:00Z"


@dataclass(frozen=True)
class GenerateConfig:
    seed: int = 42
    devices: int = 3
    duration_s: float = 60.0
    interval_s: float = 1.0
    start: str = DEFAULT_START
    # Probability per event that a device begins a short fault episode in which its
    # readings exceed the service's default alert thresholds (85 C / 7.1 mm/s).
    anomaly_rate: float = 0.01
    site_id: str = "plant-a"


def format_time(t: datetime) -> str:
    """RFC 3339 UTC with millisecond precision, e.g. 2025-01-15T08:00:01.000Z."""
    return t.astimezone(UTC).strftime("%Y-%m-%dT%H:%M:%S.") + f"{t.microsecond // 1000:03d}Z"


def parse_time(s: str) -> datetime:
    return datetime.fromisoformat(s.replace("Z", "+00:00")).astimezone(UTC)


def device_ids(n: int) -> list[str]:
    return [f"{DEVICE_KINDS[i % len(DEVICE_KINDS)]}-{i + 1:02d}" for i in range(n)]


def generate(cfg: GenerateConfig) -> Iterator[dict]:
    """Yield event dicts ordered by time, then device."""
    if cfg.devices < 1 or cfg.interval_s <= 0 or cfg.duration_s <= 0:
        raise ValueError("devices, interval and duration must be positive")
    rng = random.Random(cfg.seed)
    start = parse_time(cfg.start)
    ids = device_ids(cfg.devices)
    base_temp = {d: 55 + 10 * rng.random() for d in ids}
    base_vib = {d: 1.5 + 2 * rng.random() for d in ids}
    phase = {d: 2 * math.pi * rng.random() for d in ids}
    episode: dict[str, tuple[int, str]] = {}  # device -> (events left, kind)

    steps = int(cfg.duration_s / cfg.interval_s)
    for k in range(steps):
        t = start + timedelta(milliseconds=round(k * cfg.interval_s * 1000))
        elapsed = k * cfg.interval_s
        for d in ids:
            temp = base_temp[d] + 4 * math.sin(2 * math.pi * elapsed / 300 + phase[d]) + rng.gauss(0, 0.4)
            vib = max(0.0, base_vib[d] + rng.gauss(0, 0.25))

            if d not in episode and rng.random() < cfg.anomaly_rate:
                episode[d] = (rng.randint(3, 6), rng.choice(("temp", "vib", "both")))
            if d in episode:
                left, kind = episode[d]
                if kind in ("temp", "both"):
                    temp += 40
                if kind in ("vib", "both"):
                    vib += 7
                if left <= 1:
                    del episode[d]
                else:
                    episode[d] = (left - 1, kind)

            seq = k + 1
            yield {
                "schema_version": 1,
                "event_id": f"{d}-{seq:06d}",
                "device_id": d,
                "event_time": format_time(t),
                "sequence": seq,
                "temperature_c": round(temp, 2),
                "vibration_mm_s": round(vib, 2),
                "site_id": cfg.site_id,
            }


def write_jsonl(path: Path | str, events: Iterable[dict]) -> int:
    """Write compact JSONL; returns the number of lines."""
    n = 0
    with open(path, "w", encoding="utf-8", newline="\n") as f:
        for ev in events:
            f.write(json.dumps(ev, separators=(",", ":")) + "\n")
            n += 1
    return n


def read_jsonl(path: Path | str) -> tuple[list[dict], int]:
    """Read records; returns (records, number of unreadable/non-object lines skipped)."""
    records: list[dict] = []
    skipped = 0
    with open(path, encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            try:
                obj = json.loads(line)
            except json.JSONDecodeError:
                skipped += 1
                continue
            if isinstance(obj, dict):
                records.append(obj)
            else:
                skipped += 1
    return records, skipped
