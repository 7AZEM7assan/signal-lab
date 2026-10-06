package event

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

var (
	now = time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)
	lim = Limits{TempMinC: -50, TempMaxC: 250, VibMaxMMS: 100, MaxFutureSkew: 5 * time.Minute}
)

func record(mutate func(map[string]any)) []byte {
	m := map[string]any{
		"schema_version": 1, "event_id": "evt-1", "device_id": "press-01",
		"event_time": "2025-01-15T08:00:00Z", "sequence": 1, "temperature_c": 60.5, "vibration_mm_s": 2.5, "site_id": "plant-a",
	}
	if mutate != nil {
		mutate(m)
	}
	b, _ := json.Marshal(m)
	return b
}

func TestParseValid(t *testing.T) {
	ev, rej := Parse(record(nil), lim, now)
	if rej != nil {
		t.Fatalf("unexpected rejection: %v", rej)
	}
	if ev.EventID != "evt-1" || ev.DeviceID != "press-01" || ev.SiteID != "plant-a" || ev.Sequence == nil || *ev.Sequence != 1 {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if !ev.ReceivedAt.Equal(now) {
		t.Fatalf("received_at must be the server time, got %v", ev.ReceivedAt)
	}
}

func TestParseRejections(t *testing.T) {
	del := func(k string) func(map[string]any) { return func(m map[string]any) { delete(m, k) } }
	set := func(k string, v any) func(map[string]any) { return func(m map[string]any) { m[k] = v } }
	cases := []struct {
		name   string
		mutate func(map[string]any)
		reason string
	}{
		{"missing schema_version", del("schema_version"), ReasonMissingField},
		{"missing event_id", del("event_id"), ReasonMissingField},
		{"missing device_id", del("device_id"), ReasonMissingField},
		{"missing event_time", del("event_time"), ReasonMissingField},
		{"missing temperature", del("temperature_c"), ReasonMissingField},
		{"missing vibration", del("vibration_mm_s"), ReasonMissingField},
		{"unsupported schema", set("schema_version", 2), ReasonUnsupportedSchema},
		{"empty event id", set("event_id", ""), ReasonInvalidEventID},
		{"event id with space", set("event_id", "a b"), ReasonInvalidEventID},
		{"event id too long", set("event_id", strings.Repeat("a", 65)), ReasonInvalidEventID},
		{"event id leading dash", set("event_id", "-x"), ReasonInvalidEventID},
		{"bad device id", set("device_id", "press/01"), ReasonInvalidDeviceID},
		{"bad site id", set("site_id", "plant a"), ReasonInvalidSiteID},
		{"non-RFC3339 time", set("event_time", "2025-01-15 08:00:00"), ReasonInvalidTimestamp},
		{"garbage time", set("event_time", "yesterday"), ReasonInvalidTimestamp},
		{"future beyond skew", set("event_time", now.Add(6*time.Minute).Format(time.RFC3339)), ReasonTimeInFuture},
		{"temperature below min", set("temperature_c", -50.01), ReasonOutOfRange},
		{"temperature above max", set("temperature_c", 250.01), ReasonOutOfRange},
		{"vibration negative", set("vibration_mm_s", -0.01), ReasonOutOfRange},
		{"vibration above max", set("vibration_mm_s", 100.01), ReasonOutOfRange},
		{"negative sequence", set("sequence", -1), ReasonInvalidSequence},
		{"client-supplied received_at", set("received_at", "2025-01-15T08:00:00Z"), ReasonUnknownField},
		{"temperature wrong type", set("temperature_c", "hot"), ReasonMalformedJSON},
		{"sequence is fractional", set("sequence", 1.5), ReasonMalformedJSON},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, rej := Parse(record(tc.mutate), lim, now)
			if rej == nil {
				t.Fatalf("expected rejection %q, got none", tc.reason)
			}
			if rej.Reason != tc.reason {
				t.Fatalf("reason = %q (%s), want %q", rej.Reason, rej.Detail, tc.reason)
			}
		})
	}
}

func TestParseNotAnObject(t *testing.T) {
	for _, raw := range []string{`[1,2]`, `"text"`, `42`, `{`} {
		if _, rej := Parse([]byte(raw), lim, now); rej == nil || rej.Reason != ReasonMalformedJSON {
			t.Errorf("%s: expected malformed_json, got %v", raw, rej)
		}
	}
}

