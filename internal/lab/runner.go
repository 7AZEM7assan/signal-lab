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
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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

// Target is where batches are POSTed. For the built-in service BaseURL and Headers (the app's
// token) are used. For a service of the user's own the runner builds the target from the Config
// itself and only takes UserAgent from here, so the app's token can never be sent to it.
type Target struct {
	BaseURL   string            // e.g. http://127.0.0.1:8088
	Headers   map[string]string // e.g. the app's token header
	UserAgent string            // sent to every service; optional
}

// ErrorSample is the first thing that went wrong, kept so the user can see why a run failed.
// Message and Body are cleaned: no address, no query string, no header values.
type ErrorSample struct {
	Batch   int    `json:"batch"` // 1-based number of the batch (or request)
	Status  int    `json:"status,omitempty"`
	Message string `json:"message"`
	Body    string `json:"body,omitempty"` // start of the response body, if any
}

// Snapshot is a point-in-time view of a run, safe to marshal.
type Snapshot struct {
	State            State          `json:"state"`
	Error            string         `json:"error,omitempty"`
	Config           *Config        `json:"config,omitempty"`
	StartedAt        *time.Time     `json:"started_at,omitempty"`
	ElapsedS         float64        `json:"elapsed_s"`
	Planned          int            `json:"planned"` // records to send, including injected duplicates and malformed ones
	Batches          int            `json:"batches"`
	PlannedDurationS float64        `json:"planned_duration_s,omitempty"` // how long the sending is planned to take, when it follows recorded time
	BatchesDone      int            `json:"batches_done"`
	RecordsDone      int            `json:"records_done"`
	Accepted         int            `json:"accepted"` // acknowledged with 202 (queued, not necessarily stored)
	Rejected         int            `json:"rejected"` // refused by validation
	RejectionCounts  map[string]int `json:"rejection_counts,omitempty"`
	Throttled        int            `json:"throttled"` // 429 responses seen, including retried ones
	Retries          int            `json:"retries"`
	GaveUpRecords    int            `json:"gave_up_records"` // valid records in batches refused with 429/503 after all retries
	RequestErrors    int            `json:"request_errors"`
	ErroredRecords   int            `json:"errored_records"`
	Injected         *FaultCounts   `json:"injected,omitempty"`
	Latency          map[string]any `json:"latency,omitempty"`
	ThroughputPerS   float64        `json:"throughput_per_s"`
	Target           string         `json:"target,omitempty"`        // where it was sent: the built-in service, or your address without its query string
	External         bool           `json:"external"`                // sent to a service of your own
	Source           string         `json:"source,omitempty"`        // generated data, or the imported file's name
	StatusCounts     map[string]int `json:"status_counts,omitempty"` // responses by HTTP status
	FirstError       *ErrorSample   `json:"first_error,omitempty"`
}

// Runner runs one replay at a time.
type Runner struct {
	client *http.Client
	now    func() time.Time

	mu      sync.Mutex
	snap    Snapshot
	lat     []float64
	cancel  context.CancelFunc
	done    chan struct{}
	dataset *Dataset

	tl          []bucket // one entry per second of the latest run
	tlTruncated bool
}

// NewRunner creates an idle runner. It never follows redirects: a redirect would resend the
// headers (an API key) to another address and turn a POST into a GET, hiding the real answer.
func NewRunner() *Runner {
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Runner{client: client, now: time.Now, snap: Snapshot{State: StateIdle}}
}

// SetDataset makes d the imported file that runs with UseDataset replay. It is kept in memory only.
func (r *Runner) SetDataset(d *Dataset) {
	r.mu.Lock()
	r.dataset = d
	r.mu.Unlock()
}

// DatasetSummary describes the imported file, or nil when there is none.
func (r *Runner) DatasetSummary() *DatasetSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dataset == nil {
		return nil
	}
	s := r.dataset.Summary
	return &s
}

// ErrBusy is returned by Start while a run is in progress.
var ErrBusy = errors.New("a replay is already running")

