"""SIL: database outage/recovery and graceful shutdown."""

import json
import subprocess

import pytest
from sil_stack import wait_until

pytestmark = pytest.mark.sil


def batch(prefix: str, n: int) -> bytes:
    events = [
        {
            "schema_version": 1,
            "event_id": f"{prefix}-{i}",
            "device_id": "press-01",
            "event_time": f"2025-01-15T08:00:{i % 60:02d}Z",
            "sequence": i,
            "temperature_c": 60.0,
            "vibration_mm_s": 2.0,
        }
        for i in range(n)
    ]
    return json.dumps({"events": events}).encode()


def test_database_outage_is_visible_and_the_service_recovers(stack):
    stack.configure(db_query_timeout="1s", persist_attempts=2, persist_backoff="100ms")
    stack.reset_data()
    stack.compose("stop", "postgres")
    try:
        # Readiness reflects the database; liveness does not.
        wait_until(lambda: stack.request("/readyz")[0] == 503, timeout=20, what="/readyz to report 503")
        assert stack.request("/healthz")[0] == 200

        # Ingest still acknowledges (the queue has room), but the ack only means "queued".
        status, _, _ = stack.request("/api/v1/events", data=batch("outage", 5))
        assert status == 202
        # Workers give up after their retry budget; the loss is counted, not silent.
        wait_until(
            lambda: stack.metric("signallab_processed_events_total", outcome="failed") == 5,
            timeout=30,
            what="5 accepted events counted as failed",
        )
        assert stack.metric("signallab_processing_failures_total", stage="persist") >= 1
        # Queries fail explicitly instead of hanging.
        assert stack.request("/api/v1/events?from=2025-01-15T00:00:00Z&to=2025-01-15T01:00:00Z")[0] == 503
    finally:
        stack.compose("start", "postgres")

    wait_until(stack.is_ready, timeout=60, what="/readyz to recover")
    status, _, _ = stack.request("/api/v1/events", data=batch("after", 5))
    assert status == 202
    wait_until(lambda: stack.count_events() == 5, what="post-recovery events stored")
    ids = stack.psql("SELECT string_agg(event_id, ',' ORDER BY event_id) FROM events")
    assert (
        "outage" not in ids and "after-0" in ids
    )  # events accepted during the outage were lost, as documented


def container_state(stack, field: str) -> str:
    cid = stack.compose("ps", "-a", "-q", "app").stdout.strip()
    return subprocess.run(
        ["docker", "inspect", "--format", "{{." + field + "}}", cid],
        capture_output=True,
        text=True,
        check=True,
    ).stdout.strip()


def test_sigterm_drains_the_queue_and_exits_cleanly(stack):
    # A slow single worker leaves ~100 accepted events queued when SIGTERM arrives.
    stack.configure(workers=1, worker_batch_size=10, lab_worker_delay="150ms", shutdown_timeout="20s")
    stack.reset_data()
    status, _, _ = stack.request("/api/v1/events", data=batch("drain", 100))
    assert status == 202
    assert stack.count_events() < 100  # not everything is stored yet

    stack.compose("kill", "-s", "SIGTERM", "app")
    wait_until(
        lambda: container_state(stack, "State.Status") == "exited", timeout=40, what="app container to exit"
    )

    assert container_state(stack, "State.ExitCode") == "0"
    assert stack.count_events() == 100  # the drain stored everything that had been acknowledged
    logs = stack.compose("logs", "--no-log-prefix", "app").stdout
    assert "queue drained" in logs and "shutdown complete" in logs