func TestParseBoundaryValuesAreAccepted(t *testing.T) {
	for _, tc := range []struct{ temp, vib float64 }{{-50, 0}, {250, 100}, {85, 7.1}} {
		_, rej := Parse(record(func(m map[string]any) { m["temperature_c"], m["vibration_mm_s"] = tc.temp, tc.vib }), lim, now)
		if rej != nil {
			t.Errorf("temp=%v vib=%v: unexpected rejection %v", tc.temp, tc.vib, rej)
		}
	}
	// Exactly at the allowed skew is still accepted.
	if _, rej := Parse(record(func(m map[string]any) { m["event_time"] = now.Add(5 * time.Minute).Format(time.RFC3339) }), lim, now); rej != nil {
		t.Errorf("event at exactly max skew should be accepted: %v", rej)
	}
}

func TestNonFiniteValuesRejected(t *testing.T) {
	// JSON cannot carry NaN/Inf, but Validate is also reachable from other callers.
	temp, vib := math.NaN(), 1.0
	id, dev, ts, v := "e", "d", "2025-01-15T08:00:00Z", 1
	w := Wire{SchemaVersion: &v, EventID: &id, DeviceID: &dev, EventTime: &ts, TemperatureC: &temp, VibrationMMS: &vib}
	if _, rej := Validate(w, lim, now); rej == nil || rej.Reason != ReasonOutOfRange {
		t.Fatalf("NaN: got %v", rej)
	}
	temp, vib = 20, math.Inf(1)
	if _, rej := Validate(w, lim, now); rej == nil || rej.Reason != ReasonOutOfRange {
		t.Fatalf("+Inf: got %v", rej)
	}
}

func TestTimestampsAreNormalisedToUTCMicroseconds(t *testing.T) {
	ev, rej := Parse(record(func(m map[string]any) { m["event_time"] = "2025-01-15T10:00:00.123456789+02:00" }), lim, now)
	if rej != nil {
		t.Fatal(rej)
	}
	want := time.Date(2025, 1, 15, 8, 0, 0, 123456000, time.UTC)
	if !ev.EventTime.Equal(want) || ev.EventTime.Location() != time.UTC {
		t.Fatalf("event_time = %v, want %v", ev.EventTime, want)
	}
}

func TestRejectionDetailNeverEchoesPayload(t *testing.T) {
	secret := "SENSITIVE-VALUE-123"
	_, rej := Parse(record(func(m map[string]any) { m["event_id"] = secret + " with spaces" }), lim, now)
	if rej == nil {
		t.Fatal("expected rejection")
	}
	if strings.Contains(rej.Detail, secret) {
		t.Fatalf("detail echoes payload: %q", rej.Detail)
	}
}

func TestBatchDeduper(t *testing.T) {
	seq := func(n int64) *int64 { return &n }
	d := NewBatchDeduper()
	a := Event{EventID: "a", DeviceID: "d1", Sequence: seq(1)}
	if rej := d.Check(a); rej != nil {
		t.Fatal(rej)
	}
	if rej := d.Check(Event{EventID: "a", DeviceID: "d2"}); rej == nil || rej.Reason != ReasonDuplicateInBatch {
		t.Fatalf("repeated event_id must be rejected, got %v", rej)
	}
	if rej := d.Check(Event{EventID: "b", DeviceID: "d1", Sequence: seq(1)}); rej == nil || rej.Reason != ReasonDuplicateInBatch {
		t.Fatalf("repeated (device, sequence) must be rejected, got %v", rej)
	}
	if rej := d.Check(Event{EventID: "c", DeviceID: "d2", Sequence: seq(1)}); rej != nil {
		t.Fatalf("same sequence on another device is fine, got %v", rej)
	}
	// A rejected record must not poison later ones: "b" was rejected, so it may appear once more with a new sequence.
	if rej := d.Check(Event{EventID: "b", DeviceID: "d1", Sequence: seq(2)}); rej != nil {
		t.Fatalf("got %v", rej)
	}
}

func TestLag(t *testing.T) {
	et := time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	if got := Lag(et, et.Add(1500*time.Millisecond)); got != 1500*time.Millisecond {
		t.Fatalf("lag = %v", got)
	}
	if got := Lag(et, et.Add(-time.Second)); got != -time.Second {
		t.Fatalf("source clock ahead of server must give negative lag, got %v", got)
	}
}