// Start validates cfg, builds and plans the dataset, then runs it in the background.
func (r *Runner) Start(cfg Config, target Target, maxBatch int) (Snapshot, error) {
	external := cfg.External()
	if external {
		cfg.TargetURL = strings.TrimSpace(cfg.TargetURL)
		if cfg.PayloadFormat == "" {
			cfg.PayloadFormat = FormatBatch
		}
		if cfg.PayloadFormat == FormatSingle {
			cfg.BatchSize = 1
		}
		maxBatch = MaxExternalBatch
	}
	if err := cfg.Validate(maxBatch); err != nil {
		return Snapshot{}, err
	}
	r.mu.Lock()
	if r.snap.State == StateRunning {
		r.mu.Unlock()
		return Snapshot{}, ErrBusy
	}
	ds := r.dataset
	r.mu.Unlock()

	now := r.now()
	var (
		records []Record
		source  = "generated data"
		err     error
	)
	if cfg.UseDataset {
		if ds == nil {
			return Snapshot{}, errors.New("no file has been imported; import one first or turn off \"use my own file\"")
		}
		records, source = ds.Records, "file "+ds.Summary.Name
		if cfg.RebaseTime {
			records = ds.Rebased(now)
		}
	} else if records, err = Generate(cfg, now); err != nil {
		return Snapshot{}, err
	}
	planned, injected := PlanRecords(records, cfg)
	batches, schedule := Schedule(planned, cfg)
	var duration float64
	if len(schedule) > 0 {
		duration = schedule[len(schedule)-1]
	}
	if cfg.Speed > 0 && duration > MaxReplaySeconds {
		return Snapshot{}, fmt.Errorf("at speed %g this replay would take %s; raise the speed or use a shorter file (the longest allowed is %d hours)",
			cfg.Speed, humanSeconds(duration), MaxReplaySeconds/3600)
	}

	label := "the built-in service"
	if external {
		target = Target{BaseURL: "", Headers: cfg.TargetHeaders, UserAgent: target.UserAgent}
		label = DisplayURL(cfg.TargetURL)
	}

	ctx, cancel := context.WithCancel(context.Background())
	r.mu.Lock()
	if r.snap.State == StateRunning { // lost a race with a concurrent Start
		r.mu.Unlock()
		cancel()
		return Snapshot{}, ErrBusy
	}
	c := cfg.Redacted()
	started := now.UTC()
	r.snap = Snapshot{State: StateRunning, Config: &c, StartedAt: &started, Planned: len(planned), Batches: len(batches), PlannedDurationS: duration,
		Injected: &injected, RejectionCounts: map[string]int{}, StatusCounts: map[string]int{},
		Target: label, External: external, Source: source}
	r.lat = r.lat[:0]
	r.tl, r.tlTruncated = nil, false
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
	if s.StatusCounts != nil {
		s.StatusCounts = cloneCounts(s.StatusCounts)
	}
	if s.FirstError != nil {
		e := *s.FirstError
		s.FirstError = &e
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
				r.sendBatch(ctx, target, cfg, batches[i], i, started)
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

// parseAck reads the built-in service's acknowledgement. ok is false when the body is not one
// (a service of your own answers in its own way).
func parseAck(raw []byte) (ack ackBody, ok bool) {
	var w struct {
		Accepted        *int           `json:"accepted"`
		Rejected        *int           `json:"rejected"`
		RejectionCounts map[string]int `json:"rejection_counts"`
	}
	if json.Unmarshal(raw, &w) != nil || w.Accepted == nil {
		return ackBody{}, false
	}
	ack.Accepted, ack.RejectionCounts = *w.Accepted, w.RejectionCounts
	if w.Rejected != nil {
		ack.Rejected = *w.Rejected
	}
	return ack, true
}

type reply struct {
	status     int
	raw        []byte
	retryAfter time.Duration
	err        error
}

// payload builds the request body for one batch in the chosen format and says its content type.
func payload(format string, batch []Planned) ([]byte, string) {
	var body bytes.Buffer
	switch format {
	case FormatArray:
		body.WriteByte('[')
		for i, p := range batch {
			if i > 0 {
				body.WriteByte(',')
			}
			body.Write(p.Body)
		}
		body.WriteByte(']')
		return body.Bytes(), "application/json"
	case FormatNDJSON:
		for _, p := range batch {
			body.Write(p.Body)
			body.WriteByte('\n')
		}
		return body.Bytes(), "application/x-ndjson"
	case FormatSingle:
		return batch[0].Body, "application/json"
	}
	body.WriteString(`{"events":[`)
	for i, p := range batch {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(p.Body)
	}
	body.WriteString(`]}`)
	return body.Bytes(), "application/json"
}

func (r *Runner) sendBatch(ctx context.Context, target Target, cfg Config, batch []Planned, idx int, started time.Time) {
	external := cfg.External()
	body, ctype := payload(cfg.PayloadFormat, batch)
	reqID := fmt.Sprintf("sl-%d-%d", started.Unix(), idx+1)

	timeout := time.Duration(cfg.TimeoutS * float64(time.Second))
	for attempt := 0; ; attempt++ {
		t0 := time.Now()
		rep := r.post(ctx, cfg, target, body, ctype, reqID, timeout)
		elapsed := time.Since(t0).Seconds()
		ack, isAck := parseAck(rep.raw)

		r.mu.Lock()
		if len(r.lat) < maxLatencyLog {
			r.lat = append(r.lat, elapsed)
		}
		if ctx.Err() != nil {
			r.mu.Unlock()
			return
		}
		if rep.err == nil {
			r.snap.StatusCounts[strconv.Itoa(rep.status)]++
		}
		success := rep.status == http.StatusAccepted
		if external {
			success = rep.status >= 200 && rep.status < 300
		}
		retryable := rep.status == http.StatusTooManyRequests || rep.status == http.StatusServiceUnavailable
		switch {
		case rep.err != nil || (!success && !retryable):
			r.tick(started, len(batch), outcomeError, elapsed)
		case success:
			r.tick(started, len(batch), outcomeOK, elapsed)
		default:
			r.tick(started, len(batch), outcomeThrottled, elapsed)
		}
		switch {
		case rep.err != nil || (!success && !retryable):
			r.snap.RequestErrors++
			r.snap.ErroredRecords += len(batch)
			r.noteError(idx, rep)
			r.finishBatch(len(batch))
			r.mu.Unlock()
			return
		case success:
			if external && !(isAck && ack.Accepted+ack.Rejected == len(batch)) {
				// A service of your own: any 2xx means every record in the request was taken.
				r.snap.Accepted += len(batch)
			} else {
				r.recordAck(ack)
			}
			r.finishBatch(len(batch))
			r.mu.Unlock()
			return
		}
		// 429 or 503: the whole batch of valid records was refused.
		if rep.status == http.StatusTooManyRequests {
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
		if !sleep(ctx, rep.retryAfter) {
			return
		}
	}
}

// noteError remembers the first failure with text that is safe to show.
func (r *Runner) noteError(idx int, rep reply) {
	if r.snap.FirstError != nil {
		return
	}
	e := &ErrorSample{Batch: idx + 1}
	if rep.err != nil {
		e.Message = transportError(rep.err)
	} else {
		e.Status = rep.status
		e.Message = fmt.Sprintf("the service answered %d %s", rep.status, http.StatusText(rep.status))
		e.Body = excerpt(rep.raw, 300)
	}
	r.snap.FirstError = e
}

// transportError describes a failed request without the address: Go's errors repeat the whole
// URL, and the query string may hold a key.
func transportError(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer before the timeout"
	}
	return excerpt([]byte(err.Error()), 200)
}

// excerpt returns the start of b as one line of plain text, at most n bytes.
func excerpt(b []byte, n int) string {
	if len(b) == 0 {
		return ""
	}
	cut := len(b) > n
	if cut {
		b = b[:n]
	}
	s := strings.ToValidUTF8(string(b), "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == utf8.RuneError {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if cut {
		s += "…"
	}
	return s
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

func (r *Runner) post(ctx context.Context, cfg Config, target Target, body []byte, ctype, reqID string, timeout time.Duration) reply {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := target.BaseURL + "/api/v1/events"
	if cfg.External() {
		endpoint = cfg.TargetURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return reply{err: err}
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Request-ID", reqID)
	if target.UserAgent != "" {
		req.Header.Set("User-Agent", target.UserAgent)
	}
	for k, v := range target.Headers {
		req.Header.Set(k, v)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return reply{err: err}
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return reply{status: resp.StatusCode, raw: raw, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
}

func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil {
			d := time.Duration(secs * float64(time.Second))
			return min(max(d, 0), maxRetryAfter)
		}
		if t, err := http.ParseTime(v); err == nil {
			return min(max(time.Until(t), 0), maxRetryAfter)
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
