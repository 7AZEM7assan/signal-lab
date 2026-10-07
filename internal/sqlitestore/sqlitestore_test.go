package sqlitestore

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/metrics"
	"signallab/internal/store"
)

var th = alert.Thresholds{TemperatureC: 85, VibrationMMS: 7.1}

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "data", "signallab.db"), 5*time.Second, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func ev(device string, seq int64, temp, vib float64, at time.Time) event.Event {
	s := seq
	return event.Event{
		SchemaVersion: 1, EventID: fmt.Sprintf("%s-%06d", device, seq), DeviceID: device,
		EventTime: at.UTC(), ReceivedAt: at.UTC().Add(time.Second), Sequence: &s,
		TemperatureC: temp, VibrationMMS: vib, SiteID: "plant-a",
	}
}

var t0 = time.Date(2025, 1, 15, 8, 0, 0, 123456000, time.UTC)

func TestPersistIsIdempotentAndRaisesInclusiveAlerts(t *testing.T) {
	s, ctx := open(t), context.Background()
	batch := []event.Event{
		ev("press-01", 1, 60, 2, t0),
		ev("press-01", 2, 85.0, 2, t0.Add(time.Second)), // exactly at the threshold: fires
		ev("pump-02", 1, 60, 7.1, t0),                   // exactly at the threshold: fires
	}
	res, err := s.PersistBatch(ctx, batch, th)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Inserted) != 3 || len(res.Alerts) != 2 || res.AlertErr != nil {
		t.Fatalf("inserted=%d alerts=%d alertErr=%v", len(res.Inserted), len(res.Alerts), res.AlertErr)
	}
	// Re-sending the same batch inserts nothing and raises nothing.
	res, err = s.PersistBatch(ctx, batch, th)
	if err != nil || len(res.Inserted) != 0 || len(res.Alerts) != 0 {
		t.Fatalf("replay: err=%v inserted=%d alerts=%d", err, len(res.Inserted), len(res.Alerts))
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Events != 3 || st.Alerts != 2 || st.Devices != 2 {
		t.Fatalf("stats %+v err %v", st, err)
	}
}

func TestDuplicateInsideOneBatchCountsOnce(t *testing.T) {
	s := open(t)
	e := ev("mill-03", 1, 90, 1, t0)
	res, err := s.PersistBatch(context.Background(), []event.Event{e, e}, th)
	if err != nil || len(res.Inserted) != 1 || len(res.Alerts) != 1 {
		t.Fatalf("err=%v inserted=%d alerts=%d", err, len(res.Inserted), len(res.Alerts))
	}
}

func TestSequenceUniquenessSkipsConflictingEvent(t *testing.T) {
	s := open(t)
	a := ev("press-01", 5, 60, 2, t0)
	b := ev("press-01", 5, 61, 2, t0.Add(time.Minute))
	b.EventID = "other-id" // different event_id, same (device, sequence)
	res, err := s.PersistBatch(context.Background(), []event.Event{a, b}, th)
	if err != nil || len(res.Inserted) != 1 {
		t.Fatalf("err=%v inserted=%d", err, len(res.Inserted))
	}
}

func TestEventWithoutSequenceOrSiteRoundTrips(t *testing.T) {
	s, ctx := open(t), context.Background()
	e := ev("lathe-04", 1, 50, 1, t0)
	e.Sequence, e.SiteID = nil, ""
	if _, err := s.PersistBatch(ctx, []event.Event{e}, th); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.QueryEvents(ctx, store.Query{From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Limit: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("err=%v got=%d", err, len(got))
	}
	if got[0].Sequence != nil || got[0].SiteID != "" || !got[0].EventTime.Equal(t0) || !got[0].ReceivedAt.Equal(e.ReceivedAt) {
		t.Fatalf("round trip lost data: %+v", got[0])
	}
}

func TestKeysetPaginationCoversEverythingOnce(t *testing.T) {
	s, ctx := open(t), context.Background()
	var batch []event.Event
	for i := int64(1); i <= 25; i++ {
		// Ties on event_time across two devices exercise the (time, id) tiebreak.
		batch = append(batch, ev("press-01", i, 60, 2, t0.Add(time.Duration(i/2)*time.Second)))
		batch = append(batch, ev("pump-02", i, 60, 2, t0.Add(time.Duration(i/2)*time.Second)))
	}
	if _, err := s.PersistBatch(ctx, batch, th); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	q := store.Query{From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Limit: 7}
	pages := 0
	for {
		got, next, err := s.QueryEvents(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		pages++
		for _, e := range got {
			if seen[e.EventID] {
				t.Fatalf("event %s returned twice", e.EventID)
			}
			seen[e.EventID] = true
		}
		if next == nil {
			break
		}
		q.After = next
	}
	if len(seen) != 50 || pages != 8 {
		t.Fatalf("saw %d events in %d pages, want 50 in 8", len(seen), pages)
	}
	// Device filter and half-open range.
	got, _, _ := s.QueryEvents(ctx, store.Query{DeviceID: "pump-02", From: t0, To: t0.Add(time.Second), Limit: 100})
	for _, e := range got {
		if e.DeviceID != "pump-02" || e.EventTime.Before(t0) || !e.EventTime.Before(t0.Add(time.Second)) {
			t.Fatalf("filter leaked %+v", e)
		}
	}
}

func TestQueryAlertsAndClear(t *testing.T) {
	s, ctx := open(t), context.Background()
	if _, err := s.PersistBatch(ctx, []event.Event{ev("press-01", 1, 99, 9, t0), ev("press-01", 2, 20, 1, t0.Add(time.Second))}, th); err != nil {
		t.Fatal(err)
	}
	al, next, err := s.QueryAlerts(ctx, store.Query{From: t0.Add(-time.Hour), To: t0.Add(time.Hour), Limit: 10})
	if err != nil || next != nil || len(al) != 2 {
		t.Fatalf("alerts=%d next=%v err=%v", len(al), next, err)
	}
	if al[0].ID != "press-01-000001:temperature_high" || al[1].ID != "press-01-000001:vibration_high" {
		t.Fatalf("unexpected alert ids/order: %s, %s", al[0].ID, al[1].ID)
	}
	ne, na, err := s.Clear(ctx)
	if err != nil || ne != 2 || na != 2 {
		t.Fatalf("clear removed %d events %d alerts, err %v", ne, na, err)
	}
	st, _ := s.Stats(ctx)
	if st.Events != 0 || st.Alerts != 0 || st.OldestTime != nil {
		t.Fatalf("stats after clear: %+v", st)
	}
	// The store is still usable after a clear.
	if res, err := s.PersistBatch(ctx, []event.Event{ev("press-01", 1, 60, 2, t0)}, th); err != nil || len(res.Inserted) != 1 {
		t.Fatalf("persist after clear: %v", err)
	}
}

func TestDataSurvivesReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	s, err := Open(path, time.Second, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.PersistBatch(context.Background(), []event.Event{ev("press-01", 1, 60, 2, t0)}, th); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2, err := Open(path, time.Second, metrics.New())
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	st, _ := s2.Stats(context.Background())
	if st.Events != 1 || st.DBBytes == 0 || st.Path != path {
		t.Fatalf("after reopen: %+v", st)
	}
}
