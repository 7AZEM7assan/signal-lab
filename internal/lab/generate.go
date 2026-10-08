// Package lab is the in-process counterpart of the Python simulator: a seeded multi-device
// generator, explicit fault injection, and a paced, batching replayer that talks to the
// service over HTTP exactly like any other client.
//
// It is used by the desktop app so a user can run a replay from the UI without Python. The
// generator is deterministic for a given Config, but it is NOT byte-identical to the Python
// tool's output (the two use different random number generators); the shapes, units and
// fault semantics match.
package lab

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"time"
)

var deviceKinds = []string{"press", "pump", "mill", "lathe", "conveyor"}

// Hard limits protect the app from a typo asking for millions of records.
const (
	MaxDevices     = 200
	MaxRecords     = 500_000
	MaxConcurrency = 16
	MaxRetries     = 100
)

// Config describes one replay: what to generate, how to send it, and which faults to inject.
type Config struct {
	// Dataset.
	Seed        int64   `json:"seed"`
	Devices     int     `json:"devices"`
	DurationS   float64 `json:"duration_s"` // seconds of simulated device time
	IntervalS   float64 `json:"interval_s"` // seconds between readings per device
	AnomalyRate float64 `json:"anomaly_rate"`
	SiteID      string  `json:"site_id"`
	// Start is the first reading's time (RFC 3339). Empty means "so the last reading is now",
	// which keeps every timestamp recent and valid. SequenceStart is the first sequence number;
	// 0 derives one from the clock so repeated runs never collide. Set both (and the seed) to
	// resend the exact same events and watch idempotency skip them.
	Start         string `json:"start"`
	SequenceStart int64  `json:"sequence_start"`

	// Sending.
	RatePerS    float64 `json:"rate_per_s"` // target records per second; 0 = as fast as possible
	BatchSize   int     `json:"batch_size"`
	Concurrency int     `json:"concurrency"`
	Retries     int     `json:"retries"` // extra attempts after a 429/503
	TimeoutS    float64 `json:"timeout_s"`

	// Faults (all opt-in, all seeded).
	MalformedRate float64 `json:"malformed_rate"`
	DuplicateRate float64 `json:"duplicate_rate"`
	LateRate      float64 `json:"late_rate"`
	LateSeconds   float64 `json:"late_seconds"`
	BurstEvery    int     `json:"burst_every"` // every Nth batch starts a burst (0 = off)
	BurstSize     int     `json:"burst_size"`  // batches released together in a burst
	JitterMS      float64 `json:"jitter_ms"`
}

// Defaults returns a small, quick run with no faults.
func Defaults() Config {
	return Config{
		Seed: 42, Devices: 5, DurationS: 120, IntervalS: 2, AnomalyRate: 0.02, SiteID: "plant-a",
		RatePerS: 100, BatchSize: 20, Concurrency: 2, Retries: 20, TimeoutS: 10, LateSeconds: 120,
	}
}

// Validate checks ranges and limits. maxBatch is the service's per-request event limit.
func (c Config) Validate(maxBatch int) error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.Devices < 1 || c.Devices > MaxDevices {
		bad("devices must be 1-%d", MaxDevices)
	}
	if c.IntervalS <= 0 || c.DurationS <= 0 {
		bad("duration and interval must be positive")
	} else if c.IntervalS > c.DurationS {
		bad("interval must not exceed duration")
	} else if n := int(c.DurationS/c.IntervalS) * c.Devices; n > MaxRecords {
		bad("that would generate %d records; the limit is %d", n, MaxRecords)
	}
	if c.AnomalyRate < 0 || c.AnomalyRate > 1 {
		bad("anomaly rate must be within [0, 1]")
	}
	for name, v := range map[string]float64{"malformed rate": c.MalformedRate, "duplicate rate": c.DuplicateRate, "late rate": c.LateRate} {
		if v < 0 || v > 1 {
			bad("%s must be within [0, 1]", name)
		}
	}
	if c.RatePerS < 0 {
		bad("rate must be 0 (unpaced) or positive")
	}
	if c.BatchSize < 1 || (maxBatch > 0 && c.BatchSize > maxBatch) {
		bad("batch size must be 1-%d", maxBatch)
	}
	if c.Concurrency < 1 || c.Concurrency > MaxConcurrency {
		bad("concurrency must be 1-%d", MaxConcurrency)
	}
	if c.Retries < 0 || c.Retries > MaxRetries {
		bad("retries must be 0-%d", MaxRetries)
	}
	if c.TimeoutS <= 0 || c.TimeoutS > 120 {
		bad("timeout must be within (0, 120] seconds")
	}
	if c.LateSeconds < 0 || c.JitterMS < 0 || c.BurstEvery < 0 || c.BurstSize < 0 {
		bad("late seconds, jitter, burst every and burst size must not be negative")
	}
	if c.SiteID != "" && !validID(c.SiteID) {
		bad("site id may only contain letters, digits and . _ : - (up to 64 characters)")
	}
	if c.SequenceStart < 0 {
		bad("sequence start must not be negative")
	}
	if c.Start != "" {
		if _, err := time.Parse(time.RFC3339, c.Start); err != nil {
			bad("start must be an RFC 3339 time such as 2025-01-15T08:00:00Z")
		}
	}
	return errors.Join(errs...)
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 64 {
		return false
	}
	for i, r := range s {
		alnum := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
		if !alnum && (i == 0 || !(r == '.' || r == '_' || r == ':' || r == '-')) {
			return false
		}
	}
	return true
}

