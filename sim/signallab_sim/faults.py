"""Seeded, explicit fault injection applied on top of a recorded dataset.

Every fault is opt-in via a flag, and the seed is always printed in the run
summary, so a run can be reproduced exactly: same dataset + same FaultConfig =
same planned requests.
"""

from __future__ import annotations

import json
import random
from dataclasses import asdict, dataclass, field
from datetime import timedelta

from .generate import format_time, parse_time

MALFORMED_KINDS = (
    "missing_field",
    "wrong_type",
    "out_of_range",
    "bad_timestamp",
    "bad_schema_version",
    "bad_id",
)


@dataclass(frozen=True)
class FaultConfig:
    seed: int = 0
    malformed_rate: float = 0.0  # fraction of records replaced by an invalid variant
    duplicate_rate: float = 0.0  # fraction of records re-sent immediately (same event_id)
    late_rate: float = 0.0  # fraction of records whose event_time is moved into the past
    late_seconds: float = 120.0
    burst_every: int = 0  # every Nth batch starts a burst (0 = off)
    burst_size: int = 0  # number of batches released together in a burst
    jitter_ms: float = 0.0  # random extra delay before each request, uniform in [0, jitter]

    def validate(self) -> None:
        for name in ("malformed_rate", "duplicate_rate", "late_rate"):
            if not 0.0 <= getattr(self, name) <= 1.0:
                raise ValueError(f"{name} must be within [0, 1]")
        if min(self.late_seconds, self.jitter_ms) < 0 or min(self.burst_every, self.burst_size) < 0:
            raise ValueError("late_seconds, jitter_ms, burst_every and burst_size must be >= 0")

    def describe(self) -> dict:
        return asdict(self)


@dataclass
class FaultCounts:
    malformed: int = 0
    duplicates: int = 0
    late: int = 0
    malformed_by_kind: dict[str, int] = field(default_factory=dict)


@dataclass
class Planned:
    """One record to send: its JSON text, the kind of fault (if any) and its original offset."""

    body: str
    kind: str  # ok | malformed | duplicate | late
    t_rel: float  # seconds since the first record in the dataset, before any fault


def _corrupt(rec: dict, kind: str) -> dict:
    out = dict(rec)
    if kind == "missing_field":
        out.pop("temperature_c", None)
    elif kind == "wrong_type":
        out["temperature_c"] = "hot"
    elif kind == "out_of_range":
        out["temperature_c"] = 9999.0
    elif kind == "bad_timestamp":
        out["event_time"] = "not-a-timestamp"
    elif kind == "bad_schema_version":
        out["schema_version"] = 99
    elif kind == "bad_id":
        out["event_id"] = "bad id!"
    return out


def _dumps(rec: dict) -> str:
    return json.dumps(rec, separators=(",", ":"))


def plan_records(records: list[dict], cfg: FaultConfig) -> tuple[list[Planned], FaultCounts]:
    """Turn dataset records into the exact sequence of records that will be sent."""
    cfg.validate()
    rng = random.Random(cfg.seed)
    counts = FaultCounts()
    planned: list[Planned] = []
    first = parse_time(records[0]["event_time"]) if records else None

    for rec in records:
        # Always draw the same number of values per record so one fault's rate
        # does not shift the random stream seen by the others.
        u_malformed, u_late, u_dup = rng.random(), rng.random(), rng.random()
        kind_choice = rng.choice(MALFORMED_KINDS)

        try:
            t_rel = (parse_time(rec["event_time"]) - first).total_seconds()
        except (KeyError, ValueError, TypeError):
            t_rel = 0.0

        if u_malformed < cfg.malformed_rate:
            counts.malformed += 1
            counts.malformed_by_kind[kind_choice] = counts.malformed_by_kind.get(kind_choice, 0) + 1
            planned.append(Planned(_dumps(_corrupt(rec, kind_choice)), "malformed", t_rel))
            continue

        kind = "ok"
        if u_late < cfg.late_rate:
            rec = dict(rec)
            rec["event_time"] = format_time(
                parse_time(rec["event_time"]) - timedelta(seconds=cfg.late_seconds)
            )
            counts.late += 1
            kind = "late"
        body = _dumps(rec)
        planned.append(Planned(body, kind, t_rel))
        if u_dup < cfg.duplicate_rate:
            counts.duplicates += 1
            planned.append(Planned(body, "duplicate", t_rel))
    return planned, counts


def apply_bursts(schedule: list[float], cfg: FaultConfig) -> list[float]:
    """Release `burst_size` consecutive batches at the burst's first send time."""
    if cfg.burst_every <= 0 or cfg.burst_size <= 1:
        return schedule
    out = list(schedule)
    for i in range(cfg.burst_every, len(out), cfg.burst_every):
        for j in range(i, min(i + cfg.burst_size, len(out))):
            out[j] = schedule[i]
    return out
