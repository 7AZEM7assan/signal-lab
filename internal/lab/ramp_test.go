package lab

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func near(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %v, want %v (±%v)", name, got, want, tol)
	}
}

func TestRampTimeMatchesTheArithmetic(t *testing.T) {
	// 100 -> 300 records/s over 10 s: 100*10 + 200*10/2 = 2000 records are sent by the end of the ramp.
	near(t, "start", rampTime(0, 100, 300, 10), 0, 1e-9)
	near(t, "end of ramp", rampTime(2000, 100, 300, 10), 10, 1e-9)
	near(t, "after the ramp, held at 300/s", rampTime(2300, 100, 300, 10), 11, 1e-9)
	// The 500th record: 100t + 10t^2 = 500 -> t = (-100 + sqrt(10000+20000)) / 20.
	near(t, "inside the ramp", rampTime(500, 100, 300, 10), (-100+math.Sqrt(30000))/20, 1e-9)
	// A ramp down: 300 -> 100 over 10 s sends 2000 records too, and falls slower than it started.
	near(t, "ramp down end", rampTime(2000, 300, 100, 10), 10, 1e-9)
	// Equal rates are a plain constant rate.
	near(t, "flat", rampTime(500, 100, 100, 10), 5, 1e-9)
}

func TestScheduleFollowsTheRamp(t *testing.T) {
	p := recorded(2300, 0)
	got := scheduleOf(p, func(c *Config) { c.BatchSize, c.RatePerS, c.RampToPerS, c.RampS = 100, 100, 300, 10 })
	// batches of 100: the 21st batch starts after 2000 records, the 24th after 2300 - 100.
	near(t, "batch 21 starts when the ramp ends", got[20], 10, 1e-9)
	near(t, "batch 22 is one third of a second later", got[21], 10+100.0/300, 1e-9)
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Fatalf("the schedule runs backwards at %d", i)
		}
	}
	// With the ramp off the old arithmetic still holds.
	flat := scheduleOf(p, func(c *Config) { c.BatchSize, c.RatePerS = 100, 100 })
	near(t, "flat rate", flat[5], 5, 1e-9)
}

func TestRampIsValidated(t *testing.T) {
	ok := func(mut func(*Config)) error { c := small(); mut(&c); return c.Validate(500) }
	if err := ok(func(c *Config) { c.RatePerS, c.RampToPerS, c.RampS = 50, 500, 20 }); err != nil {
		t.Errorf("a normal ramp: %v", err)
	}
	for name, mut := range map[string]func(*Config){
		"no starting rate":    func(c *Config) { c.RatePerS, c.RampToPerS, c.RampS = 0, 500, 20 },
		"no ramp time":        func(c *Config) { c.RatePerS, c.RampToPerS, c.RampS = 50, 500, 0 },
		"negative final rate": func(c *Config) { c.RampToPerS = -1 },
		"absurd final rate":   func(c *Config) { c.RatePerS, c.RampToPerS, c.RampS = 50, MaxRampRate+1, 20 },
	} {
		if err := ok(mut); err == nil {
			t.Errorf("%s should be refused", name)
		}
	}
	// A service outside this computer keeps its 2,000 records per second limit, ramp included.
	c := external("http://203.0.113.9/ingest")
	c.TargetConfirmed, c.RatePerS, c.RampToPerS, c.RampS = true, 100, MaxExternalRate+1, 10
	if err := c.Validate(500); err == nil || !strings.Contains(err.Error(), "ramp must stay within") {
		t.Fatalf("a remote ramp past the limit must be refused: %v", err)
	}
}

func TestTheTimelineShowsWhereTheServiceStartedToPushBack(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Fine for the first 30 requests, then it pushes back with 429 for every request.
		if n.Add(1) > 30 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	run := NewRunner()
	c := small()
	c.DurationS, c.IntervalS, c.Devices = 40, 1, 2 // 80 records
	c.BatchSize, c.RatePerS, c.Retries, c.TimeoutS = 2, 0, 0, 5
	c.TargetURL = srv.URL
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	waitDone(t, run)
	tl := run.Timeline()
	if len(tl.Points) == 0 {
		t.Fatal("no timeline")
	}
	var requests, throttled, records int
	for _, p := range tl.Points {
		requests += p.Requests
		throttled += p.Throttled
		records += p.Records
	}
	if requests != 40 || throttled != 10 || records != 60 {
		t.Fatalf("40 requests of 2 records: 30 accepted (60 records), 10 pushed back; got requests %d, throttled %d, records %d", requests, throttled, records)
	}
	s := tl.Summary
	if s.FirstThrottledAtS == nil || s.PeakAcceptedPerS == 0 || s.PeakSentPerS < s.PeakAcceptedPerS {
		t.Fatalf("summary = %+v", s)
	}
	if s.FirstErrorAtS != nil {
		t.Fatalf("429 is pushback, not an error: %+v", s)
	}
	// A new run starts a fresh record.
	c.TargetURL = srv.URL
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	waitDone(t, run)
	again := 0
	for _, p := range run.Timeline().Points {
		again += p.Requests
	}
	if again != 40 {
		t.Fatalf("the second run should record only itself: %d requests", again)
	}
}

func TestTheTimelineCountsFailuresAsErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }))
	defer srv.Close()
	run := NewRunner()
	c := small()
	c.BatchSize, c.RatePerS, c.TargetURL = 10, 0, srv.URL
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	waitDone(t, run)
	s := run.Timeline().Summary
	if s.FirstErrorAtS == nil || s.FirstThrottledAtS != nil {
		t.Fatalf("summary = %+v", s)
	}
}

func TestLatencyPercentilesPerSecond(t *testing.T) {
	r := NewRunner()
	start := time.Now()
	r.mu.Lock()
	for i := 1; i <= 100; i++ {
		r.tick(start, 1, outcomeOK, float64(i)/1000) // 1 ms ... 100 ms
	}
	r.mu.Unlock()
	p := r.Timeline().Points[0]
	if p.Requests != 100 || p.P50Ms < 49 || p.P50Ms > 51 || p.P95Ms < 94 || p.P95Ms > 96 {
		t.Fatalf("point = %+v", p)
	}
}
