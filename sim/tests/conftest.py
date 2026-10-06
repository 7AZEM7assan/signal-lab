"""Shared fixtures. SIL fixtures live in sil_stack.py and are only imported by SIL tests."""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
SAMPLE = REPO / "data" / "sample.jsonl"
EXPECTED = REPO / "data" / "sample.expected.json"


class StubService:
    """A tiny stand-in for the ingest endpoint, used to test the replayer in isolation."""

    def __init__(self) -> None:
        self.requests: list[list[dict]] = []
        self.throttle_first = 0  # answer the first N requests with 429
        self.lock = threading.Lock()
        stub = self

        class Handler(BaseHTTPRequestHandler):
            protocol_version = "HTTP/1.1"

            def log_message(self, *_args) -> None:  # keep test output quiet
                pass

            def do_POST(self) -> None:
                body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
                events = body["events"]
                with stub.lock:
                    stub.requests.append(events)
                    throttled = len(stub.requests) <= stub.throttle_first
                rejected = [e for e in events if not isinstance(e.get("temperature_c"), int | float)]
                accepted = 0 if throttled else len(events) - len(rejected)
                payload = {
                    "received": len(events),
                    "accepted": accepted,
                    "rejected": len(rejected),
                    "rejection_counts": {"stub_invalid": len(rejected)} if rejected else None,
                }
                data = json.dumps(payload).encode()
                self.send_response(429 if throttled else 202)
                if throttled:
                    self.send_header("Retry-After", "0")
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)

    @property
    def url(self) -> str:
        return f"http://127.0.0.1:{self.server.server_address[1]}"

    def __enter__(self) -> StubService:
        self.thread.start()
        return self

    def __exit__(self, *_exc) -> None:
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def stub():
    with StubService() as s:
        yield s


# ---- SIL fixtures (only used by tests marked `sil`) ----


@pytest.fixture(scope="session")
def stack():
    import sil_stack

    if not sil_stack.docker_available():
        pytest.skip("Docker is not available; SIL tests need `docker compose`")
    s = sil_stack.Stack()
    try:
        s.start()
        yield s
    finally:
        s.stop()


@pytest.fixture
def service(stack):
    """The service with default settings and empty tables."""
    stack.configure()
    stack.reset_data()
    return stack
