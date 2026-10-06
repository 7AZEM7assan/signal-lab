"""SIL: live WebSocket delivery, and a stuck client that must not hold up ingestion."""

import json
import socket
import time
from urllib.parse import urlsplit

import pytest
from conftest import SAMPLE
from sil_stack import wait_until
from websockets.sync.client import connect

from signallab_sim.generate import GenerateConfig, generate, read_jsonl
from signallab_sim.replay import ReplayConfig, run

pytestmark = pytest.mark.sil


def ws_url(stack, query: str = "") -> str:
    return "ws" + stack.base_url[4:] + "/ws" + query


def collect(ws, want_events: int, want_alerts: int, timeout: float = 20.0) -> tuple[list[dict], list[dict]]:
    events, alerts = [], []
    deadline = time.monotonic() + timeout
    while (len(events) < want_events or len(alerts) < want_alerts) and time.monotonic() < deadline:
        try:
            msg = json.loads(ws.recv(timeout=max(0.1, deadline - time.monotonic())))
        except TimeoutError:
            break
        (events if msg["type"] == "event" else alerts if msg["type"] == "alert" else []).append(msg["data"])
    return events, alerts


def test_client_receives_events_and_alerts_live(service):
    with connect(ws_url(service)) as ws:
        hello = json.loads(ws.recv(timeout=5))
        assert hello["type"] == "hello"  # the subscription is registered once hello arrives
        records, _ = read_jsonl(SAMPLE)
        run(records, ReplayConfig(url=service.base_url, batch_size=10))
        events, alerts = collect(ws, want_events=30, want_alerts=6)
    assert len(events) == 30 and len(alerts) == 6
    assert {a["alert_id"] for a in alerts} >= {
        "press-01-000006:temperature_high",
        "pump-02-000004:vibration_high",
    }
    assert set(events[0]) == {"event_id", "device_id", "event_time", "temperature_c", "vibration_mm_s"}


def test_device_filter_only_delivers_that_device(service):
    with connect(ws_url(service, "?device_id=mill-03")) as ws:
        ws.recv(timeout=5)  # hello
        records, _ = read_jsonl(SAMPLE)
        run(records, ReplayConfig(url=service.base_url, batch_size=10))
        events, alerts = collect(ws, want_events=10, want_alerts=2)
    assert len(events) == 10 and {e["device_id"] for e in events} == {"mill-03"}
    assert len(alerts) == 2  # mill-03 seq 9 crosses both thresholds


def open_stuck_client(stack) -> socket.socket:
    """A WebSocket client that completes the handshake and then never reads again."""
    parts = urlsplit(stack.base_url)
    sock = socket.socket()
    sock.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 2048)
    sock.connect((parts.hostname, parts.port))
    sock.sendall(
        (
            "GET /ws HTTP/1.1\r\n"
            f"Host: {parts.hostname}:{parts.port}\r\n"
            "Upgrade: websocket\r\nConnection: Upgrade\r\n"
            "Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n"
        ).encode()
    )
    assert b"101" in sock.recv(1024).split(b"\r\n")[0]
    return sock


def test_stuck_client_does_not_stall_ingestion_or_other_clients(service):
    # Small write timeout so a stuck peer is cut off quickly once kernel buffers fill.
    service.configure(ws_write_timeout="500ms")
    service.reset_data()
    stuck = open_stuck_client(service)
    try:
        wait_until(lambda: service.metric("signallab_ws_clients") >= 1, what="stuck client registered")
        records = list(generate(GenerateConfig(seed=21, devices=10, duration_s=300, anomaly_rate=0.0)))
        with connect(ws_url(service)) as healthy:
            healthy.recv(timeout=5)
            summary = run(
                records, ReplayConfig(url=service.base_url, batch_size=100, concurrency=4, retries=20)
            )
            events, _ = collect(healthy, want_events=len(records), want_alerts=0, timeout=30)
        assert summary.request_errors == 0 and summary.accepted == len(records)
        wait_until(lambda: service.count_events() == len(records), timeout=30, what="all events persisted")
        assert len(events) == len(records)  # the healthy client saw everything despite the stuck one
    finally:
        stuck.close()
