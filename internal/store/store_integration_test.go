package store_test

import (
	"context"
	"testing"
	"time"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/metrics"
	"signallab/internal/store"
	"signallab/internal/testdb"
)

var (
	t0 = time.Date(2025, 1, 15, 8, 0, 0, 0, time.UTC)
	th = alert.Thresholds{TemperatureC: 85, VibrationMMS: 7.1}
)

func ev(id, device string, sec int, seq int64, temp, vib float64) event.Event {
	s := seq
	e := event.Event{SchemaVersion: 1, EventID: id, DeviceID: device, EventTime: t0.Add(time.Duration(sec) * time.Second),
		ReceivedAt: t0.Add(time.Hour), TemperatureC: temp, VibrationMMS: vib, SiteID: "plant-a"}
	if seq >= 0 {
		e.Sequence = &s
	}
	return e
}

func newStore(t *testing.T) *store.Store {
	return store.New(testdb.New(t), 5*time.Second, metrics.New())
}

func TestMigrateIsIdempotent(t *testing.T) {
	pool := testdb.New(t) // already migrated once
	applied, err := store.Migrate(context.Background(), pool)
	if err != nil {
		t.Fatal(err)
	}
	if len(applied) != 0 {
		t.Fatalf("second run must apply nothing, applied %v", applied)
	}
}

func TestPersistStoresEventsAndAlerts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	res, err := s.PersistBatch(ctx, []event.Event{
		ev("e1", "press-01", 0, 1, 60, 2),
		ev("e2", "press-01", 1, 2, 85, 2),   // boundary: temperature alert
		ev("e3", "press-01", 2, 3, 90, 7.1), // both rules
	}, th)
	if err != nil || res.AlertErr != nil {
		t.Fatalf("err=%v alertErr=%v", err, res.AlertErr)
	}
	if len(res.Inserted) != 3 || len(res.Alerts) != 3 {
		t.Fatalf("inserted=%d alerts=%d, want 3 and 3", len(res.Inserted), len(res.Alerts))
	}

	evs, next, err := s.QueryEvents(ctx, store.Query{DeviceID: "press-01", From: t0, To: t0.Add(time.Minute), Limit: 10})
	if err != nil || next != nil || len(evs) != 3 {
		t.Fatalf("events: %d next=%v err=%v", len(evs), next, err)
	}
	if evs[0].EventID != "e1" || evs[0].SiteID != "plant-a" || *evs[0].Sequence != 1 || !evs[0].EventTime.Equal(t0) {
		t.Fatalf("round trip mismatch: %+v", evs[0])
	}
	alerts, _, err := s.QueryAlerts(ctx, store.Query{From: t0, To: t0.Add(time.Minute), Limit: 10})
	if err != nil || len(alerts) != 3 {
		t.Fatalf("alerts: %d err=%v", len(alerts), err)
	}
}

func TestDuplicateEventIDIsSkippedNotReAlerted(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	first, err := s.PersistBatch(ctx, []event.Event{ev("dup", "d1", 0, 1, 99, 0)}, th)
	if err != nil || len(first.Inserted) != 1 || len(first.Alerts) != 1 {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := s.PersistBatch(ctx, []event.Event{ev("dup", "d1", 0, 1, 99, 0)}, th)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Inserted) != 0 || len(second.Alerts) != 0 {
		t.Fatalf("a replayed event must not be re-inserted or re-alerted: %+v", second)
	}
	alerts, _, _ := s.QueryAlerts(ctx, store.Query{From: t0, To: t0.Add(time.Minute), Limit: 10})
	if len(alerts) != 1 {
		t.Fatalf("exactly one alert expected, got %d", len(alerts))
	}
}

func TestDuplicateDeviceSequenceWithNewEventIDIsSkipped(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	if _, err := s.PersistBatch(ctx, []event.Event{ev("a", "d1", 0, 7, 20, 1)}, th); err != nil {
		t.Fatal(err)
	}
	res, err := s.PersistBatch(ctx, []event.Event{ev("b", "d1", 1, 7, 20, 1), ev("c", "d2", 1, 7, 20, 1)}, th)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Inserted) != 1 || res.Inserted[0].EventID != "c" {
		t.Fatalf("only the other device's sequence 7 should insert, got %+v", res.Inserted)
	}
}

