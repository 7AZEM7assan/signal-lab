package lab

import (
	"encoding/json"
	"math/rand/v2"
	"time"
)

var malformedKinds = []string{"missing_field", "wrong_type", "out_of_range", "bad_timestamp", "bad_schema_version", "bad_id"}

// Planned is one record to send: its JSON body, the fault applied (if any) and its offset
// in dataset time, used only for pacing by recorded time.
type Planned struct {
	Body []byte
	Kind string  // ok | malformed | duplicate | late
	TRel float64 // seconds since the first record, before any fault
}

// FaultCounts says how many faults were actually injected.
type FaultCounts struct {
	Malformed       int            `json:"malformed"`
	Duplicates      int            `json:"duplicates"`
	Late            int            `json:"late"`
	MalformedByKind map[string]int `json:"malformed_by_kind,omitempty"`
}

func corrupt(rec Record, kind string) Record {
	out := make(Record, len(rec))
	for k, v := range rec {
		out[k] = v
	}
	switch kind {
	case "missing_field":
		delete(out, "temperature_c")
	case "wrong_type":
		out["temperature_c"] = "hot"
	case "out_of_range":
		out["temperature_c"] = 9999.0
	case "bad_timestamp":
		out["event_time"] = "not-a-timestamp"
	case "bad_schema_version":
		out["schema_version"] = 99
	case "bad_id":
		out["event_id"] = "bad id!"
	}
	return out
}

// PlanRecords turns dataset records into the exact sequence that will be sent. Each record
// always draws the same number of random values, so one fault's rate never shifts the random
// stream seen by the others: the same dataset and Config give the same plan.
func PlanRecords(records []Record, c Config) ([]Planned, FaultCounts) {
	rng := rand.New(rand.NewPCG(uint64(c.Seed), 0xfa17))
	counts := FaultCounts{MalformedByKind: map[string]int{}}
	planned := make([]Planned, 0, len(records))
	var first time.Time
	if len(records) > 0 {
		first, _ = time.Parse(time.RFC3339, timeOf(records[0]))
	}
	for _, rec := range records {
		uMalformed, uLate, uDup := rng.Float64(), rng.Float64(), rng.Float64()
		kindChoice := malformedKinds[rng.IntN(len(malformedKinds))]

		tRel := 0.0
		if ts, err := time.Parse(time.RFC3339, timeOf(rec)); err == nil {
			tRel = ts.Sub(first).Seconds()
		}

		if uMalformed < c.MalformedRate {
			counts.Malformed++
			counts.MalformedByKind[kindChoice]++
			planned = append(planned, Planned{Body: mustJSON(corrupt(rec, kindChoice)), Kind: "malformed", TRel: tRel})
			continue
		}
		kind := "ok"
		if uLate < c.LateRate {
			late := make(Record, len(rec))
			for k, v := range rec {
				late[k] = v
			}
			if ts, err := time.Parse(time.RFC3339, timeOf(rec)); err == nil {
				late["event_time"] = FormatTime(ts.Add(-time.Duration(c.LateSeconds * float64(time.Second))))
			}
			rec = late
			counts.Late++
			kind = "late"
		}
		body := mustJSON(rec)
		planned = append(planned, Planned{Body: body, Kind: kind, TRel: tRel})
		if uDup < c.DuplicateRate {
			counts.Duplicates++
			planned = append(planned, Planned{Body: body, Kind: "duplicate", TRel: tRel})
		}
	}
	if len(counts.MalformedByKind) == 0 {
		counts.MalformedByKind = nil
	}
	return planned, counts
}

// timeOf is the record's event_time text, or "" when it is missing or not text (imported files
// may contain anything).
func timeOf(r Record) string {
	s, _ := r["event_time"].(string)
	return s
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // Records only hold JSON-safe values
	}
	return b
}

// Schedule groups records into batches and returns each batch's send offset in seconds.
func Schedule(planned []Planned, c Config) ([][]Planned, []float64) {
	var batches [][]Planned
	for i := 0; i < len(planned); i += c.BatchSize {
		batches = append(batches, planned[i:min(i+c.BatchSize, len(planned))])
	}
	sent := 0
	schedule := make([]float64, len(batches))
	for i, b := range batches {
		if c.RatePerS > 0 {
			schedule[i] = float64(sent) / c.RatePerS
		}
		sent += len(b)
	}
	return batches, applyBursts(schedule, c)
}

// applyBursts releases BurstSize consecutive batches at the burst's first send time.
func applyBursts(schedule []float64, c Config) []float64 {
	if c.BurstEvery <= 0 || c.BurstSize <= 1 {
		return schedule
	}
	out := append([]float64(nil), schedule...)
	for i := c.BurstEvery; i < len(out); i += c.BurstEvery {
		for j := i; j < min(i+c.BurstSize, len(out)); j++ {
			out[j] = schedule[i]
		}
	}
	return out
}
