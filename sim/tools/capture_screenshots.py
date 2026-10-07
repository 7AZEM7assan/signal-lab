"""Capture screenshots of the monitor page while a seeded replay runs.

Optional developer tool (not part of the simulator or the test suite). It needs Playwright
and a Chromium build:

    pip install playwright && playwright install chromium
    make up
    python sim/tools/capture_screenshots.py --out docs/screenshots

Set CHROMIUM_PATH to use an existing Chromium binary instead of Playwright's own.

For each viewport it (optionally) restarts the app and truncates the tables so the counters on
the page start at zero, generates a seeded dataset, opens the page, replays the dataset at a
fixed rate, and saves a full-page screenshot. It also fails if the page logs console errors or
overflows horizontally, which makes it a cheap smoke test of the page itself.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from playwright.sync_api import sync_playwright

VIEWPORTS = {
    "monitor-desktop-light": {"viewport": {"width": 1280, "height": 900}, "color_scheme": "light"},
    "monitor-desktop-dark": {"viewport": {"width": 1280, "height": 900}, "color_scheme": "dark"},
    "monitor-mobile": {
        "viewport": {"width": 390, "height": 844},
        "color_scheme": "light",
        "device_scale_factor": 2,
    },
}
SIM_DIR = Path(__file__).resolve().parents[1]


def sim(*args: str) -> subprocess.CompletedProcess:
    env = {**os.environ, "PYTHONPATH": str(SIM_DIR)}
    return subprocess.run(
        [sys.executable, "-m", "signallab_sim", *args], env=env, check=True, capture_output=True, text=True
    )


def reset_stack() -> None:
    subprocess.run(
        [
            "docker",
            "compose",
            "exec",
            "-T",
            "postgres",
            "psql",
            "-U",
            "signallab",
            "-d",
            "signallab",
            "-qc",
            "TRUNCATE alerts, events",
        ],
        check=True,
        capture_output=True,
    )
    subprocess.run(["docker", "compose", "restart", "app"], check=True, capture_output=True)


def wait_ready(url: str) -> None:
    for _ in range(60):
        if subprocess.run(["curl", "-sf", "-o", "/dev/null", f"{url}/readyz"]).returncode == 0:
            return
        time.sleep(0.5)
    raise SystemExit("service did not become ready")


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--url", default="http://localhost:8088")
    ap.add_argument("--out", default="docs/screenshots")
    ap.add_argument("--seed", type=int, default=11)
    ap.add_argument("--devices", type=int, default=5)
    ap.add_argument(
        "--duration", type=int, default=40, help="seconds of simulated time (one event per device per second)"
    )
    ap.add_argument("--rate", type=int, default=40, help="replay rate in events per second")
    ap.add_argument(
        "--no-reset", action="store_true", help="do not truncate tables or restart the app between viewports"
    )
    args = ap.parse_args()

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    problems: list[str] = []

    with tempfile.TemporaryDirectory() as tmp, sync_playwright() as p:
        data = Path(tmp) / "demo.jsonl"
        sim(
            "generate",
            "--out",
            str(data),
            "--seed",
            str(args.seed),
            "--devices",
            str(args.devices),
            "--duration",
            str(args.duration),
            "--anomaly-rate",
            "0.05",
        )
        launch = (
            {"executable_path": os.environ["CHROMIUM_PATH"], "args": ["--no-sandbox"]}
            if os.environ.get("CHROMIUM_PATH")
            else {}
        )
        browser = p.chromium.launch(**launch)
        for name, ctx_args in VIEWPORTS.items():
            if not args.no_reset:
                reset_stack()
            wait_ready(args.url)
            ctx = browser.new_context(**ctx_args)
            page = ctx.new_page()
            logged: list[str] = []
            page.on(
                "console",
                lambda m, logged=logged: logged.append(m.text) if m.type in ("error", "warning") else None,
            )
            page.on("pageerror", lambda e, logged=logged: logged.append(str(e)))
            page.goto(args.url, wait_until="load")
            page.wait_for_function(
                "document.getElementById('ws').textContent.trim() === 'connected'", timeout=10_000
            )
            sim("replay", str(data), "--url", args.url, "--batch-size", "10", "--rate", str(args.rate))
            time.sleep(3)  # let the last frames and one metrics poll land
            page.screenshot(path=str(out / f"{name}.png"), full_page=True)
            if page.evaluate("document.documentElement.scrollWidth") > page.evaluate("window.innerWidth"):
                problems.append(f"{name}: page overflows horizontally")
            problems += [f"{name}: console: {m}" for m in logged]
            print(f"{name}: events={page.inner_text('#nEvents')} alerts={page.inner_text('#nAlerts')}")
            ctx.close()
        browser.close()

    for line in problems:
        print("PROBLEM", line, file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
