"""SIL: replay the committed deterministic dataset and verify persisted events and alerts."""

import json

import pytest
from conftest import EXPECTED, SAMPLE
from sil_stack import wait_until

from signallab_sim.generate import read_jsonl
from signallab_sim.replay import ReplayConfig, run

pytestmark = pytest.mark.sil

START, END = "2025-01-15T00:00:00Z", "2025-01-15T12:00:00Z"
EXPECTED_ALERTS = json.loads(EXPECTED.read_text())["alerts"]


def replay_sample(stack, **kw):
    records, _ = read_jsonl(SAMPLE)
    return run(records, ReplayConfig(url=stack.base_url, batch_size=10, **kw))


def test_replay_persists_every_event_and_exactly_the_expected_alerts(service):
    summary = replay_sample(service)
    assert (summary.attempted, summary.accepted, summary.rejected) == (30, 30, 0)
    assert summary.request_errors == 0 and summary.overloaded_records == 0

    wait_until(lambda: service.count_events() == 30, what="30 events persisted")
    events = service.query_all("events", START, END)
    assert len(events) == 30
    assert {e["event_id"] for e in events} == {r["event_id"] for r in read_jsonl(SAMPLE)[0]}

    alerts = service.query_all("alerts", START, END)
    got = {
        (a["alert_id"], a["device_id"], a["event_id"], a["rule"], a["threshold"], a["observed"])
        for a in alerts
    }
    want = {
        (a["alert_id"], a["device_id"], a["event_id"], a["rule"], a["threshold"], a["observed"])
        for a in EXPECTED_ALERTS
    }
    assert got == want
    assert len(alerts) == len(EXPECTED_ALERTS) == 6  # includes both boundary values: 85.0 C and 7.1 mm/s

    assert service.metric("signallab_ingest_events_total", outcome="accepted") == 30
    wait_until(
        lambda: service.metric("signallab_processed_events_total", outcome="stored") == 30,
        what="stored metric",
    )
    assert service.metric("signallab_alerts_created_total", rule="temperature_high") == 3
    assert service.metric("signallab_alerts_created_total", rule="vibration_high") == 3


def test_cursor_pagination_walks_all_events_exactly_once(service):
    replay_sample(service)
    wait_until(lambda: service.count_events() == 30, what="30 events persisted")
    events = service.query_all("events", START, END, limit=7)
    ids = [e["event_id"] for e in events]
    assert len(ids) == len(set(ids)) == 30
    times = [(e["event_time"], e["event_id"]) for e in events]
    assert times == sorted(times)


def test_device_filter_and_time_bounds(service):
    replay_sample(service)
    wait_until(lambda: service.count_events() == 30, what="30 events persisted")
    page = service.get_json(f"/api/v1/events?device_id=pump-02&from={START}&to={END}")
    assert {e["device_id"] for e in page["events"]} == {"pump-02"} and len(page["events"]) == 10
    page = service.get_json("/api/v1/events?from=2025-01-15T08:00:02Z&to=2025-01-15T08:00:04Z")
    assert len(page["events"]) == 6  # seconds 2 and 3 for three devices; the upper bound is exclusive


def test_replaying_the_same_file_twice_is_idempotent(service):
    replay_sample(service)
    wait_until(lambda: service.count_events() == 30, what="first pass persisted")
    second = replay_sample(service)
    assert second.accepted == 30  # acknowledged: duplicates are resolved after the ack, by the database
    wait_until(
        lambda: service.metric("signallab_processed_events_total", outcome="duplicate") == 30,
        what="30 duplicates recognised",
    )
    assert service.count_events() == 30
    assert len(service.query_all("alerts", START, END)) == 6  # no re-alerting
