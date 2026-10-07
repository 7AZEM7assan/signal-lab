"""Record a replay against a running Signal Lab service for the static demo page.

Optional developer tool (not part of the simulator or the test suite). Standard library only.

    make up                                  # or run the binary against any PostgreSQL
    python sim/tools/record_demo.py --url http://localhost:8088 --out demo/recording.json

It generates a seeded dataset, opens the service's WebSocket feed, polls /metrics, and replays the
dataset with seeded faults (malformed, duplicate, late records) at a fixed rate. Everything the
monitor page would have seen is written to one JSON file with millisecond offsets, which
demo/index.html then plays back with no backend. For the queue meter to move, start the service
with a small SIGNALLAB_QUEUE_CAPACITY and a SIGNALLAB_LAB_WORKER_DELAY.

The service should be freshly started with empty tables, so the counters in the recording start
at zero.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
import urllib.request
from pathlib import Path
from urllib.parse import urlparse

SIM_DIR = Path(__file__).resolve().parents[1]

# Metrics kept per sample, in the order they appear in the recording's "metrics" rows.
COLUMNS = [
    "t",
    "queue_depth",
    "accepted",
    "rejected_invalid",
    "rejected_overload",
    "stored",
    "duplicate",
    "failed",
]


def sim(*args: str, **kw) -> subprocess.CompletedProcess:
    env = {**os.environ, "PYTHONPATH": str(SIM_DIR)}
    return subprocess.run([sys.executable, "-m", "signallab_sim", *args], env=env, text=True, **kw)


def sample(text: str, name: str, label: str | None = None) -> float:
    total = 0.0
    for line in text.splitlines():
        if not line.startswith(name):
            continue
        rest = line[len(name) :]
        if rest[:1] not in ("{", " "):
            continue
        if label and label not in rest:
            continue
        total += float(line.rsplit(" ", 1)[1])
    return total


class WSReader(threading.Thread):
    """Minimal RFC 6455 client: text frames only, replies to pings, ignores everything else."""

    def __init__(self, url: str, t0: float):
        super().__init__(daemon=True)
        u = urlparse(url)
        self.sock = socket.create_connection((u.hostname, u.port or 80), timeout=10)
        key = base64.b64encode(os.urandom(16)).decode()
        self.sock.sendall(
            (
                f"GET /ws HTTP/1.1\r\nHost: {u.netloc}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n\r\n"
            ).encode()
        )
        head = b""
        while b"\r\n\r\n" not in head:
            chunk = self.sock.recv(4096)
            if not chunk:
                raise SystemExit("websocket handshake failed: connection closed")
            head += chunk
        head, _, self.buf = head.partition(b"\r\n\r\n")
        if b" 101 " not in head.split(b"\r\n", 1)[0]:
            raise SystemExit(f"websocket handshake failed: {head.split(chr(13).encode(), 1)[0]!r}")
        self.sock.settimeout(0.5)
        self.t0 = t0
        self.messages: list[dict] = []
        self.stop = False

    def _read(self, n: int) -> bytes:
        while len(self.buf) < n:
            if self.stop:
                raise EOFError
            try:
                chunk = self.sock.recv(65536)
            except TimeoutError:
                continue
            if not chunk:
                raise EOFError
            self.buf += chunk
        out, self.buf = self.buf[:n], self.buf[n:]
        return out

    def run(self) -> None:
        try:
            while not self.stop:
                b0, b1 = self._read(2)
                opcode, n = b0 & 0x0F, b1 & 0x7F
                if n == 126:
                    (n,) = struct.unpack(">H", self._read(2))
                elif n == 127:
                    (n,) = struct.unpack(">Q", self._read(8))
                payload = self._read(n)
                now = time.monotonic()
                if opcode == 0x1:
                    msg = json.loads(payload)
                    self.messages.append(
                        {"t": round((now - self.t0) * 1000), "type": msg["type"], "data": msg["data"]}
                    )
                elif opcode == 0x9:  # ping -> masked pong
                    mask = os.urandom(4)
                    masked = bytes(c ^ mask[i % 4] for i, c in enumerate(payload))
                    self.sock.sendall(bytes([0x8A, 0x80 | len(payload)]) + mask + masked)
                elif opcode == 0x8:
                    return
        except EOFError:
            return


def read_metrics(url: str) -> dict:
    with urllib.request.urlopen(f"{url}/metrics", timeout=5) as r:
        text = r.read().decode()
    ing = "signallab_ingest_events_total"
    proc = "signallab_processed_events_total"
    return {
        "queue_depth": sample(text, "signallab_queue_depth"),
        "queue_capacity": sample(text, "signallab_queue_capacity"),
        "accepted": sample(text, ing, 'outcome="accepted"'),
        "rejected_invalid": sample(text, ing, 'outcome="rejected_invalid"'),
        "rejected_overload": sample(text, ing, 'outcome="rejected_overload"'),
        "stored": sample(text, proc, 'outcome="stored"'),
        "duplicate": sample(text, proc, 'outcome="duplicate"'),
        "failed": sample(text, proc, 'outcome="failed"'),
    }


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://localhost:8088")
    ap.add_argument("--out", default="demo/recording.json")
    ap.add_argument("--seed", type=int, default=11)
    ap.add_argument("--devices", type=int, default=8)
    ap.add_argument("--duration", type=int, default=300, help="seconds of simulated device time")
    ap.add_argument("--interval", type=int, default=5)
    ap.add_argument("--rate", type=float, default=60.0, help="replay events per second")
    ap.add_argument("--batch-size", type=int, default=10)
    ap.add_argument("--anomaly-rate", type=float, default=0.02)
    ap.add_argument("--malformed-rate", type=float, default=0.03)
    ap.add_argument("--duplicate-rate", type=float, default=0.05)
    ap.add_argument("--late-rate", type=float, default=0.03)
    ap.add_argument("--service-config", default="", help="free-text note on how the service was configured")
    args = ap.parse_args()

    with tempfile.TemporaryDirectory() as tmp:
        data = str(Path(tmp) / "demo.jsonl")
        sim(
            "generate",
            "--out",
            data,
            "--seed",
            str(args.seed),
            "--devices",
            str(args.devices),
            "--duration",
            str(args.duration),
            "--interval",
            str(args.interval),
            "--anomaly-rate",
            str(args.anomaly_rate),
            check=True,
            capture_output=True,
        )
        n_records = len(Path(data).read_text().splitlines())

        first = read_metrics(args.url)
        if first["accepted"] or first["stored"]:
            raise SystemExit("service already has traffic; restart it with empty tables first")
        cap = first["queue_capacity"]

        t0 = time.monotonic()
        ws = WSReader(args.url, t0)
        ws.start()
        rows: list[list[float]] = []
        stop_poll = threading.Event()

        def poll() -> None:
            while not stop_poll.is_set():
                m = read_metrics(args.url)
                rows.append([round((time.monotonic() - t0) * 1000)] + [int(m[c]) for c in COLUMNS[1:]])
                stop_poll.wait(0.25)

        poller = threading.Thread(target=poll, daemon=True)
        poller.start()
        time.sleep(0.5)

        replay = sim(
            "replay",
            data,
            "--url",
            args.url,
            "--rate",
            str(args.rate),
            "--batch-size",
            str(args.batch_size),
            "--concurrency",
            "2",
            "--retries",
            "20",
            "--seed",
            str(args.seed),
            "--malformed-rate",
            str(args.malformed_rate),
            "--duplicate-rate",
            str(args.duplicate_rate),
            "--late-rate",
            str(args.late_rate),
            "--json",
            check=True,
            capture_output=True,
        )
        summary = json.loads(replay.stdout)

        # Drain: wait until the queue is empty and the processed counters stop moving.
        last, stable = None, 0
        while stable < 6:
            time.sleep(0.25)
            cur = rows[-1][1:] if rows else None
            stable = stable + 1 if (cur == last and cur and cur[0] == 0) else 0
            last = cur
        time.sleep(0.5)
        stop_poll.set()
        poller.join()
        ws.stop = True
        ws.join(timeout=3)

    events = [m for m in ws.messages if m["type"] == "event"]
    alerts = [m for m in ws.messages if m["type"] == "alert"]
    out = {
        "meta": {
            "recorded_at": time.strftime("%Y-%m-%d", time.gmtime()),
            "source": "real Signal Lab service: python -m signallab_sim replay -> Go service -> PostgreSQL",
            "seed": args.seed,
            "devices": args.devices,
            "records_in_dataset": n_records,
            "queue_capacity": int(cap),
            "service_config": args.service_config,
            "replay": {
                "rate_per_s": args.rate,
                "batch_size": args.batch_size,
                "malformed_rate": args.malformed_rate,
                "duplicate_rate": args.duplicate_rate,
                "late_rate": args.late_rate,
            },
            "replay_summary": summary,
            "duration_ms": rows[-1][0],
        },
        "columns": COLUMNS,
        "metrics": rows,
        "messages": ws.messages,
    }
    path = Path(args.out)
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(out, separators=(",", ":")) + "\n")
    print(
        f"wrote {path}: {len(events)} events, {len(alerts)} alerts, {len(rows)} metric samples, "
        f"{out['meta']['duration_ms'] / 1000:.1f} s, {path.stat().st_size / 1024:.0f} KiB"
    )
    print("final counters:", dict(zip(COLUMNS, rows[-1], strict=True)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
