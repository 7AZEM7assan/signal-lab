// Package pipeline is the bounded, in-memory path between the ingest API and the database.
//
// Delivery semantics (also documented in the README):
//
//   - "Accepted" means: validated and placed on an in-memory queue. It does NOT mean
//     persisted. Events on the queue are lost if the process dies.
//   - The queue is bounded. A batch is enqueued all-or-nothing; if it does not fit,
//     Enqueue returns ErrQueueFull immediately and the caller answers 429. Nothing is
//     dropped silently and nothing blocks.
//   - Workers persist in batches and retry failed attempts with exponential backoff
//     (capped at maxBackoff). While a worker is retrying it takes nothing new off the
//     queue, so a database outage shorter than the retry window shows up as a filling
//     queue and then 429s on ingest: that is the backpressure path.
//   - If the attempts are exhausted (default: roughly 26 s of outage), or the shutdown
//     drain deadline passes, the affected events are discarded and counted in
//     processing_failures / processed{failed}. Acknowledged events can therefore be lost.
//   - Processing is at-least-once from the client's view (clients may resend) and
//     idempotent in the database (duplicates are skipped). It is not exactly-once.
//   - Workers run concurrently, so events are NOT stored or broadcast in arrival order.
package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/metrics"
	"signallab/internal/store"
)

var (
	// ErrQueueFull means the batch did not fit; retry later.
	ErrQueueFull = errors.New("queue full")
	// ErrClosed means the pipeline is draining for shutdown.
	ErrClosed = errors.New("pipeline closed")
)

// Persister is what the pipeline needs from the database layer.
type Persister interface {
	PersistBatch(ctx context.Context, evs []event.Event, th alert.Thresholds) (store.Result, error)
}

// Broadcaster receives committed events and alerts for live fan-out.
type Broadcaster interface {
	Broadcast(deviceID string, payload []byte)
}

type Config struct {
	Capacity   int
	Workers    int
	BatchSize  int
	Attempts   int // total persist attempts per batch, >= 1
	Backoff    time.Duration
	LabDelay   time.Duration
	Thresholds alert.Thresholds
}

type item struct {
	ev         event.Event
	batchID    string
	enqueuedAt time.Time
}

type Pipeline struct {
	cfg   Config
	th    atomic.Pointer[alert.Thresholds] // live-editable; starts as cfg.Thresholds
	db    Persister
	bc    Broadcaster
	m     *metrics.Metrics
	log   *slog.Logger
	queue chan item

	mu     sync.Mutex // guards closed and serialises enqueues (see Enqueue)
	closed bool

	wg     sync.WaitGroup
	ctx    context.Context // cancelled only when the drain deadline passes
	cancel context.CancelFunc
}

// New creates the pipeline and starts its workers.
func New(cfg Config, db Persister, bc Broadcaster, m *metrics.Metrics, log *slog.Logger) *Pipeline {
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pipeline{cfg: cfg, db: db, bc: bc, m: m, log: log, queue: make(chan item, cfg.Capacity), ctx: ctx, cancel: cancel}
	initial := cfg.Thresholds
	p.th.Store(&initial)
	m.Registry.MustRegister(
		gaugeFunc("signallab_queue_depth", "Events currently waiting in the ingest queue.", func() float64 { return float64(len(p.queue)) }),
		gaugeFunc("signallab_queue_capacity", "Configured ingest queue capacity.", func() float64 { return float64(cfg.Capacity) }),
	)
	for i := 0; i < cfg.Workers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
	return p
}

// Depth and Capacity expose queue state (for responses and tests).
// SetThresholds changes the alert thresholds for batches persisted from now on. Events already
// stored keep the alerts they were given.
func (p *Pipeline) SetThresholds(th alert.Thresholds) { p.th.Store(&th) }

// Thresholds returns the thresholds currently in effect.
func (p *Pipeline) Thresholds() alert.Thresholds { return *p.th.Load() }

func (p *Pipeline) Depth() int    { return len(p.queue) }
func (p *Pipeline) Capacity() int { return cap(p.queue) }

// Closed reports whether the pipeline has begun draining.
func (p *Pipeline) Closed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Enqueue places all events on the queue or none of them.
//
// Concurrency: enqueuers hold p.mu, and only enqueuers add to the channel, so
// free space computed under the lock can only grow (workers only remove). The
// sends below therefore never block, and a batch is never half-accepted.
func (p *Pipeline) Enqueue(batchID string, evs []event.Event) error {
	if len(evs) == 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return ErrClosed
	}
	if cap(p.queue)-len(p.queue) < len(evs) {
		return ErrQueueFull
	}
	now := time.Now()
	for _, ev := range evs {
		p.queue <- item{ev: ev, batchID: batchID, enqueuedAt: now}
	}
	return nil
}

// Close stops accepting events, lets workers drain what is queued, and waits.
// If ctx expires first, in-flight work is cancelled, the rest is discarded and
// counted, and ctx's error is returned.
func (p *Pipeline) Close(ctx context.Context) error {
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		close(p.queue) // safe: all senders hold p.mu and check closed first
	}
	p.mu.Unlock()

	done := make(chan struct{})
	go func() { p.wg.Wait(); close(done) }()
	select {
	case <-done:
		p.cancel() // release the context; nothing is left to abort
		return nil
	case <-ctx.Done():
		p.cancel() // abort retries/sleeps; remaining items fail fast and are counted
		<-done
		return ctx.Err()
	}
}

