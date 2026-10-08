package lab

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"signallab/internal/event"
)

var fixedNow = time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)

func small() Config {
	c := Defaults()
	c.Devices, c.DurationS, c.IntervalS, c.RatePerS = 3, 60, 1, 0
	return c
}

func TestGenerateIsDeterministicAndValidForTheService(t *testing.T) {
	c := small()
	a, err := Generate(c, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := Generate(c, fixedNow)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("same config and clock produced different datasets")
	}
	if len(a) != 3*60 {
		t.Fatalf("got %d records, want 180", len(a))
	}
	c2 := c
	c2.Seed = 43
	other, _ := Generate(c2, fixedNow)
	if reflect.DeepEqual(a, other) {
		t.Fatal("different seeds produced identical data")
	}
	lim := event.Limits{TempMinC: -50, TempMaxC: 250, VibMaxMMS: 100, MaxFutureSkew: 5 * time.Minute}
	ids := map[string]bool{}
	for i, r := range a {
		raw, _ := json.Marshal(r)
		ev, rej := event.Parse(raw, lim, fixedNow)
		if rej != nil {
			t.Fatalf("record %d rejected by the service validator: %v (%s)", i, rej, raw)
		}
		if ids[ev.EventID] {
			t.Fatalf("duplicate event id %s", ev.EventID)
		}
		ids[ev.EventID] = true
		if ev.EventTime.After(fixedNow) {
			t.Fatalf("default start must not produce future events: %v", ev.EventTime)
		}
	}
}

func TestExplicitStartAndSequenceRepeatExactly(t *testing.T) {
	c := small()
	c.Start, c.SequenceStart = "2025-01-15T08:00:00Z", 1000
	a, _ := Generate(c, fixedNow)
	b, _ := Generate(c, fixedNow.Add(time.Hour)) // a different clock must not matter
	if !reflect.DeepEqual(a, b) {
		t.Fatal("explicit start and sequence should make the dataset independent of the clock")
	}
	if a[0]["event_id"] != "press-01-1000" || a[0]["event_time"] != "2025-01-15T08:00:00.000Z" {
		t.Fatalf("unexpected first record %v", a[0])
	}
}

