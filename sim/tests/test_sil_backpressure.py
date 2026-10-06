"""SIL: overload behaves predictably. A small queue plus an artificially slow worker is
hammered by concurrent clients; the service must answer 429 promptly, never exceed its
queue capacity, and persist exactly what it acknowledged."""

import contextlib
import threading
import time

import pytest
from sil_stack import wait_until

from signallab_sim.generate import GenerateConfig, generate
from signallab_sim.replay import ReplayConfig, run

pytestmark = pytest.mark.sil

CAPACITY = 40


@pytest.fixture
def slow_service(stack):
    # One worker taking at most 10 events per 300 ms transaction: ~33 events/s drain rate.
    stack.configure(
        queue_capacity=CAPACITY,
        max_batch_events=20,
        workers=1,
        worker_batch_size=10,
        lab_worker_delay="300ms",
    )
    stack.reset_data()
    return stack


def test_overload_returns_429_and_persists_exactly_what_was_acknowledged(slow_service):
    records = list(
        generate(GenerateConfig(seed=4, devices=4, duration_s=100, anomaly_rate=0.0))
    )  # 400 events

    max_depth = 0
    stop = threading.Event()

    def watch_queue():
        nonlocal max_depth
        while not stop.is_set():
            with contextlib.suppress(OSError):
                max_depth = max(max_depth, slow_service.metric("signallab_queue_depth"))
            time.sleep(0.02)

    watcher = threading.Thread(target=watch_queue, daemon=True)
    watcher.start()
    try:
        summary = run(
            records, ReplayConfig(url=slow_service.base_url, batch_size=20, concurrency=8, retries=0)
        )
    finally:
        stop.set()
        watcher.join()

    assert summary.request_errors == 0
    assert summary.throttled_responses > 0 and summary.overloaded_records > 0  # overload was actually reached
    assert summary.accepted + summary.overloaded_records == summary.attempted  # every record has one outcome
    assert max_depth <= CAPACITY  # bounded memory
    assert (
        slow_service.metric("signallab_ingest_events_total", outcome="rejected_overload")
        == summary.overloaded_records
    )

    # Acknowledged means eventually stored; refused means never stored.
    wait_until(
        lambda: slow_service.count_events() == summary.accepted, timeout=60, what="accepted events persisted"
    )
    assert slow_service.metric("signallab_processed_events_total", outcome="stored") == summary.accepted
    assert slow_service.metric("signallab_processed_events_total", outcome="failed") == 0


def test_clients_that_honour_retry_after_get_everything_through(slow_service):
    records = list(generate(GenerateConfig(seed=5, devices=2, duration_s=60, anomaly_rate=0.0)))  # 120 events
    summary = run(records, ReplayConfig(url=slow_service.base_url, batch_size=20, concurrency=6, retries=60))
    assert summary.request_errors == 0 and summary.overloaded_records == 0
    assert summary.accepted == len(records)
    wait_until(lambda: slow_service.count_events() == len(records), timeout=60, what="all events persisted")
