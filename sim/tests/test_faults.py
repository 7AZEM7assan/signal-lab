import json

import pytest
from conftest import SAMPLE

from signallab_sim.faults import FaultConfig, apply_bursts, plan_records
from signallab_sim.generate import parse_time, read_jsonl


@pytest.fixture(scope="module")
def records():
    return read_jsonl(SAMPLE)[0]


def test_no_faults_means_unchanged_records(records):
    planned, counts = plan_records(records, FaultConfig())
    assert [json.loads(p.body) for p in planned] == records
    assert (counts.malformed, counts.duplicates, counts.late) == (0, 0, 0)
    assert {p.kind for p in planned} == {"ok"}


def test_same_seed_same_plan_and_other_seed_differs(records):
    cfg = FaultConfig(seed=7, malformed_rate=0.2, duplicate_rate=0.2, late_rate=0.2)
    a, ca = plan_records(records, cfg)
    b, cb = plan_records(records, cfg)
    assert [p.body for p in a] == [p.body for p in b] and ca == cb
    c, _ = plan_records(records, FaultConfig(seed=8, malformed_rate=0.2, duplicate_rate=0.2, late_rate=0.2))
    assert [p.body for p in a] != [p.body for p in c]


def test_counts_match_planned_kinds(records):
    cfg = FaultConfig(seed=3, malformed_rate=0.3, duplicate_rate=0.3, late_rate=0.3)
    planned, counts = plan_records(records, cfg)
    kinds = [p.kind for p in planned]
    assert kinds.count("malformed") == counts.malformed == sum(counts.malformed_by_kind.values())
    assert kinds.count("duplicate") == counts.duplicates
    assert len(planned) == len(records) + counts.duplicates


def test_duplicates_are_byte_identical_and_adjacent(records):
    planned, _ = plan_records(records, FaultConfig(seed=1, duplicate_rate=1.0))
    assert len(planned) == 2 * len(records)
    for orig, dup in zip(planned[0::2], planned[1::2], strict=True):
        assert dup.kind == "duplicate" and dup.body == orig.body


def test_late_records_move_event_time_back_only(records):
    planned, counts = plan_records(records, FaultConfig(seed=2, late_rate=1.0, late_seconds=90))
    assert counts.late == len(records)
    for rec, p in zip(records, planned, strict=True):
        shifted = json.loads(p.body)
        delta = parse_time(rec["event_time"]) - parse_time(shifted["event_time"])
        assert delta.total_seconds() == 90
        assert shifted["event_id"] == rec["event_id"]


def test_malformed_variants_are_actually_invalid(records):
    planned, _ = plan_records(records, FaultConfig(seed=4, malformed_rate=1.0))
    seen = set()
    for rec, p in zip(records, planned, strict=True):
        bad = json.loads(p.body)
        assert bad != rec
        seen.add(tuple(sorted(k for k in bad if bad[k] != rec.get(k)) or ["missing"]))
    assert len(seen) > 1  # more than one kind of corruption was used


def test_apply_bursts_releases_batches_together():
    schedule = [0.0, 1.0, 2.0, 3.0, 4.0, 5.0, 6.0]
    out = apply_bursts(schedule, FaultConfig(burst_every=3, burst_size=2))
    assert out == [0.0, 1.0, 2.0, 3.0, 3.0, 5.0, 6.0]  # batches 3 and 4 are released together
    assert apply_bursts(schedule, FaultConfig()) == schedule


@pytest.mark.parametrize(
    "kwargs",
    [{"malformed_rate": 1.5}, {"duplicate_rate": -0.1}, {"late_seconds": -1}, {"burst_size": -2}],
)
def test_invalid_configs_are_rejected(kwargs):
    with pytest.raises(ValueError):
        FaultConfig(**kwargs).validate()
