"""SIL: malformed, duplicate and late records replayed against the real service.

Expected outcomes are derived from the seeded fault plan (not from the service), so the
test checks the documented policy: invalid records are rejected with a reason and never
crash anything; duplicates inside one request are rejected immediately; duplicates across
requests are acknowledged and then skipped by the database; alerts fire once per event.
"""

import json

import pytest
from conftest import SAMPLE
from sil_stack import wait_until

from signallab_sim.faults import FaultConfig, plan_records
from signallab_sim.generate import read_jsonl
from signallab_sim.replay import ReplayConfig, run

pytestmark = pytest.mark.sil

START, END = "2025-01-15T00:00:00Z", "2025-01-15T12:00:00Z"
BATCH = 10
KNOWN_REASONS = {
    "malformed_json",
    "unknown_field",
    "unsupported_schema_version",
    "missing_field",
    "invalid_event_id",
    "invalid_device_id",
    "invalid_site_id",
    "invalid_timestamp",
    "time_in_future",
    "invalid_sequence",
    "out_of_range",
    "duplicate_in_batch",
}


def expected_outcome(records, faults):
    planned, counts = plan_records(records, faults)
    stored: dict[str, dict] = {}  # event_id -> first valid version
    in_batch_dups = 0
    for i in range(0, len(planned), BATCH):
        seen: set[str] = set()
        for p in planned[i : i + BATCH]:
            if p.kind == "malformed":
                continue
            rec = json.loads(p.body)
            if rec["event_id"] in seen:
                in_batch_dups += 1
            seen.add(rec["event_id"])
            stored.setdefault(rec["event_id"], rec)
    alerts = {
        f"{r['event_id']}:{rule}"
        for r in stored.values()
        for rule, hit in (
            ("temperature_high", r["temperature_c"] >= 85.0),
            ("vibration_high", r["vibration_mm_s"] >= 7.1),
        )
        if hit
    }
    return counts, stored, in_batch_dups, alerts


def test_seeded_faults_follow_the_documented_policy(service):
    records, _ = read_jsonl(SAMPLE)
    faults = FaultConfig(seed=7, malformed_rate=0.1, duplicate_rate=0.1, late_rate=0.1)
    counts, stored, in_batch_dups, alerts = expected_outcome(records, faults)
    assert (
        counts.malformed > 0 and counts.duplicates > 0 and counts.late > 0
    )  # the seed exercises every fault

    summary = run(records, ReplayConfig(url=service.base_url, batch_size=BATCH, faults=faults))

    assert summary.request_errors == 0
    assert summary.rejected == counts.malformed + in_batch_dups
    assert set(summary.rejection_counts) <= KNOWN_REASONS
    assert summary.rejection_counts.get("duplicate_in_batch", 0) == in_batch_dups
    assert summary.accepted == summary.attempted - summary.rejected

    wait_until(
        lambda: service.count_events() == len(stored), what=f"{len(stored)} unique valid events stored"
    )
    got_ids = {e["event_id"] for e in service.query_all("events", START, END)}
    assert got_ids == set(stored)
    got_alerts = {a["alert_id"] for a in service.query_all("alerts", START, END)}
    assert got_alerts == alerts

    # Every invalid record was counted under a bounded reason, and nothing crashed.
    assert service.metric("signallab_ingest_events_total", outcome="rejected_invalid") == summary.rejected
    assert service.is_ready()


def test_late_events_are_accepted_and_stored_at_their_source_time(service):
    records, _ = read_jsonl(SAMPLE)
    summary = run(
        records,
        ReplayConfig(
            url=service.base_url, batch_size=10, faults=FaultConfig(seed=2, late_rate=1.0, late_seconds=3600)
        ),
    )
    assert summary.rejected == 0 and summary.accepted == 30
    wait_until(lambda: service.count_events() == 30, what="late events stored")
    # Shifted one hour back: 07:00:00..07:00:09, not the original 08:00 window.
    page = service.get_json("/api/v1/events?from=2025-01-15T07:00:00Z&to=2025-01-15T07:30:00Z&limit=100")
    assert len(page["events"]) == 30
    assert (
        service.get_json("/api/v1/events?from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z")["events"] == []
    )