func TestEventsWithoutSequenceAreNotConstrained(t *testing.T) {
	s := newStore(t)
	res, err := s.PersistBatch(context.Background(), []event.Event{ev("a", "d1", 0, -1, 20, 1), ev("b", "d1", 1, -1, 20, 1)}, th)
	if err != nil || len(res.Inserted) != 2 {
		t.Fatalf("inserted=%d err=%v", len(res.Inserted), err)
	}
}

func TestSameEventIDTwiceInOneBatchInsertsOnce(t *testing.T) {
	s := newStore(t)
	res, err := s.PersistBatch(context.Background(), []event.Event{ev("x", "d1", 0, -1, 99, 0), ev("x", "d1", 0, -1, 99, 0)}, th)
	if err != nil || len(res.Inserted) != 1 || len(res.Alerts) != 1 {
		t.Fatalf("inserted=%d alerts=%d err=%v", len(res.Inserted), len(res.Alerts), err)
	}
}

func TestAlertFailureKeepsEventsCommitted(t *testing.T) {
	// Force the alert statement to fail by dropping its table inside this isolated schema.
	pool := testdb.New(t)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `DROP TABLE alerts`); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool, 5*time.Second, metrics.New())
	res, err := s.PersistBatch(ctx, []event.Event{ev("e1", "d1", 0, 1, 99, 0)}, th)
	if err != nil {
		t.Fatalf("event persistence must not fail because alerts failed: %v", err)
	}
	if res.AlertErr == nil || len(res.Inserted) != 1 || len(res.Alerts) != 0 {
		t.Fatalf("expected AlertErr with the event stored: %+v", res)
	}
	evs, _, err := s.QueryEvents(ctx, store.Query{From: t0, To: t0.Add(time.Minute), Limit: 10})
	if err != nil || len(evs) != 1 {
		t.Fatalf("event must be committed: %d %v", len(evs), err)
	}
}

func TestKeysetPaginationAndFilters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	var batch []event.Event
	for i := 0; i < 7; i++ {
		batch = append(batch, ev("p"+string(rune('0'+i)), "d1", i, int64(i), 20, 1))
	}
	batch = append(batch, ev("other", "d2", 3, 0, 20, 1))
	if _, err := s.PersistBatch(ctx, batch, th); err != nil {
		t.Fatal(err)
	}

	var seen []string
	var after *store.Page
	for pages := 0; pages < 10; pages++ {
		got, next, err := s.QueryEvents(ctx, store.Query{DeviceID: "d1", From: t0, To: t0.Add(time.Minute), Limit: 3, After: after})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range got {
			seen = append(seen, e.EventID)
		}
		if next == nil {
			break
		}
		cursor := store.EncodeCursor(*next)
		if after, err = store.DecodeCursor(cursor); err != nil {
			t.Fatal(err)
		}
	}
	if len(seen) != 7 || seen[0] != "p0" || seen[6] != "p6" {
		t.Fatalf("pagination should walk all 7 d1 events in order, got %v", seen)
	}

	// Half-open range: [t0+2s, t0+4s) holds seconds 2 and 3 for all devices.
	got, _, _ := s.QueryEvents(ctx, store.Query{From: t0.Add(2 * time.Second), To: t0.Add(4 * time.Second), Limit: 50})
	if len(got) != 3 { // p2, p3, other(3)
		t.Fatalf("range filter: got %d events, want 3", len(got))
	}
}

func TestCursorRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "***", "bm9waXBl"} { // "nopipe"
		if _, err := store.DecodeCursor(bad); err == nil {
			t.Errorf("cursor %q should be rejected", bad)
		}
	}
}

func TestCanceledContextFailsFastWithoutPanic(t *testing.T) {
	s := newStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.PersistBatch(ctx, []event.Event{ev("e1", "d1", 0, 1, 20, 1)}, th)
	if err == nil {
		t.Fatal("expected an error for a cancelled context")
	}
}
