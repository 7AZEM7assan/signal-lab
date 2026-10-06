"""SIL: HTTP contract of the real service (status codes, limits, probes, metrics)."""

import json

import pytest

pytestmark = pytest.mark.sil

GOOD = {
    "schema_version": 1,
    "event_id": "api-1",
    "device_id": "press-01",
    "event_time": "2025-01-15T08:00:00Z",
    "temperature_c": 60.0,
    "vibration_mm_s": 2.0,
}


def post(stack, body: bytes | dict):
    data = body if isinstance(body, bytes) else json.dumps(body).encode()
    status, raw, headers = stack.request("/api/v1/events", data=data)
    return status, json.loads(raw), headers


def test_probes_and_metrics(service):
    assert service.request("/healthz")[0] == 200
    status, body, _ = service.request("/readyz")
    assert status == 200 and json.loads(body)["checks"]["database"] == "ok"
    text = service.request("/metrics")[1].decode()
    for name in (
        "signallab_queue_depth",
        "signallab_queue_capacity",
        "signallab_ingest_events_total",
        "signallab_validation_failures_total",
        "signallab_processed_events_total",
        "signallab_queue_wait_seconds_bucket",
        "signallab_processing_duration_seconds_bucket",
        "signallab_ws_clients",
        "signallab_db_operation_duration_seconds_bucket",
    ):
        assert name in text, name
    assert service.metric("signallab_queue_capacity") == 1000


def test_batch_response_reports_per_record_outcomes(service):
    status, out, headers = post(
        service, {"events": [GOOD, {**GOOD, "event_id": "api-2", "temperature_c": 9999}]}
    )
    assert status == 202
    assert (out["received"], out["accepted"], out["rejected"]) == (2, 1, 1)
    assert out["rejections"][0]["index"] == 1 and out["rejections"][0]["reason"] == "out_of_range"
    assert out["batch_id"] == headers["X-Request-Id"]
    assert out["queue"]["capacity"] == 1000


def test_invalid_requests_get_specific_errors(service):
    assert post(service, b"{not json")[0] == 400
    assert post(service, {"events": []})[0] == 400
    status, out, _ = post(service, {"events": [GOOD] * 501})
    assert status == 413 and out["error"]["code"] == "batch_too_large"
    status, out, _ = post(service, b'{"events":[' + b" " * (2 << 20) + b"]}")
    assert status == 413 and out["error"]["code"] == "payload_too_large"
    assert service.count_events() == 0


def test_query_endpoints_validate_their_parameters(service):
    for path in (
        "/api/v1/events",  # from/to are required
        "/api/v1/events?from=2025-01-15T00:00:00Z&to=2025-03-15T00:00:00Z",  # wider than the 24h maximum
        "/api/v1/alerts?from=2025-01-15T00:00:00Z&to=2025-01-15T01:00:00Z&limit=100000",
    ):
        assert service.request(path)[0] == 400, path