func TestAnomaliesCrossTheDefaultThresholds(t *testing.T) {
	c := small()
	c.AnomalyRate, c.DurationS = 0.2, 120
	recs, _ := Generate(c, fixedNow)
	hot := 0
	for _, r := range recs {
		if r["temperature_c"].(float64) >= 85 || r["vibration_mm_s"].(float64) >= 7.1 {
			hot++
		}
	}
	if hot == 0 {
		t.Fatal("expected some readings above the alert thresholds")
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	cases := map[string]func(*Config){
		"devices":     func(c *Config) { c.Devices = 0 },
		"too many":    func(c *Config) { c.Devices, c.DurationS, c.IntervalS = 200, 100000, 1 },
		"rate":        func(c *Config) { c.MalformedRate = 1.5 },
		"batch":       func(c *Config) { c.BatchSize = 1000 },
		"concurrency": func(c *Config) { c.Concurrency = 99 },
		"start":       func(c *Config) { c.Start = "yesterday" },
		"site":        func(c *Config) { c.SiteID = "bad site!" },
		"interval":    func(c *Config) { c.IntervalS = 500 },
	}
	for name, mutate := range cases {
		c := small()
		mutate(&c)
		if err := c.Validate(500); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	if err := small().Validate(500); err != nil {
		t.Fatalf("defaults should validate: %v", err)
	}
}

func TestFaultPlanIsReproducibleAndCounted(t *testing.T) {
	c := small()
	c.MalformedRate, c.DuplicateRate, c.LateRate = 0.1, 0.1, 0.1
	recs, _ := Generate(c, fixedNow)
	p1, n1 := PlanRecords(recs, c)
	p2, n2 := PlanRecords(recs, c)
	if !reflect.DeepEqual(p1, p2) || !reflect.DeepEqual(n1, n2) {
		t.Fatal("same seed should give the same plan")
	}
	kinds := map[string]int{}
	for _, p := range p1 {
		kinds[p.Kind]++
	}
	if kinds["malformed"] != n1.Malformed || kinds["duplicate"] != n1.Duplicates || kinds["late"] != n1.Late {
		t.Fatalf("plan kinds %v do not match counts %+v", kinds, n1)
	}
	if n1.Malformed == 0 || n1.Duplicates == 0 || n1.Late == 0 {
		t.Fatalf("with 10%% rates over 180 records expected every fault to appear: %+v", n1)
	}
	if len(p1) != len(recs)-n1.Malformed+n1.Malformed+n1.Duplicates {
		t.Fatalf("planned %d records from %d with %d duplicates", len(p1), len(recs), n1.Duplicates)
	}
	// Zero rates leave the dataset untouched.
	clean, nc := PlanRecords(recs, small())
	if len(clean) != len(recs) || nc.Malformed+nc.Duplicates+nc.Late != 0 {
		t.Fatal("no faults were requested but some were injected")
	}
	// Each malformed kind is rejected by the real validator.
	lim := event.Limits{TempMinC: -50, TempMaxC: 250, VibMaxMMS: 100, MaxFutureSkew: 5 * time.Minute}
	for _, kind := range malformedKinds {
		raw, _ := json.Marshal(corrupt(recs[0], kind))
		if _, rej := event.Parse(raw, lim, fixedNow); rej == nil {
			t.Errorf("malformed kind %q was accepted by the validator", kind)
		}
	}
}

func TestBurstsReleaseBatchesTogether(t *testing.T) {
	c := small()
	c.RatePerS, c.BatchSize, c.BurstEvery, c.BurstSize = 10, 10, 3, 3
	recs, _ := Generate(c, fixedNow)
	p, _ := PlanRecords(recs, c)
	_, sched := Schedule(p, c)
	if sched[3] != sched[4] || sched[4] != sched[5] || sched[5] == sched[6] {
		t.Fatalf("batches 3-5 should share a send time and 6 should differ: %v", sched[:8])
	}
}

func TestRunnerEndToEndWithBackpressure(t *testing.T) {
	var posts, throttled atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Test") != "yes" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		n := posts.Add(1)
		var req struct {
			Events []json.RawMessage `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if n%4 == 0 { // every 4th request is turned away once
			throttled.Add(1)
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"accepted":0,"rejected":0}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{"accepted": len(req.Events) - 1, "rejected": 1, "rejection_counts": map[string]int{"missing_field": 1}})
	}))
	defer srv.Close()

	c := small()
	c.BatchSize, c.Concurrency, c.Retries = 20, 3, 5
	run := NewRunner()
	if _, err := run.Start(c, Target{BaseURL: srv.URL, Headers: map[string]string{"X-Test": "yes"}}, 500); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var snap Snapshot
	for time.Now().Before(deadline) {
		if snap = run.Snapshot(); snap.State != StateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.State != StateDone {
		t.Fatalf("run did not finish: %s", snap)
	}
	batches := 180 / 20
	if snap.Batches != batches || snap.BatchesDone != batches || snap.RecordsDone != 180 {
		t.Fatalf("progress: %+v", snap)
	}
	if snap.Throttled == 0 || snap.Retries != snap.Throttled || int32(snap.Throttled) != throttled.Load() {
		t.Fatalf("throttled=%d retries=%d server saw %d", snap.Throttled, snap.Retries, throttled.Load())
	}
	if snap.Accepted != 19*batches || snap.Rejected != batches || snap.RejectionCounts["missing_field"] != batches {
		t.Fatalf("accepted=%d rejected=%d counts=%v", snap.Accepted, snap.Rejected, snap.RejectionCounts)
	}
	if snap.RequestErrors != 0 || snap.GaveUpRecords != 0 || snap.Latency["count"].(int) != int(posts.Load()) {
		t.Fatalf("errors=%d gaveUp=%d latency=%v posts=%d", snap.RequestErrors, snap.GaveUpRecords, snap.Latency, posts.Load())
	}
}

func TestRunnerRefusesSecondRunAndCanBeStopped(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-block:
		case <-r.Context().Done(): // the runner abandons the request when it is stopped
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"accepted":1}`))
	}))
	defer srv.Close()
	defer close(block)

	c := small()
	c.RatePerS, c.Concurrency, c.BatchSize = 5, 1, 5
	run := NewRunner()
	if _, err := run.Start(c, Target{BaseURL: srv.URL}, 500); err != nil {
		t.Fatal(err)
	}
	if _, err := run.Start(c, Target{BaseURL: srv.URL}, 500); err != ErrBusy {
		t.Fatalf("second start: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	snap := run.Stop()
	if snap.State != StateStopped {
		t.Fatalf("state after stop: %s", snap.State)
	}
	if !strings.Contains(snap.String(), "stopped") {
		t.Fatal("summary should mention the state")
	}
	if _, err := run.Start(c, Target{BaseURL: srv.URL}, 500); err != nil {
		t.Fatalf("a new run should be allowed after stop: %v", err)
	}
	run.Stop()
}

func TestRunnerReportsTransportErrors(t *testing.T) {
	c := small()
	run := NewRunner()
	if _, err := run.Start(c, Target{BaseURL: "http://127.0.0.1:1"}, 500); err != nil { // nothing listens on port 1
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	var snap Snapshot
	for time.Now().Before(deadline) {
		if snap = run.Snapshot(); snap.State != StateRunning {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if snap.State != StateDone || snap.RequestErrors == 0 || snap.ErroredRecords != 180 || snap.Accepted != 0 {
		t.Fatalf("expected every batch to error: %+v", snap)
	}
}

func TestAutomaticSequencesOfSeparateRunsNeverOverlap(t *testing.T) {
	c := small()
	c.Devices, c.DurationS, c.IntervalS = 5, 100, 1
	first, _ := Generate(c, fixedNow)
	// A second run started only a short while later (here 30 s) must not reuse any
	// (device, sequence) pair of the first one: the database would skip those events.
	second, _ := Generate(c, fixedNow.Add(30*time.Second))
	seen := map[string]bool{}
	for _, r := range first {
		seen[fmt.Sprintf("%v/%v", r["device_id"], r["sequence"])] = true
	}
	for _, r := range second {
		if k := fmt.Sprintf("%v/%v", r["device_id"], r["sequence"]); seen[k] {
			t.Fatalf("(device, sequence) %s appears in both runs", k)
		}
	}
	// And the ids stay valid for the service (at most 64 characters).
	for _, r := range second {
		if len(r["event_id"].(string)) > 64 {
			t.Fatalf("event id too long: %v", r["event_id"])
		}
	}
}
