"""Small summary statistics, standard library only."""

from __future__ import annotations

import math
from collections.abc import Sequence


def percentile(sorted_values: Sequence[float], p: float) -> float:
    """Nearest-rank percentile of an ascending sequence. p is in (0, 100]."""
    if not sorted_values:
        return 0.0
    rank = max(1, math.ceil(p / 100.0 * len(sorted_values)))
    return sorted_values[min(rank, len(sorted_values)) - 1]


def latency_summary(latencies_s: Sequence[float]) -> dict[str, float]:
    """p50/p95/p99/max/mean of request latencies, reported in milliseconds."""
    if not latencies_s:
        return {"count": 0, "p50_ms": 0.0, "p95_ms": 0.0, "p99_ms": 0.0, "max_ms": 0.0, "mean_ms": 0.0}
    ordered = sorted(latencies_s)
    return {
        "count": len(ordered),
        "p50_ms": round(percentile(ordered, 50) * 1000, 3),
        "p95_ms": round(percentile(ordered, 95) * 1000, 3),
        "p99_ms": round(percentile(ordered, 99) * 1000, 3),
        "max_ms": round(ordered[-1] * 1000, 3),
        "mean_ms": round(sum(ordered) / len(ordered) * 1000, 3),
    }
