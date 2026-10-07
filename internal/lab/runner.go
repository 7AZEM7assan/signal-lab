package lab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"net/http"
	"sort"
	"strconv"
	"sync"
	"time"
)

const maxRetryAfter = 5 * time.Second

// State of a replay.
type State string

const (
	StateIdle     State = "idle"
	StateRunning  State = "running"
	StateDone     State = "done"
	StateStopped  State = "stopped"
	StateFailed   State = "failed"
	maxLatencyLog       = 200_000 // bounds memory for very long runs
)

// Target is where batches are POSTed.
type Target struct {
	BaseURL string            // e.g. http://127.0.0.1:8088
	Headers map[string]string // e.g. the app's token header
}

// Snapshot is a point-in-time view of a run, safe to marshal.
type Snapshot struct {
	State           State          `json:"state"`
	Error           string         `json:"error,omitempty"`
	Config          *Config        `json:"config,omitempty"`
	StartedAt       *time.Time     `json:"started_at,omitempty"`
	ElapsedS        float64        `json:"elapsed_s"`
	Planned         int            `json:"planned"` // records to send, including injected duplicates and malformed ones
	Batches         int            `json:"batches"`
	BatchesDone     int            `json:"batches_done"`
	RecordsDone     int            `json:"records_done"`
	Accepted        int            `json:"accepted"` // acknowledged with 202 (queued, not necessarily stored)
	Rejected        int            `json:"rejected"` // refused by validation
	RejectionCounts map[string]int `json:"rejection_counts,omitempty"`
	Throttled       int            `json:"throttled"` // 429 responses seen, including retried ones
	Retries         int            `json:"retries"`
	GaveUpRecords   int            `json:"gave_up_records"` // valid records in batches refused with 429/503 after all retries
	RequestErrors   int            `json:"request_errors"`
	ErroredRecords  int            `json:"errored_records"`
	Injected        *FaultCounts   `json:"injected,omitempty"`
	Latency         map[string]any `json:"latency,omitempty"`
	ThroughputPerS  float64        `json:"throughput_per_s"`
}

// Runner runs one replay at a time.
type Runner struct {
	client *http.Client
	now    func() time.Time

	mu     sync.Mutex
	snap   Snapshot
	lat    []float64
	cancel context.CancelFunc
	done   chan struct{}
}

// NewRunner creates an idle runner.
func NewRunner() *Runner {
	return &Runner{client: &http.Client{}, now: time.Now, snap: Snapshot{State: StateIdle}}
}

// ErrBusy is returned by Start while a run is in progress.
var ErrBusy = errors.New("a replay is already running")

// Start validates cfg, generates and plans the dataset, then runs it in the background.
func (r *Runner) Start(cfg Config, target Target, maxBatch int) (Snapshot, error) {
	if err := cfg.Validate(maxBatch); err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	if r.snap.State == StateRunning {
		r.mu.Unlock()
		return Snapshot{}, ErrBusy
	}
	r.mu.Unlock()

	now := r.now()
	records, err := Generate(cfg, now)
	if err != nil {
		return Snapshot{}, err
	}
	planned, injected := PlanRecords(records, cfg)
	batches, schedule := Schedule(planned, cfg)

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.snap.State == StateRunning { // lost a race with a concurrent Start
		r.mu.Unlock()
		cancel()
		return Snapshot{}, ErrBusy
	}
	c := cfg
	started := now.UTC()
	r.snap = Snapshot{State: StateRunning, Config: &c, StartedAt: &started, Planned: len(planned), Batches: len(batches),
		Injected: &injected, RejectionCounts: map[string]int{}}
	r.lat = r.lat[:0]
	r.cancel = cancel
	r.done = make(chan struct{})
	done := r.done
	r.mu.Unlock()

	go func() {
		defer close(done)
		r.run(ctx, cfg, target, batches, schedule, started)
	}()
	return r.Snapshot(), nil
}

// Stop asks a running replay to finish early and waits for it.
func (r *Runner) Stop() Snapshot {
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	running := r.snap.State == StateRunning
	r.mu.Unlock()
	if running && cancel != nil {
		cancel()
		<-done
	}
	return r.Snapshot()
}

// Snapshot returns the current progress.
func (r *Runner) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snap
	if s.RejectionCounts != nil {
		s.RejectionCounts = cloneCounts(s.RejectionCounts)
	}
	if s.State == StateRunning && s.StartedAt != nil {
		s.ElapsedS = r.now().Sub(*s.StartedAt).Seconds()
	}
	if s.ElapsedS > 0 {
		s.ThroughputPerS = float64(s.RecordsDone) / s.ElapsedS
	}
	s.Latency = latencySummary(r.lat)
	return s
}

