package lab

import (
	"sort"
	"time"
)

const (
	// maxTimelineSeconds bounds the per-second record of a run (one hour); a longer run keeps the first hour.
	maxTimelineSeconds = 3600
	// maxSamplesPerSecond bounds the latencies kept for one second, so a very fast run cannot use unbounded memory.
	maxSamplesPerSecond = 500
)

const (
	outcomeOK        = "ok"
	outcomeThrottled = "throttled"
	outcomeError     = "error"
)

// bucket is one second of a run.
type bucket struct {
	requests, sent, records, throttled, errors int
	lat                                        []float64
}

// Point is one second of a run, as shown in the timeline. T is the second since the start.
type Point struct {
	T         int     `json:"t"`
	Requests  int     `json:"requests"`  // requests that got an answer (or failed) in this second, retries included
	Sent      int     `json:"sent"`      // records those requests carried: how hard the service was pushed
	Records   int     `json:"records"`   // records the service accepted in this second
	Throttled int     `json:"throttled"` // answered 429 or 503
	Errors    int     `json:"errors"`    // failed, or answered with another error
	P50Ms     float64 `json:"p50_ms"`
	P95Ms     float64 `json:"p95_ms"`
}

// TimelineSummary picks out what a run shows: the best second, and when it first went wrong.
type TimelineSummary struct {
	PeakSentPerS      int     `json:"peak_sent_per_s"`
	PeakAcceptedPerS  int     `json:"peak_accepted_per_s"`
	FirstThrottledAtS *int    `json:"first_throttled_at_s,omitempty"`
	SentPerSThen      *int    `json:"sent_per_s_at_first_throttle,omitempty"`
	FirstErrorAtS     *int    `json:"first_error_at_s,omitempty"`
	SentPerSAtError   *int    `json:"sent_per_s_at_first_error,omitempty"`
	P95FirstMs        float64 `json:"p95_first_ms"` // the slowest 5% of requests in the first seconds with traffic
	P95LastMs         float64 `json:"p95_last_ms"`  // and in the last seconds
}

// Timeline is the per-second record of the current or latest run.
type Timeline struct {
	Points    []Point         `json:"points"`
	Truncated bool            `json:"truncated,omitempty"`
	Summary   TimelineSummary `json:"summary"`
}

// tick records one answered request in the second it finished. The caller holds r.mu.
func (r *Runner) tick(started time.Time, records int, outcome string, latencyS float64) {
	sec := int(r.now().Sub(started).Seconds())
	if sec < 0 {
		sec = 0
	}
	if sec >= maxTimelineSeconds {
		r.tlTruncated = true
		return
	}
	for len(r.tl) <= sec {
		r.tl = append(r.tl, bucket{})
	}
	b := &r.tl[sec]
	b.requests++
	b.sent += records
	switch outcome {
	case outcomeOK:
		b.records += records
	case outcomeThrottled:
		b.throttled++
	default:
		b.errors++
	}
	if len(b.lat) < maxSamplesPerSecond {
		b.lat = append(b.lat, latencyS)
	}
}

// Timeline returns the per-second record of the current or latest run.
func (r *Runner) Timeline() Timeline {
	r.mu.Lock()
	defer r.mu.Unlock()
	tl := Timeline{Points: make([]Point, len(r.tl)), Truncated: r.tlTruncated}
	for i, b := range r.tl {
		p := Point{T: i, Requests: b.requests, Sent: b.sent, Records: b.records, Throttled: b.throttled, Errors: b.errors}
		if len(b.lat) > 0 {
			s := append([]float64(nil), b.lat...)
			sort.Float64s(s)
			p.P50Ms = ms(s[(len(s)-1)/2])
			p.P95Ms = ms(s[int(float64(len(s)-1)*0.95)])
		}
		tl.Points[i] = p
	}
	tl.Summary = summarize(tl.Points)
	return tl
}

func ms(seconds float64) float64 { return float64(int(seconds*10000+0.5)) / 10 }

func summarize(pts []Point) TimelineSummary {
	var s TimelineSummary
	var busy []Point
	for _, p := range pts {
		if p.Requests > 0 {
			busy = append(busy, p)
		}
		s.PeakSentPerS = max(s.PeakSentPerS, p.Sent)
		s.PeakAcceptedPerS = max(s.PeakAcceptedPerS, p.Records)
		if p.Throttled > 0 && s.FirstThrottledAtS == nil {
			t, sent := p.T, p.Sent
			s.FirstThrottledAtS, s.SentPerSThen = &t, &sent
		}
		if p.Errors > 0 && s.FirstErrorAtS == nil {
			t, sent := p.T, p.Sent
			s.FirstErrorAtS, s.SentPerSAtError = &t, &sent
		}
	}
	if n := len(busy); n > 0 {
		edge := min(3, n)
		s.P95FirstMs, s.P95LastMs = worstP95(busy[:edge]), worstP95(busy[n-edge:])
	}
	return s
}

func worstP95(pts []Point) float64 {
	v := 0.0
	for _, p := range pts {
		v = max(v, p.P95Ms)
	}
	return v
}
