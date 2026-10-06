package pipeline

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/metrics"
	"signallab/internal/store"
)

type fakePersister struct {
	mu        sync.Mutex
	seen      map[string]bool
	persisted []event.Event
	calls     int
	failFirst int           // number of leading calls that return an error
	alwaysErr error         // every call fails
	alertErr  error         // returned in Result.AlertErr
	block     chan struct{} // if non-nil, PersistBatch waits for it to close (or for ctx)
}

func (f *fakePersister) PersistBatch(ctx context.Context, evs []event.Event, th alert.Thresholds) (store.Result, error) {
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return store.Result{}, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.alwaysErr != nil {
		return store.Result{}, f.alwaysErr
	}
	if f.calls <= f.failFirst {
		return store.Result{}, errors.New("transient")
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	var res store.Result
	for _, ev := range evs { // mimic ON CONFLICT DO NOTHING on event_id
		if f.seen[ev.EventID] {
			continue
		}
		f.seen[ev.EventID] = true
		res.Inserted = append(res.Inserted, ev)
		f.persisted = append(f.persisted, ev)
		if f.alertErr == nil {
			res.Alerts = append(res.Alerts, th.Evaluate(ev, time.Now())...)
		}
	}
	res.AlertErr = f.alertErr
	return res, nil
}

func (f *fakePersister) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.persisted)
}

type fakeBroadcaster struct {
	mu   sync.Mutex
	msgs []string
}

func (b *fakeBroadcaster) Broadcast(_ string, payload []byte) {
	b.mu.Lock()
	b.msgs = append(b.msgs, string(payload))
	b.mu.Unlock()
}

func (b *fakeBroadcaster) len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.msgs)
}

func evs(prefix string, n int, temp float64) []event.Event {
	out := make([]event.Event, n)
	for i := range out {
		out[i] = event.Event{EventID: prefix + string(rune('a'+i)), DeviceID: "d1", TemperatureC: temp,
			EventTime: time.Now(), ReceivedAt: time.Now()}
	}
	return out
}

func newPipe(t *testing.T, cfg Config, db Persister) (*Pipeline, *fakeBroadcaster, *metrics.Metrics) {
	t.Helper()
	if cfg.Workers == 0 {
		cfg.Workers = 1
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = 10
	}
	if cfg.Attempts == 0 {
		cfg.Attempts = 1
	}
	if cfg.Thresholds == (alert.Thresholds{}) {
		cfg.Thresholds = alert.Thresholds{TemperatureC: 85, VibrationMMS: 7.1}
	}
	bc := &fakeBroadcaster{}
	m := metrics.New()
	p := New(cfg, db, bc, m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return p, bc, m
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func closeOK(t *testing.T, p *Pipeline) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestEnqueueIsAllOrNothingAndBounded(t *testing.T) {
	db := &fakePersister{block: make(chan struct{})}
	p, _, _ := newPipe(t, Config{Capacity: 5, BatchSize: 1}, db)

	if err := p.Enqueue("b0", evs("w", 1, 20)); err != nil { // the single worker takes this and blocks in the persister
		t.Fatal(err)
	}
	eventually(t, func() bool { return p.Depth() == 0 }, "worker to take the first event")

	if err := p.Enqueue("b1", evs("x", 3, 20)); err != nil {
		t.Fatal(err)
	}
	if err := p.Enqueue("b2", evs("y", 3, 20)); !errors.Is(err, ErrQueueFull) { // only 2 slots free
		t.Fatalf("expected ErrQueueFull, got %v", err)
	}
	if p.Depth() != 3 {
		t.Fatalf("a rejected batch must leave the queue untouched: depth=%d, want 3", p.Depth())
	}
	if err := p.Enqueue("b3", evs("z", 2, 20)); err != nil {
		t.Fatalf("a batch that exactly fits must be accepted: %v", err)
	}
	if err := p.Enqueue("b4", evs("v", 1, 20)); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full queue must reject, got %v", err)
	}

	close(db.block)
	closeOK(t, p)
	if db.count() != 6 {
		t.Fatalf("persisted %d events, want 6 (1 in flight + 3 + 2)", db.count())
	}
}

func TestQueueNeverExceedsCapacityUnderConcurrency(t *testing.T) {
	db := &fakePersister{block: make(chan struct{})}
	p, _, _ := newPipe(t, Config{Capacity: 20, BatchSize: 1}, db)
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				batch := evs(string(rune('A'+g))+string(rune('a'+i)), 3, 20)
				if p.Enqueue("b", batch) == nil {
					mu.Lock()
					accepted += len(batch)
					mu.Unlock()
				}
				if p.Depth() > p.Capacity() {
					t.Errorf("depth %d exceeds capacity %d", p.Depth(), p.Capacity())
				}
			}
		}(g)
	}
	wg.Wait()
	close(db.block)
	closeOK(t, p)
	if db.count() != accepted {
		t.Fatalf("every accepted event must be persisted: accepted=%d persisted=%d", accepted, db.count())
	}
}

