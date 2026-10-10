package appctl

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"signallab/internal/lab"
)

// ReportInfo is everything a replay report is made from.
type ReportInfo struct {
	Version  string
	Platform string
	Snap     lab.Snapshot
	Timeline lab.Timeline
	Dataset  *lab.DatasetSummary // the imported file, when the run used it
}

const maxReportSeconds = 120 // rows of the per-second table; a longer run is thinned evenly

func mdEscape(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "|", "\\|"), "\n", " ")
}

// Markdown renders the report as plain text a person can read, paste into a ticket or keep.
func (ri ReportInfo) Markdown() string {
	s, tl := ri.Snap, ri.Timeline
	var b strings.Builder
	w := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	w("# Signal Lab replay report\n\n")
	started := "-"
	if s.StartedAt != nil {
		started = s.StartedAt.UTC().Format(time.RFC3339)
	}
	w("Signal Lab %s on %s. Run started %s, state: **%s**, took %.1f s.\n\n", ri.Version, ri.Platform, started, s.State, s.ElapsedS)

	w("## Setup\n\n| | |\n|---|---|\n")
	w("| Sent to | %s |\n| Data | %s |\n", mdEscape(s.Target), mdEscape(s.Source))
	if c := s.Config; c != nil {
		pace := "as fast as possible"
		if c.RatePerS > 0 {
			pace = fmt.Sprintf("%g records/s", c.RatePerS)
		}
		if c.RampToPerS > 0 {
			pace = fmt.Sprintf("ramp from %g to %g records/s over %g s", c.RatePerS, c.RampToPerS, c.RampS)
		}
		if c.Speed > 0 {
			pace += fmt.Sprintf(", following the recorded time at %g x", c.Speed)
		}
		w("| Pace | %s |\n| Batch size / connections | %d / %d |\n| Retries on 429 | %d |\n", pace, c.BatchSize, c.Concurrency, c.Retries)
		if c.MalformedRate+c.DuplicateRate+c.LateRate > 0 || c.BurstEvery > 0 {
			w("| Faults added | malformed %g%%, duplicated %g%%, late %g%%, burst every %d batches of %d |\n",
				100*c.MalformedRate, 100*c.DuplicateRate, 100*c.LateRate, c.BurstEvery, c.BurstSize)
		}
	}
	w("\n## Results\n\n| | |\n|---|---|\n")
	w("| Records sent | %d of %d planned |\n| Accepted | %d |\n| Rejected as invalid | %d |\n", s.RecordsDone, s.Planned, s.Accepted, s.Rejected)
	w("| Told to slow down (429) | %d times, %d retries, %d records given up |\n| Request errors | %d (%d records) |\n", s.Throttled, s.Retries, s.GaveUpRecords, s.RequestErrors, s.ErroredRecords)
	w("| Average throughput | %.0f records/s |\n", s.ThroughputPerS)
	if l := s.Latency; l != nil && l["count"] != nil {
		w("| Request latency | p50 %v ms, p95 %v ms, p99 %v ms, max %v ms |\n", l["p50_ms"], l["p95_ms"], l["p99_ms"], l["max_ms"])
	}
	if len(s.StatusCounts) > 0 {
		codes := make([]string, 0, len(s.StatusCounts))
		for c := range s.StatusCounts {
			codes = append(codes, c)
		}
		sort.Strings(codes)
		parts := make([]string, len(codes))
		for i, c := range codes {
			parts[i] = fmt.Sprintf("%s x %d", c, s.StatusCounts[c])
		}
		w("| Responses | %s |\n", strings.Join(parts, ", "))
	}
	if e := s.FirstError; e != nil {
		w("| First problem | request %d: %s |\n", e.Batch, mdEscape(e.Message+quoted(e.Body)))
	}

	if len(tl.Points) > 0 {
		sm := tl.Summary
		w("\n## Run over time\n\n")
		w("- Sent up to %d records a second; the service accepted up to %d.\n", sm.PeakSentPerS, sm.PeakAcceptedPerS)
		if sm.FirstThrottledAtS != nil {
			w("- First told to slow down (429) at %d s, when about %d records a second were being sent.\n", *sm.FirstThrottledAtS, deref(sm.SentPerSThen))
		}
		if sm.FirstErrorAtS != nil {
			w("- First error at %d s, at about %d records a second.\n", *sm.FirstErrorAtS, deref(sm.SentPerSAtError))
		}
		w("- The slowest 5%% of requests took %v ms at the start and %v ms at the end.\n", sm.P95FirstMs, sm.P95LastMs)
		if tl.Truncated {
			w("- Only the first hour is recorded.\n")
		}
		step := 1
		if len(tl.Points) > maxReportSeconds {
			step = (len(tl.Points) + maxReportSeconds - 1) / maxReportSeconds
		}
		w("\n| Second | Sent | Accepted | Requests | 429 | Errors | p95 ms |\n|---:|---:|---:|---:|---:|---:|---:|\n")
		for i := 0; i < len(tl.Points); i += step {
			p := tl.Points[i]
			w("| %d | %d | %d | %d | %d | %d | %v |\n", p.T, p.Sent, p.Records, p.Requests, p.Throttled, p.Errors, p.P95Ms)
		}
		if step > 1 {
			w("\nEvery %d seconds are shown.\n", step)
		}
	}

	if d := ri.Dataset; d != nil && d.Report != nil {
		w("\n## Data check of %s\n\n%d rows, %d devices.\n\n", mdEscape(d.Name), d.Rows, d.Report.DevicesTotal)
		for _, f := range d.Report.Findings {
			w("- **%s**: %s\n", strings.ToUpper(f.Level), mdEscape(f.Text))
		}
		for _, g := range d.Report.Rejections {
			ex := make([]string, len(g.Examples))
			for i, e := range g.Examples {
				ex[i] = fmt.Sprintf("row %d: %s", e.Row, mdEscape(e.Detail))
			}
			w("  - %d x %s (%s)\n", g.Count, g.Label, strings.Join(ex, "; "))
		}
	}
	w("\n---\nAddresses are shown without their query string and header values are never included. Readings sent to your own service are not stored by Signal Lab. Rows are counted after a file's header.\n")
	return b.String()
}

func quoted(body string) string {
	if body == "" {
		return ""
	}
	return " - " + body
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
