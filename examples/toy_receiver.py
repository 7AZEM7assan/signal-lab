#!/usr/bin/env python3
"""A tiny stand-in for the service you want to test. Standard library only.

    python3 examples/toy_receiver.py
    python3 examples/toy_receiver.py --capacity 200 --drain 100 --api-key secret

It listens on http://127.0.0.1:3000/ingest and accepts every payload shape Signal Lab can send
(batch, array, ndjson, single). It shows what a service under test might do:

* a record that is not valid gets a 400 and nothing from that request is kept;
* an event_id it has seen before is counted as a duplicate and kept once (idempotency);
* when its pretend queue is full it answers 429 with Retry-After, like a service under load;
* with --api-key it wants an X-Api-Key header, so you can try the Headers box.

Press Ctrl-C for a summary. This is a teaching toy, not a model of a real service.
"""

import argparse
import json
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REQUIRED = ("event_id", "device_id", "event_time", "temperature_c", "vibration_mm_s")


class State:
    def __init__(self, capacity, drain):
        self.capacity, self.drain = capacity, drain
        self.level, self.updated = 0.0, time.monotonic()
        self.seen, self.lock = set(), threading.Lock()
        self.requests = self.events = self.duplicates = self.throttled = (
            self.refused
        ) = 0

    def room_for(self, n):
        now = time.monotonic()
        self.level = max(0.0, self.level - (now - self.updated) * self.drain)
        self.updated = now
        if self.level + n > self.capacity:
            return False
        self.level += n
        return True


def parse(body, content_type):
    text = body.decode("utf-8")
    if "ndjson" in content_type:
        return [json.loads(line) for line in text.splitlines() if line.strip()]
    data = json.loads(text)
    if isinstance(data, dict) and isinstance(data.get("events"), list):
        return data["events"]
    return data if isinstance(data, list) else [data]


def problem(rec):
    if not isinstance(rec, dict):
        return "a record is not a JSON object"
    for field in REQUIRED:
        if field not in rec:
            return f"missing field {field}"
    for field in ("temperature_c", "vibration_mm_s"):
        if isinstance(rec[field], bool) or not isinstance(rec[field], (int, float)):
            return f"{field} must be a number"
    return None


def make_handler(state, api_key):
    class Handler(BaseHTTPRequestHandler):
        def reply(self, status, payload, retry_after=None):
            body = json.dumps(payload).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            if retry_after is not None:
                self.send_header("Retry-After", str(retry_after))
            self.end_headers()
            self.wfile.write(body)

        def do_POST(self):
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            if api_key and self.headers.get("X-Api-Key") != api_key:
                return self.reply(401, {"error": "missing or wrong X-Api-Key header"})
            try:
                records = parse(body, self.headers.get("Content-Type", ""))
            except (ValueError, UnicodeDecodeError):
                return self.reply(400, {"error": "the body is not valid JSON"})
            bad = next((p for p in map(problem, records) if p), None)
            with state.lock:
                state.requests += 1
                if bad:
                    state.refused += 1
                    return self.reply(400, {"error": bad})
                if not state.room_for(len(records)):
                    state.throttled += 1
                    return self.reply(
                        429, {"error": "too busy, try again"}, retry_after=1
                    )
                new = 0
                for rec in records:
                    if rec["event_id"] in state.seen:
                        state.duplicates += 1
                    else:
                        state.seen.add(rec["event_id"])
                        new += 1
                state.events += new
            self.reply(202, {"accepted": len(records), "stored": new})

        def log_message(self, fmt, *args):
            pass

    return Handler


def main():
    ap = argparse.ArgumentParser(description="a toy service to try Signal Lab against")
    ap.add_argument("--port", type=int, default=3000)
    ap.add_argument(
        "--capacity",
        type=int,
        default=100000,
        help="pretend queue size in events (small = more 429s)",
    )
    ap.add_argument(
        "--drain",
        type=float,
        default=10000,
        help="events per second the pretend queue empties at",
    )
    ap.add_argument("--api-key", default="", help="require this X-Api-Key header")
    args = ap.parse_args()
    state = State(args.capacity, args.drain)
    server = ThreadingHTTPServer(
        ("127.0.0.1", args.port), make_handler(state, args.api_key)
    )
    print(f"listening on http://127.0.0.1:{args.port}/ingest  (Ctrl-C for a summary)")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    print(
        f"\n{state.requests} requests: {state.events} events stored once, {state.duplicates} duplicates skipped, "
        f"{state.throttled} answered 429, {state.refused} refused as invalid"
    )


if __name__ == "__main__":
    main()