func TestCloseDrainsQueueThenRejects(t *testing.T) {
	db := &fakePersister{}
	p, bc, m := newPipe(t, Config{Capacity: 50, BatchSize: 4}, db)
	if err := p.Enqueue("b", evs("e", 10, 20)); err != nil {
		t.Fatal(err)
	}
	closeOK(t, p)
	if db.count() != 10 {
		t.Fatalf("drain should persist all 10, got %d", db.count())
	}
	if bc.len() != 10 {
		t.Fatalf("expected 10 event broadcasts, got %d", bc.len())
	}
	if got := testutil.ToFloat64(m.ProcessedEvents.WithLabelValues(metrics.ProcessedStored)); got != 10 {
		t.Fatalf("stored metric = %v", got)
	}
	if err := p.Enqueue("late", evs("l", 1, 20)); !errors.Is(err, ErrClosed) {
		t.Fatalf("expected ErrClosed after Close, got %v", err)
	}
	closeOK(t, p) // Close is idempotent
}

func TestRetryThenSuccess(t *testing.T) {
	db := &fakePersister{failFirst: 2}
	p, _, m := newPipe(t, Config{Capacity: 10, Attempts: 3, Backoff: time.Millisecond}, db)
	if err := p.Enqueue("b", evs("e", 2, 20)); err != nil {
		t.Fatal(err)
	}
	closeOK(t, p)
	if db.count() != 2 {
		t.Fatalf("events should be stored after retries, got %d", db.count())
	}
	if got := testutil.ToFloat64(m.ProcessingFailures.WithLabelValues(metrics.StagePersist)); got != 2 {
		t.Fatalf("persist failure metric = %v, want 2", got)
	}
}

func TestGiveUpAfterAttemptsIsCountedNotSilent(t *testing.T) {
	db := &fakePersister{alwaysErr: errors.New("db down")}
	p, bc, m := newPipe(t, Config{Capacity: 10, Attempts: 2, Backoff: time.Millisecond}, db)
	if err := p.Enqueue("b", evs("e", 3, 99)); err != nil {
		t.Fatal(err)
	}
	closeOK(t, p)
	if got := testutil.ToFloat64(m.ProcessedEvents.WithLabelValues(metrics.ProcessedFailed)); got != 3 {
		t.Fatalf("failed metric = %v, want 3", got)
	}
	if bc.len() != 0 {
		t.Fatal("nothing must be broadcast for events that were not stored")
	}
}

func TestDuplicatesAreNotRebroadcastOrRealerted(t *testing.T) {
	db := &fakePersister{}
	p, bc, m := newPipe(t, Config{Capacity: 10}, db)
	hot := evs("same", 1, 90) // fires temperature_high
	if err := p.Enqueue("b1", hot); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return bc.len() == 2 }, "first event + alert broadcast")
	if err := p.Enqueue("b2", hot); err != nil { // a client retry of the same event
		t.Fatal(err)
	}
	closeOK(t, p)
	if bc.len() != 2 {
		t.Fatalf("duplicate must not produce more broadcasts, got %d messages", bc.len())
	}
	if got := testutil.ToFloat64(m.ProcessedEvents.WithLabelValues(metrics.ProcessedDuplicate)); got != 1 {
		t.Fatalf("duplicate metric = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.AlertsCreated.WithLabelValues(string(alert.RuleTemperatureHigh))); got != 1 {
		t.Fatalf("alerts created = %v, want 1", got)
	}
}

func TestAlertPersistFailureDoesNotBlockEvents(t *testing.T) {
	db := &fakePersister{alertErr: errors.New("alert insert failed")}
	p, bc, m := newPipe(t, Config{Capacity: 10}, db)
	if err := p.Enqueue("b", evs("e", 1, 99)); err != nil {
		t.Fatal(err)
	}
	closeOK(t, p)
	if db.count() != 1 || bc.len() != 1 {
		t.Fatalf("event must still be stored and broadcast: stored=%d broadcasts=%d", db.count(), bc.len())
	}
	if got := testutil.ToFloat64(m.ProcessingFailures.WithLabelValues(metrics.StageAlertPersist)); got != 1 {
		t.Fatalf("alert_persist failure metric = %v, want 1", got)
	}
}

func TestCloseDeadlineAbandonsStuckWorkAndCountsIt(t *testing.T) {
	db := &fakePersister{block: make(chan struct{})} // never released: simulates a hung database
	p, _, m := newPipe(t, Config{Capacity: 10, BatchSize: 1}, db)
	if err := p.Enqueue("b", evs("e", 4, 20)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if got := testutil.ToFloat64(m.ProcessedEvents.WithLabelValues(metrics.ProcessedFailed)); got != 4 {
		t.Fatalf("all 4 undelivered events must be counted as failed, got %v", got)
	}
	if got := testutil.ToFloat64(m.ProcessingFailures.WithLabelValues(metrics.StageShutdownDrop)); got != 4 {
		t.Fatalf("shutdown_drop = %v, want 4", got)
	}
}

func TestBackoffDoublesAndIsCapped(t *testing.T) {
	base := 200 * time.Millisecond
	want := []time.Duration{200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond,
		3200 * time.Millisecond, maxBackoff, maxBackoff}
	for i, w := range want {
		if got := backoffFor(base, i+1); got != w {
			t.Errorf("attempt %d: backoff %v, want %v", i+1, got, w)
		}
	}
	if got := backoffFor(base, 1000); got != maxBackoff {
		t.Errorf("huge attempt numbers must not overflow: %v", got)
	}
}
