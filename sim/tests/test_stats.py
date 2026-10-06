from signallab_sim.stats import latency_summary, percentile


def test_percentile_nearest_rank():
    data = list(range(1, 101))  # 1..100
    assert percentile(data, 50) == 50
    assert percentile(data, 95) == 95
    assert percentile(data, 99) == 99
    assert percentile(data, 100) == 100
    assert percentile([7.0], 99) == 7.0
    assert percentile([], 50) == 0.0


def test_latency_summary_reports_milliseconds():
    s = latency_summary([0.001, 0.002, 0.003, 0.004])
    assert s["count"] == 4 and s["p50_ms"] == 2.0 and s["max_ms"] == 4.0 and s["mean_ms"] == 2.5
    assert latency_summary([])["count"] == 0
