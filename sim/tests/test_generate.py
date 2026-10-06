import json

from conftest import EXPECTED, SAMPLE

from signallab_sim.generate import GenerateConfig, generate, read_jsonl, write_jsonl

TEMP_ALERT, VIB_ALERT = 85.0, 7.1


def test_same_seed_gives_identical_output():
    cfg = GenerateConfig(seed=11, devices=4, duration_s=30)
    assert list(generate(cfg)) == list(generate(cfg))


def test_different_seed_changes_output():
    a = list(generate(GenerateConfig(seed=1, duration_s=20)))
    b = list(generate(GenerateConfig(seed=2, duration_s=20)))
    assert a != b


def test_shape_ordering_and_unique_ids():
    events = list(generate(GenerateConfig(seed=3, devices=3, duration_s=10, interval_s=2.0)))
    assert len(events) == 3 * 5
    assert len({e["event_id"] for e in events}) == len(events)
    times = [e["event_time"] for e in events]
    assert times == sorted(times)
    first = events[0]
    assert first["schema_version"] == 1 and first["sequence"] == 1
    assert first["event_time"].endswith("Z") and first["site_id"] == "plant-a"


def test_without_anomalies_nothing_crosses_default_thresholds():
    events = generate(GenerateConfig(seed=5, devices=5, duration_s=300, anomaly_rate=0.0))
    assert all(e["temperature_c"] < TEMP_ALERT and e["vibration_mm_s"] < VIB_ALERT for e in events)


def test_anomaly_episodes_cross_thresholds():
    events = list(generate(GenerateConfig(seed=5, devices=2, duration_s=100, anomaly_rate=1.0)))
    assert all(e["temperature_c"] >= TEMP_ALERT or e["vibration_mm_s"] >= VIB_ALERT for e in events)


def test_jsonl_roundtrip_and_garbage_is_skipped(tmp_path):
    events = list(generate(GenerateConfig(seed=9, duration_s=5)))
    path = tmp_path / "x.jsonl"
    assert write_jsonl(path, events) == len(events)
    with open(path, "a", encoding="utf-8") as f:
        f.write("not json\n[1,2]\n\n")
    records, skipped = read_jsonl(path)
    assert records == events
    assert skipped == 2


def test_committed_sample_matches_its_expected_alerts():
    records, skipped = read_jsonl(SAMPLE)
    expected = json.loads(EXPECTED.read_text())
    assert skipped == 0 and len(records) == expected["events"] == 30
    derived = set()
    for r in records:
        if r["temperature_c"] >= TEMP_ALERT:
            derived.add(f"{r['event_id']}:temperature_high")
        if r["vibration_mm_s"] >= VIB_ALERT:
            derived.add(f"{r['event_id']}:vibration_high")
    assert derived == {a["alert_id"] for a in expected["alerts"]}
    assert len(expected["alerts"]) == 6