func (p *Pipeline) worker() {
	defer p.wg.Done()
	for first := range p.queue {
		batch := append(make([]item, 0, p.cfg.BatchSize), first)
	fill:
		for len(batch) < p.cfg.BatchSize {
			select {
			case it, ok := <-p.queue:
				if !ok {
					break fill
				}
				batch = append(batch, it)
			default:
				break fill
			}
		}
		p.process(batch)
	}
}

func (p *Pipeline) process(batch []item) {
	taken := time.Now()
	evs := make([]event.Event, len(batch))
	for i, it := range batch {
		evs[i] = it.ev
		p.m.QueueWait.Observe(taken.Sub(it.enqueuedAt).Seconds())
		p.m.SourceLag.Observe(event.Lag(it.ev.EventTime, it.ev.ReceivedAt).Seconds())
	}

	if p.cfg.LabDelay > 0 { // lab-only overload knob
		select {
		case <-time.After(p.cfg.LabDelay):
		case <-p.ctx.Done():
		}
	}

	res, err := p.persistWithRetry(evs, batch[0].batchID)
	if err != nil {
		p.m.ProcessedEvents.WithLabelValues(metrics.ProcessedFailed).Add(float64(len(evs)))
		p.log.Error("discarding accepted events after persist failure",
			"batch_id", batch[0].batchID, "events", len(evs), "attempts", p.cfg.Attempts, "error", err)
		if p.ctx.Err() != nil {
			p.m.ProcessingFailures.WithLabelValues(metrics.StageShutdownDrop).Add(float64(len(evs)))
		}
		return
	}

	p.m.ProcessedEvents.WithLabelValues(metrics.ProcessedStored).Add(float64(len(res.Inserted)))
	p.m.ProcessedEvents.WithLabelValues(metrics.ProcessedDuplicate).Add(float64(len(evs) - len(res.Inserted)))

	if res.AlertErr != nil {
		p.m.ProcessingFailures.WithLabelValues(metrics.StageAlertPersist).Inc()
		p.log.Error("alert persistence failed; events were stored", "batch_id", batch[0].batchID, "error", res.AlertErr)
	}
	for _, a := range res.Alerts {
		p.m.AlertsCreated.WithLabelValues(string(a.Rule)).Inc()
	}

	// Broadcast only after commit, and only events that were new: duplicates are not re-announced.
	for _, ev := range res.Inserted {
		p.bc.Broadcast(ev.DeviceID, encode("event", eventPayload(ev)))
	}
	for _, a := range res.Alerts {
		p.bc.Broadcast(a.DeviceID, encode("alert", alertPayload(a)))
	}
}

func (p *Pipeline) persistWithRetry(evs []event.Event, batchID string) (store.Result, error) {
	var lastErr error
	for attempt := 1; attempt <= p.cfg.Attempts; attempt++ {
		start := time.Now()
		res, err := p.db.PersistBatch(p.ctx, evs, *p.th.Load())
		p.m.ProcessingDuration.Observe(time.Since(start).Seconds())
		if err == nil {
			return res, nil
		}
		lastErr = err
		p.m.ProcessingFailures.WithLabelValues(metrics.StagePersist).Inc()
		p.log.Warn("persist attempt failed", "batch_id", batchID, "attempt", attempt, "of", p.cfg.Attempts, "error", err)
		if attempt == p.cfg.Attempts || p.ctx.Err() != nil {
			break
		}
		select {
		case <-time.After(backoffFor(p.cfg.Backoff, attempt)):
		case <-p.ctx.Done():
		}
	}
	return store.Result{}, lastErr
}

// maxBackoff caps the exponential retry delay.
const maxBackoff = 5 * time.Second

// backoffFor returns the delay after the given failed attempt (1-based): base, 2*base, 4*base, ... capped.
func backoffFor(base time.Duration, attempt int) time.Duration {
	shift := attempt - 1
	if shift > 20 || base<<shift > maxBackoff || base<<shift < 0 {
		return maxBackoff
	}
	return base << shift
}

// ---- wire payloads for WebSocket clients: only what a monitor needs ----

type envelope struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

type EventPayload struct {
	EventID      string    `json:"event_id"`
	DeviceID     string    `json:"device_id"`
	EventTime    time.Time `json:"event_time"`
	TemperatureC float64   `json:"temperature_c"`
	VibrationMMS float64   `json:"vibration_mm_s"`
}

type AlertPayload struct {
	AlertID   string     `json:"alert_id"`
	DeviceID  string     `json:"device_id"`
	EventID   string     `json:"event_id"`
	Rule      alert.Rule `json:"rule"`
	Threshold float64    `json:"threshold"`
	Observed  float64    `json:"observed"`
	EventTime time.Time  `json:"event_time"`
}

func eventPayload(ev event.Event) EventPayload {
	return EventPayload{ev.EventID, ev.DeviceID, ev.EventTime, ev.TemperatureC, ev.VibrationMMS}
}

func alertPayload(a alert.Alert) AlertPayload {
	return AlertPayload{a.ID, a.DeviceID, a.EventID, a.Rule, a.Threshold, a.Observed, a.EventTime}
}

func encode(kind string, data any) []byte {
	b, err := json.Marshal(envelope{Type: kind, Data: data})
	if err != nil { // unreachable for these concrete types; keep the worker alive regardless
		return []byte(`{"type":"error"}`)
	}
	return b
}
