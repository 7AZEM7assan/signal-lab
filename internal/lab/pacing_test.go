package lab

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorded builds n planned records one `step` seconds apart, in batches of one record.
func recorded(n int, step float64) []Planned {
	out := make([]Planned, n)
	for i := range out {
		out[i] = Planned{Body: []byte(`{}`), Kind: "ok", TRel: float64(i) * step}
	}
	return out
}

func scheduleOf(p []Planned, mutate func(*Config)) []float64 {
	c := small()
	c.BatchSize, c.RatePerS = 1, 0
	mutate(&c)
	_, s := Schedule(p, c)
	return s
}

func TestSpeedFollowsTheRecordedTime(t *testing.T) {
	p := recorded(4, 10) // readings 0, 10, 20 and 30 seconds apart in the file
	for _, tc := range []struct {
		speed float64
		want  []float64
	}{
		{1, []float64{0, 10, 20, 30}},
		{10, []float64{0, 1, 2, 3}},
		{0.5, []float64{0, 20, 40, 60}},
	} {
		got := scheduleOf(p, func(c *Config) { c.Speed = tc.speed })
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("speed %g: schedule %v, want %v", tc.speed, got, tc.want)
			}
		}
	}
}

func TestSpeedOffKeepsTheRatePacingAsBefore(t *testing.T) {
	p := recorded(4, 10)
	got := scheduleOf(p, func(c *Config) { c.RatePerS = 2 })
	want := []float64{0, 0.5, 1, 1.5}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rate pacing changed: %v, want %v", got, want)
		}
	}
	if all := scheduleOf(p, func(c *Config) {}); all[3] != 0 {
		t.Fatalf("unpaced should send at once: %v", all)
	}
}

func TestTheRateStillCapsAFastSpeed(t *testing.T) {
	// Recorded one second apart, replayed at 1000x would be 1 ms apart, but 10 records/s is the limit.
	got := scheduleOf(recorded(5, 1), func(c *Config) { c.Speed, c.RatePerS = 1000, 10 })
	for i, g := range got {
		if want := float64(i) / 10; g != want {
			t.Fatalf("schedule %v: batch %d at %v, want %v", got, i, g, want)
		}
	}
}

func TestAFileOutOfOrderNeverGoesBackInTime(t *testing.T) {
	p := recorded(4, 10)
	p[2].TRel = 5 // older than the reading before it
	got := scheduleOf(p, func(c *Config) { c.Speed = 1 })
	want := []float64{0, 10, 10, 30}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("schedule %v, want %v", got, want)
		}
	}
	// A record before the first one (negative offset) is sent at the start, not before it.
	p[0].TRel = -5
	if got := scheduleOf(p, func(c *Config) { c.Speed = 1 }); got[0] != 0 {
		t.Fatalf("negative offset: %v", got)
	}
}

func TestSpeedIsValidated(t *testing.T) {
	for _, tc := range []struct {
		speed float64
		ok    bool
	}{{0, true}, {1, true}, {MaxSpeed, true}, {-1, false}, {MaxSpeed + 1, false}} {
		c := small()
		c.Speed = tc.speed
		err := c.Validate(500)
		if (err == nil) != tc.ok {
			t.Errorf("speed %v: err = %v", tc.speed, err)
		}
	}
	// Towards a service that is not on this computer the rate limit still applies when following recorded time.
	c := external("http://203.0.113.9/ingest")
	c.TargetConfirmed, c.Speed, c.RatePerS = true, 1, 0
	if err := c.Validate(500); err == nil || !strings.Contains(err.Error(), "rate must be between 1 and") {
		t.Fatalf("a remote service must keep its rate limit: %v", err)
	}
}

func TestAReplayThatWouldTakeTooLongIsRefusedWithAHint(t *testing.T) {
	d := parse(t, "long.csv", "timestamp,device,temp,vibration\n2020-01-01T00:00:00Z,a-1,50,1\n2020-01-03T00:00:00Z,a-1,51,1\n")
	run := NewRunner()
	run.SetDataset(d)
	c := small()
	c.UseDataset, c.BatchSize, c.Speed = true, 1, 1
	c.TargetURL = "http://127.0.0.1:1/x"
	_, err := run.Start(c, Target{}, 500)
	if err == nil || !strings.Contains(err.Error(), "48.0 h") || !strings.Contains(err.Error(), "raise the speed") {
		t.Fatalf("expected a hint about the 48 hour replay: %v", err)
	}
	c.Speed = 1000 // 48 h / 1000 is under 3 minutes
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatalf("a faster speed should be accepted: %v", err)
	}
	run.Stop()
}

func TestTheRunnerSendsAtTheRecordedPaceAndReportsTheDuration(t *testing.T) {
	var mu sync.Mutex
	var at []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		mu.Lock()
		at = append(at, time.Now())
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// Three readings 0.4 s apart in the file.
	csv := "timestamp,device,temp,vibration\n"
	for i := 0; i < 3; i++ {
		csv += fmt.Sprintf("2020-01-01T00:00:00.%03dZ,a-1,50,1\n", i*400)
	}
	run := NewRunner()
	run.SetDataset(parse(t, "slow.csv", csv))
	c := small()
	c.UseDataset, c.BatchSize, c.Speed, c.TargetURL = true, 1, 1, srv.URL
	snap, err := run.Start(c, Target{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if snap.PlannedDurationS < 0.79 || snap.PlannedDurationS > 0.81 {
		t.Fatalf("planned duration = %v, want about 0.8", snap.PlannedDurationS)
	}
	waitDone(t, run)
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 {
		t.Fatalf("requests = %d", len(at))
	}
	if gap := at[2].Sub(at[0]); gap < 700*time.Millisecond || gap > 2*time.Second {
		t.Fatalf("first to last request took %v, want about 800 ms", gap)
	}
}
