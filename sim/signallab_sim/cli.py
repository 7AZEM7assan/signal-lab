"""Command line interface: `python -m signallab_sim generate|replay ...`."""

from __future__ import annotations

import argparse
import json
import sys

from .faults import FaultConfig
from .generate import DEFAULT_START, GenerateConfig, generate, read_jsonl, write_jsonl
from .replay import ReplayConfig, run


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="signallab-sim", description=__doc__)
    sub = p.add_subparsers(dest="command", required=True)

    g = sub.add_parser("generate", help="write a deterministic JSONL dataset")
    g.add_argument("--out", required=True, help="output JSONL path")
    g.add_argument("--seed", type=int, default=42)
    g.add_argument("--devices", type=int, default=3)
    g.add_argument("--duration", type=float, default=60.0, help="seconds of simulated time")
    g.add_argument("--interval", type=float, default=1.0, help="seconds between readings per device")
    g.add_argument("--start", default=DEFAULT_START, help="RFC 3339 UTC start time")
    g.add_argument(
        "--anomaly-rate",
        type=float,
        default=0.01,
        help="per-event probability of starting a threshold-crossing episode",
    )
    g.add_argument("--site-id", default="plant-a")

    r = sub.add_parser("replay", help="replay a JSONL dataset to a running service")
    r.add_argument("file", help="input JSONL path")
    r.add_argument(
        "--url", default="http://localhost:8088", help="service base URL (the NGINX port by default)"
    )
    pace = r.add_mutually_exclusive_group()
    pace.add_argument("--rate", type=float, help="target events per second")
    pace.add_argument(
        "--speed", type=float, help="multiple of recorded time (1 = real time); default: no pacing"
    )
    r.add_argument("--batch-size", type=int, default=50)
    r.add_argument("--concurrency", type=int, default=1, help="parallel connections (1 keeps request order)")
    r.add_argument("--retries", type=int, default=0, help="retries per batch after 429/503")
    r.add_argument("--timeout", type=float, default=10.0, help="per-request timeout in seconds")
    r.add_argument(
        "--rebase-time",
        action="store_true",
        help="shift event_time so the first record is 'now' (makes source lag meaningful)",
    )
    r.add_argument("--json", action="store_true", help="print the summary as JSON")

    f = r.add_argument_group("fault injection (all off by default; seeded and reproducible)")
    f.add_argument("--seed", type=int, default=0, help="seed for fault selection and jitter")
    f.add_argument("--malformed-rate", type=float, default=0.0, help="fraction of records made invalid")
    f.add_argument("--duplicate-rate", type=float, default=0.0, help="fraction of records re-sent")
    f.add_argument("--late-rate", type=float, default=0.0, help="fraction of records with a past event_time")
    f.add_argument(
        "--late-seconds", type=float, default=120.0, help="how far into the past late records move"
    )
    f.add_argument("--burst-every", type=int, default=0, help="every Nth batch starts a burst")
    f.add_argument("--burst-size", type=int, default=0, help="batches released together in a burst")
    f.add_argument("--jitter-ms", type=float, default=0.0, help="random extra delay before each request")
    return p


def main(argv: list[str] | None = None) -> int:
    args = build_parser().parse_args(argv)
    try:
        if args.command == "generate":
            cfg = GenerateConfig(
                seed=args.seed,
                devices=args.devices,
                duration_s=args.duration,
                interval_s=args.interval,
                start=args.start,
                anomaly_rate=args.anomaly_rate,
                site_id=args.site_id,
            )
            n = write_jsonl(args.out, generate(cfg))
            print(f"wrote {n} events to {args.out} (seed={cfg.seed}, devices={cfg.devices})")
            return 0

        records, skipped = read_jsonl(args.file)
        if not records:
            print(f"no readable records in {args.file}", file=sys.stderr)
            return 2
        cfg = ReplayConfig(
            url=args.url,
            rate=args.rate,
            speed=args.speed,
            batch_size=args.batch_size,
            concurrency=args.concurrency,
            retries=args.retries,
            timeout_s=args.timeout,
            rebase_time=args.rebase_time,
            faults=FaultConfig(
                seed=args.seed,
                malformed_rate=args.malformed_rate,
                duplicate_rate=args.duplicate_rate,
                late_rate=args.late_rate,
                late_seconds=args.late_seconds,
                burst_every=args.burst_every,
                burst_size=args.burst_size,
                jitter_ms=args.jitter_ms,
            ),
        )
        summary = run(records, cfg, skipped)
        print(json.dumps(summary.to_dict(), indent=2) if args.json else summary.format())
        return 1 if summary.request_errors else 0
    except ValueError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2
    except OSError as e:
        print(f"error: {e}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