func cloneCounts(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func (r *Runner) run(ctx context.Context, cfg Config, target Target, batches [][]Planned, schedule []float64, started time.Time) {
	var nextMu sync.Mutex
	next := 0
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			jitter := rand.New(rand.NewPCG(uint64(cfg.Seed), uint64(id)+1))
			for {
				nextMu.Lock()
				i := next
				next++
				nextMu.Unlock()
				if i >= len(batches) || ctx.Err() != nil {
					return
				}
				delay := time.Duration(schedule[i]*float64(time.Second)) - time.Since(start)
				if cfg.JitterMS > 0 {
					delay += time.Duration(jitter.Float64() * cfg.JitterMS * float64(time.Millisecond))
				}
				if delay > 0 && !sleep(ctx, delay) {
					return
				}
				r.sendBatch(ctx, target, cfg, batches[i])
			}
		}(w)
	}
	wg.Wait()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.snap.ElapsedS = time.Since(start).Seconds()
	switch {
	case ctx.Err() != nil:
		r.snap.State = StateStopped
	default:
		r.snap.State = StateDone
	}
	r.cancel = nil
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

type ackBody struct {
	Accepted        int            `json:"accepted"`
	Rejected        int            `json:"rejected"`
	RejectionCounts map[string]int `json:"rejection_counts"`
}

func (r *Runner) sendBatch(ctx context.Context, target Target, cfg Config, batch []Planned) {
	var body bytes.Buffer
	body.WriteString(`{"events":[`)
	for i, p := range batch {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(p.Body)
	}
	body.WriteString(`]}`)
	payload := body.Bytes()

	timeout := time.Duration(cfg.TimeoutS * float64(time.Second))
	for attempt := 0; ; attempt++ {
		t0 := time.Now()
		status, ack, retryAfter, err := r.post(ctx, target, payload, timeout)
		elapsed := time.Since(t0).Seconds()

		r.mu.Lock()
		if len(r.lat) < maxLatencyLog {
			r.lat = append(r.lat, elapsed)
		}
		switch {
		case ctx.Err() != nil:
			r.mu.Unlock()
			return
		case err != nil || (status != http.StatusAccepted && status != http.StatusTooManyRequests && status != http.StatusServiceUnavailable):
			r.snap.RequestErrors++
			r.snap.ErroredRecords += len(batch)
			r.finishBatch(len(batch))
			r.mu.Unlock()
			return
		case status == http.StatusAccepted:
			r.recordAck(ack)
			r.finishBatch(len(batch))
			r.mu.Unlock()
			return
		}
		// 429 or 503: the whole batch of valid records was refused.
		if status == http.StatusTooManyRequests {
			r.snap.Throttled++
		}
		if attempt >= cfg.Retries {
			r.recordRejections(ack)
			r.snap.GaveUpRecords += max(0, len(batch)-ack.Rejected)
			r.finishBatch(len(batch))
			r.mu.Unlock()
			return
		}
		r.snap.Retries++
		r.mu.Unlock()
		if !sleep(ctx, retryAfter) {
			return
		}
	}
}

func (r *Runner) finishBatch(n int) {
	r.snap.BatchesDone++
	r.snap.RecordsDone += n
}

func (r *Runner) recordAck(a ackBody) {
	r.snap.Accepted += a.Accepted
	r.recordRejections(a)
}

func (r *Runner) recordRejections(a ackBody) {
	r.snap.Rejected += a.Rejected
	for reason, n := range a.RejectionCounts {
		r.snap.RejectionCounts[reason] += n
	}
}

func (r *Runner) post(ctx context.Context, target Target, payload []byte, timeout time.Duration) (int, ackBody, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target.BaseURL+"/api/v1/events", bytes.NewReader(payload))
	if err != nil {
		return 0, ackBody{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return 0, ackBody{}, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var ack ackBody
	_ = json.Unmarshal(raw, &ack)
	return resp.StatusCode, ack, parseRetryAfter(resp.Header.Get("Retry-After")), nil
}

func parseRetryAfter(v string) time.Duration {
	if v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			d := time.Duration(secs * float64(time.Second))
			return min(max(d, 0), maxRetryAfter)
		}
	}
	return time.Second
}

// latencySummary reports request latencies in milliseconds (nearest-rank percentiles).
func latencySummary(lat []float64) map[string]any {
	if len(lat) == 0 {
		return nil
	}
	s := append([]float64(nil), lat...)
	sort.Float64s(s)
	pct := func(p float64) float64 {
		rank := max(1, int(math.Ceil(p/100*float64(len(s)))))
		return round3(s[min(rank, len(s))-1] * 1000)
	}
	var sum float64
	for _, v := range s {
		sum += v
	}
	return map[string]any{
		"count": len(s), "p50_ms": pct(50), "p95_ms": pct(95), "p99_ms": pct(99),
		"max_ms": round3(s[len(s)-1] * 1000), "mean_ms": round3(sum / float64(len(s)) * 1000),
	}
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }

// String is a one-line summary for logs.
func (s Snapshot) String() string {
	return fmt.Sprintf("%s: %d/%d records, %d accepted, %d rejected, %d throttled", s.State, s.RecordsDone, s.Planned, s.Accepted, s.Rejected, s.Throttled)
}
