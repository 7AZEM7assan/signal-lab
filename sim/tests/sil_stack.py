"""Software-in-the-loop harness: drives a disposable Docker Compose stack.

The stack (deploy/compose.test.yml) runs the real Go service image against a real
PostgreSQL, in its own compose project with its own volume, so tests never touch
a developer's local data. The service's configuration can be changed per test by
recreating only the app container with different SIGNALLAB_* environment values.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import time
import urllib.error
import urllib.request
from collections.abc import Callable
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
COMPOSE_FILE = REPO / "deploy" / "compose.test.yml"

# Must mirror the defaults in deploy/compose.test.yml. Passing every knob explicitly
# keeps a developer's own SIGNALLAB_* shell variables from leaking into tests.
DEFAULTS = {
    "SIGNALLAB_QUEUE_CAPACITY": "1000",
    "SIGNALLAB_MAX_BATCH_EVENTS": "500",
    "SIGNALLAB_WORKERS": "4",
    "SIGNALLAB_WORKER_BATCH_SIZE": "100",
    "SIGNALLAB_LAB_WORKER_DELAY": "0s",
    "SIGNALLAB_WS_CLIENT_BUFFER": "1024",
    "SIGNALLAB_WS_WRITE_TIMEOUT": "5s",
    "SIGNALLAB_PERSIST_ATTEMPTS": "10",
    "SIGNALLAB_PERSIST_BACKOFF": "200ms",
    "SIGNALLAB_DB_QUERY_TIMEOUT": "5s",
    "SIGNALLAB_SHUTDOWN_TIMEOUT": "15s",
}


def docker_available() -> bool:
    if shutil.which("docker") is None:
        return False
    try:
        return subprocess.run(["docker", "info"], capture_output=True, timeout=20).returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def wait_until(
    predicate: Callable[[], bool], timeout: float = 20.0, interval: float = 0.1, what: str = ""
) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if predicate():
                return
        except (OSError, urllib.error.URLError, ValueError, KeyError):
            pass
        time.sleep(interval)
    raise AssertionError(f"timed out after {timeout}s waiting for: {what or predicate}")


class Stack:
    def __init__(self) -> None:
        self.knobs = dict(DEFAULTS)
        self.base_url = ""

    # ---- docker compose plumbing ----
    def compose(self, *args: str, check: bool = True) -> subprocess.CompletedProcess:
        return subprocess.run(
            ["docker", "compose", "-f", str(COMPOSE_FILE), *args],
            cwd=REPO,
            env={**os.environ, **self.knobs},
            capture_output=True,
            text=True,
            check=check,
        )

    def start(self) -> None:
        self.compose("up", "-d", "--build", "--wait")
        self._discover_port()

    def stop(self) -> None:
        self.compose("down", "-v", "--remove-orphans", check=False)

    def _discover_port(self) -> None:
        out = self.compose("port", "app", "8080").stdout.strip().splitlines()[-1]
        self.base_url = "http://127.0.0.1:" + out.rsplit(":", 1)[1]

    def configure(self, **knobs: object) -> None:
        """Recreate the app container if its settings differ. Names are lowercase, without prefix."""
        desired = dict(DEFAULTS)
        for k, v in knobs.items():
            desired["SIGNALLAB_" + k.upper()] = str(v)
        if desired == self.knobs and self.is_ready():
            return
        self.knobs = desired
        self.compose("up", "-d", "--wait", "--force-recreate", "--no-deps", "app")
        self._discover_port()

    # ---- database ----
    def psql(self, sql: str) -> str:
        return self.compose(
            "exec", "-T", "postgres", "psql", "-U", "signallab", "-d", "signallab_test", "-tA", "-c", sql
        ).stdout.strip()

    def reset_data(self) -> None:
        self.psql("TRUNCATE alerts, events")

    def count_events(self) -> int:
        return int(self.psql("SELECT count(*) FROM events"))

    # ---- HTTP helpers ----
    def request(self, path: str, data: bytes | None = None, timeout: float = 10.0) -> tuple[int, bytes, dict]:
        req = urllib.request.Request(
            self.base_url + path, data=data, headers={"Content-Type": "application/json"}
        )
        try:
            with urllib.request.urlopen(req, timeout=timeout) as resp:
                return resp.status, resp.read(), dict(resp.headers)
        except urllib.error.HTTPError as e:
            return e.code, e.read(), dict(e.headers)

    def get_json(self, path: str) -> dict:
        status, body, _ = self.request(path)
        assert status == 200, f"GET {path} -> {status}: {body[:200]!r}"
        return json.loads(body)

    def is_ready(self) -> bool:
        try:
            return self.request("/readyz", timeout=3)[0] == 200
        except (OSError, urllib.error.URLError):
            return False

    def metric(self, name: str, **labels: str) -> float:
        """Sum of samples of `name` whose labels include all given label=value pairs."""
        text = self.request("/metrics")[1].decode()
        total = 0.0
        for line in text.splitlines():
            if not line.startswith(name) or line[len(name) : len(name) + 1] not in ("{", " "):
                continue
            if all(f'{k}="{v}"' in line for k, v in labels.items()):
                total += float(line.rsplit(" ", 1)[1])
        return total

    def query_all(self, kind: str, start: str, end: str, limit: int = 500) -> list[dict]:
        """Walk every page of GET /api/v1/{events,alerts} using the cursor."""
        out: list[dict] = []
        cursor = ""
        while True:
            path = f"/api/v1/{kind}?from={start}&to={end}&limit={limit}" + (
                f"&cursor={cursor}" if cursor else ""
            )
            page = self.get_json(path)
            out.extend(page[kind])
            cursor = page.get("next_cursor", "")
            if not cursor:
                return out