// Record is one telemetry reading as sent on the wire.
type Record map[string]any

// DeviceIDs returns n device names such as press-01, pump-02, mill-03.
func DeviceIDs(n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%02d", deviceKinds[i%len(deviceKinds)], i+1)
	}
	return ids
}

// FormatTime is RFC 3339 UTC with millisecond precision.
func FormatTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

type episode struct {
	left int
	kind int // 0 temperature, 1 vibration, 2 both
}

// Generate returns the readings ordered by time then device. now anchors an empty Start.
func Generate(c Config, now time.Time) ([]Record, error) {
	if err := c.Validate(0); err != nil {
		return nil, err
	}
	rng := rand.New(rand.NewPCG(uint64(c.Seed), 0x5167a1ab))
	steps := int(c.DurationS / c.IntervalS)
	var start time.Time
	if c.Start == "" {
		start = now.UTC().Add(-time.Duration(c.DurationS * float64(time.Second))).Truncate(time.Millisecond)
	} else {
		start, _ = time.Parse(time.RFC3339, c.Start)
	}
	seq0 := c.SequenceStart
	if seq0 == 0 {
		// A fresh base for every run. Sequence numbers are unique per device in the database, so two
		// runs that overlapped here would have their events silently skipped as duplicates.
		// Microseconds since the epoch leave a run of N steps clear of any run started more than
		// N microseconds later, which separate (non-overlapping) runs always are.
		seq0 = now.UnixMicro()
	}
	site := c.SiteID

	ids := DeviceIDs(c.Devices)
	baseTemp := make(map[string]float64, len(ids))
	baseVib := make(map[string]float64, len(ids))
	phase := make(map[string]float64, len(ids))
	for _, d := range ids {
		baseTemp[d] = 55 + 10*rng.Float64()
		baseVib[d] = 1.5 + 2*rng.Float64()
		phase[d] = 2 * math.Pi * rng.Float64()
	}
	active := map[string]episode{}

	out := make([]Record, 0, steps*len(ids))
	for k := 0; k < steps; k++ {
		t := start.Add(time.Duration(math.Round(float64(k)*c.IntervalS*1000)) * time.Millisecond)
		elapsed := float64(k) * c.IntervalS
		for _, d := range ids {
			temp := baseTemp[d] + 4*math.Sin(2*math.Pi*elapsed/300+phase[d]) + rng.NormFloat64()*0.4
			vib := math.Max(0, baseVib[d]+rng.NormFloat64()*0.25)

			if _, on := active[d]; !on && rng.Float64() < c.AnomalyRate {
				active[d] = episode{left: 3 + rng.IntN(4), kind: rng.IntN(3)}
			}
			if ep, on := active[d]; on {
				if ep.kind == 0 || ep.kind == 2 {
					temp += 40
				}
				if ep.kind == 1 || ep.kind == 2 {
					vib += 7
				}
				if ep.left <= 1 {
					delete(active, d)
				} else {
					active[d] = episode{left: ep.left - 1, kind: ep.kind}
				}
			}

			seq := seq0 + int64(k)
			r := Record{
				"schema_version": 1,
				"event_id":       fmt.Sprintf("%s-%d", d, seq),
				"device_id":      d,
				"event_time":     FormatTime(t),
				"sequence":       seq,
				"temperature_c":  round2(temp),
				"vibration_mm_s": round2(vib),
			}
			if site != "" {
				r["site_id"] = site
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
