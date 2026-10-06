import json
from datetime import UTC, datetime

import pytest
from conftest import SAMPLE

from signallab_sim.cli import main
from signallab_sim.faults import FaultConfig, Planned
from signallab_sim.generate import parse_time, read_jsonl
from signallab_sim.replay import ReplayConfig, build_schedule, rebase, run


@pytest.fixture(scope="module")
def records():
    return read_jsonl(SAMPLE)[0]


def planned(n, t_step=1.0):
    return [Planned("{}", "ok", i * t_step) for i in range(n)]


def test_batches_and_summary_counts(stub, records):
    s = run(records, ReplayConfig(url=stub.url, batch_size=7))
    assert [len(r) for r in stub.requests] == [7, 7, 7, 7, 2]
    assert (s.attempted, s.batches, s.accepted, s.rejected) == (30, 5, 30, 0)
    assert s.request_errors == 0 and s.overloaded_records == 0
    assert s.latency["count"] == 5 and s.elapsed_s > 0


def test_injected_malformed_records_are_reported_as_rejected(stub, records):
    cfg = ReplayConfig(url=stub.url, batch_size=10, faults=FaultConfig(seed=7, malformed_rate=0.2))
    s = run(records, cfg)
    wrongly_typed = s.injected["malformed"]
    assert wrongly_typed > 0
    assert 0 < s.rejected <= wrongly_typed  # the stub only rejects the missing/wrong-type variants
    assert s.injected["duplicates"] == 0
    assert s.faults["seed"] == 7  # the seed is always echoed for reproducibility


def test_rate_schedule_is_events_over_rate():
    batches, sched = build_schedule(planned(25), ReplayConfig(rate=100, batch_size=10))
    assert [len(b) for b in batches] == [10, 10, 5]
    assert sched == [0.0, 0.1, 0.2]


def test_speed_schedule_follows_recorded_time():
    _, sched = build_schedule(planned(20, t_step=2.0), ReplayConfig(speed=4.0, batch_size=5))
    assert sched == [0.0, 2.5, 5.0, 7.5]  # batch starts at t_rel 0,10,20,30 divided by speed 4


def test_unpaced_schedule_is_all_zero():
    _, sched = build_schedule(planned(30), ReplayConfig(batch_size=10))
    assert sched == [0.0, 0.0, 0.0]


def test_rate_and_speed_are_mutually_exclusive():
    with pytest.raises(ValueError):
        ReplayConfig(rate=10, speed=2).validate()


def test_retry_after_429_then_success(stub, records):
    stub.throttle_first = 2
    s = run(records[:10], ReplayConfig(url=stub.url, batch_size=10, retries=3))
    assert s.throttled_responses == 2 and s.retries_used == 2
    assert s.accepted == 10 and s.overloaded_records == 0
    assert len(stub.requests) == 3
    assert s.latency["count"] == 3  # latency is measured per attempt


def test_429_without_retries_is_reported_as_overload(stub, records):
    stub.throttle_first = 1000
    s = run(records[:10], ReplayConfig(url=stub.url, batch_size=5, retries=0))
    assert s.accepted == 0 and s.overloaded_records == 10 and s.throttled_responses == 2


def test_connection_failure_counts_request_errors(records):
    s = run(records[:10], ReplayConfig(url="http://127.0.0.1:1", batch_size=5, timeout_s=1))
    assert s.request_errors == 2 and s.errored_records == 10 and s.accepted == 0


def test_concurrent_senders_deliver_every_batch_once(stub, records):
    s = run(records, ReplayConfig(url=stub.url, batch_size=3, concurrency=4))
    ids = [e["event_id"] for req in stub.requests for e in req]
    assert sorted(ids) == sorted(r["event_id"] for r in records)
    assert s.accepted == 30


def test_rebase_shifts_all_times_by_one_offset(records):
    now = datetime(2030, 1, 1, tzinfo=UTC)
    shifted = rebase(records, now)
    assert parse_time(shifted[0]["event_time"]) == now
    gap = parse_time(records[-1]["event_time"]) - parse_time(records[0]["event_time"])
    assert parse_time(shifted[-1]["event_time"]) - now == gap
    assert records[0]["event_time"] == "2025-01-15T08:00:00.000Z"  # input is not mutated


def test_cli_generate_then_replay_json(stub, tmp_path, capsys):
    out = tmp_path / "d.jsonl"
    assert main(["generate", "--out", str(out), "--seed", "5", "--duration", "10", "--devices", "2"]) == 0
    assert main(["replay", str(out), "--url", stub.url, "--batch-size", "10", "--json"]) == 0
    summary = json.loads(capsys.readouterr().out.split("\n", 1)[1])
    assert summary["attempted"] == 20 and summary["accepted"] == 20


def test_cli_exit_codes(tmp_path, capsys):
    assert main(["replay", str(tmp_path / "missing.jsonl")]) == 2
    empty = tmp_path / "e.jsonl"
    empty.write_text("")
    assert main(["replay", str(empty)]) == 2
    assert main(["replay", str(SAMPLE), "--url", "http://127.0.0.1:1", "--rate", "0"]) == 2
    capsys.readouterr()
