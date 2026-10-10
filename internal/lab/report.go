package lab

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"signallab/internal/event"
)

// DataReport is the "check my data" result for an imported file. It looks at the whole file once,
// without sending anything, and says in plain words what a replay of it would run into.
type DataReport struct {
	Rows              int              `json:"rows"` // data rows, not counting a CSV header
	Findings          []Finding        `json:"findings"`
	Rejections        []RejectionGroup `json:"rejections,omitempty"` // rows Signal Lab's own checks would reject, by reason
	DuplicateIDs      int              `json:"duplicate_ids"`        // rows whose event_id appeared in an earlier row
	DuplicateReadings int              `json:"duplicate_readings"`   // rows with the same device and time as an earlier row
	OutOfOrder        int              `json:"out_of_order"`         // rows older than an earlier row of the same device
	Gaps              int              `json:"gaps"`                 // unusually long silences, per device
	StuckRuns         int              `json:"stuck_runs"`           // a sensor repeating one value many times in a row
	Temperature       *ValueStats      `json:"temperature_c,omitempty"`
	Vibration         *ValueStats      `json:"vibration_mm_s,omitempty"`
	DevicesTotal      int              `json:"devices_total"`
	Devices           []DeviceReport   `json:"devices,omitempty"` // the ones with the most findings first, at most maxReportDevices
}

// Finding is one line of the report. Level is "problem", "warning" or "ok".
type Finding struct {
	Level string `json:"level"`
	Text  string `json:"text"`
}

// RejectionGroup counts rows rejected for one reason and shows a few of them.
type RejectionGroup struct {
	Reason   string       `json:"reason"`
	Label    string       `json:"label"` // the reason in everyday words
	Count    int          `json:"count"`
	Examples []RowExample `json:"examples,omitempty"`
}

// RowExample points at a row of the file (counted after the header) and says what is wrong with it.
type RowExample struct {
	Row    int    `json:"row"`
	Detail string `json:"detail"`
}

// ValueStats summarises one measurement over the rows that have a number in it.
type ValueStats struct {
	Count int     `json:"count"`
	Min   float64 `json:"min"`
	Max   float64 `json:"max"`
	Mean  float64 `json:"mean"`
}

// DeviceReport is the per-device part of the report.
type DeviceReport struct {
	Device         string  `json:"device"`
	Rows           int     `json:"rows"`
	FirstTime      string  `json:"first_time,omitempty"`
	LastTime       string  `json:"last_time,omitempty"`
	UsualSeconds   float64 `json:"usual_interval_seconds,omitempty"` // the typical time between readings
	LongestSeconds float64 `json:"longest_gap_seconds,omitempty"`
	Gaps           int     `json:"gaps"`
	OutOfOrder     int     `json:"out_of_order"`
	StuckRuns      int     `json:"stuck_runs"`
}

const (
	maxReportDevices   = 20
	maxRejectExamples  = 3
	minIntervalsForGap = 5  // readings needed before "usual spacing" means anything
	gapFactor          = 3  // a silence this many times the usual spacing is reported
	stuckRunLength     = 10 // this many identical values in a row look like a stuck sensor
)

type point struct {
	t       time.Time
	temp    float64
	vib     float64
	hasTemp bool
	hasVib  bool
}

// analyze checks every record once. It returns the per-reason counts that DatasetSummary.Problems has
// always held, and the full report.
func analyze(records []Record, lim event.Limits, now time.Time, haveID bool) (map[string]int, *DataReport) {
	rep := &DataReport{Rows: len(records)}
	problems := map[string]int{}
	groups := map[string]*RejectionGroup{}

	ids := map[string]bool{}
	readings := map[string]bool{}
	type devState struct {
		points  []point
		last    time.Time
		ooo     int
		rows    int
		devName string
	}
	devs := map[string]*devState{}
	var temp, vib statsAcc

	for i, rec := range records {
		if raw, err := json.Marshal(rec); err != nil {
			problems[event.ReasonMalformedJSON]++
		} else if _, rej := event.Parse(raw, lim, now); rej != nil {
			problems[rej.Reason]++
			g := groups[rej.Reason]
			if g == nil {
				g = &RejectionGroup{Reason: rej.Reason, Label: reasonLabel(rej.Reason)}
				groups[rej.Reason] = g
			}
			g.Count++
			if len(g.Examples) < maxRejectExamples {
				g.Examples = append(g.Examples, RowExample{Row: i + 1, Detail: rej.Detail})
			}
		}

		if haveID {
			if id, ok := rec["event_id"].(string); ok && id != "" {
				if ids[id] {
					rep.DuplicateIDs++
				}
				ids[id] = true
			}
		}
		dev, _ := rec["device_id"].(string)
		ts, _ := rec["event_time"].(string)
		if dev != "" && ts != "" {
			key := dev + "\x00" + ts
			if readings[key] {
				rep.DuplicateReadings++
			}
			readings[key] = true
		}

		p := point{}
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			p.t = t
		}
		if v, ok := numberOf(rec["temperature_c"]); ok {
			p.temp, p.hasTemp = v, true
			temp.add(v)
		}
		if v, ok := numberOf(rec["vibration_mm_s"]); ok {
			p.vib, p.hasVib = v, true
			vib.add(v)
		}
		if dev == "" {
			continue
		}
		d := devs[dev]
		if d == nil {
			d = &devState{devName: dev}
			devs[dev] = d
		}
		d.rows++
		if !p.t.IsZero() {
			if !d.last.IsZero() && p.t.Before(d.last) {
				d.ooo++
				rep.OutOfOrder++
			} else {
				d.last = p.t
			}
			d.points = append(d.points, p)
		}
	}

	rep.Temperature, rep.Vibration = temp.stats(), vib.stats()
	for _, g := range groups {
		rep.Rejections = append(rep.Rejections, *g)
	}
	sort.Slice(rep.Rejections, func(i, j int) bool {
		if rep.Rejections[i].Count != rep.Rejections[j].Count {
			return rep.Rejections[i].Count > rep.Rejections[j].Count
		}
		return rep.Rejections[i].Reason < rep.Rejections[j].Reason
	})

	var all []DeviceReport
	for name, d := range devs {
		dr := DeviceReport{Device: name, Rows: d.rows, OutOfOrder: d.ooo}
		pts := append([]point(nil), d.points...)
		sort.SliceStable(pts, func(i, j int) bool { return pts[i].t.Before(pts[j].t) })
		if len(pts) > 0 {
			dr.FirstTime, dr.LastTime = FormatTime(pts[0].t), FormatTime(pts[len(pts)-1].t)
		}
		var gaps []float64
		for k := 1; k < len(pts); k++ {
			if s := pts[k].t.Sub(pts[k-1].t).Seconds(); s > 0 {
				gaps = append(gaps, s)
			}
		}
		if len(gaps) >= minIntervalsForGap {
			sorted := append([]float64(nil), gaps...)
			sort.Float64s(sorted)
			usual := sorted[len(sorted)/2]
			dr.UsualSeconds = usual
			for _, g := range gaps {
				if g > usual*gapFactor {
					dr.Gaps++
					if g > dr.LongestSeconds {
						dr.LongestSeconds = g
					}
				}
			}
		}
		dr.StuckRuns = stuckRuns(pts, func(p point) (float64, bool) { return p.temp, p.hasTemp }) +
			stuckRuns(pts, func(p point) (float64, bool) { return p.vib, p.hasVib })
		rep.Gaps += dr.Gaps
		rep.StuckRuns += dr.StuckRuns
		all = append(all, dr)
	}
	rep.DevicesTotal = len(all)
	sort.Slice(all, func(i, j int) bool {
		a, b := all[i].Gaps+all[i].OutOfOrder+all[i].StuckRuns, all[j].Gaps+all[j].OutOfOrder+all[j].StuckRuns
		if a != b {
			return a > b
		}
		return all[i].Device < all[j].Device
	})
	if len(all) > maxReportDevices {
		all = all[:maxReportDevices]
	}
	rep.Devices = all
	rep.Findings = findings(rep)
	if len(problems) == 0 {
		problems = nil
	}
	return problems, rep
}

// stuckRuns counts runs of at least stuckRunLength identical consecutive values.
func stuckRuns(pts []point, get func(point) (float64, bool)) int {
	runs, length := 0, 0
	var prev float64
	have := false
	for _, p := range pts {
		v, ok := get(p)
		if !ok {
			have, length = false, 0
			continue
		}
		if have && v == prev {
			length++
		} else {
			length = 1
		}
		if length == stuckRunLength {
			runs++
		}
		prev, have = v, true
	}
	return runs
}

func numberOf(v any) (float64, bool) {
	switch x := v.(type) {
	case json.Number:
		f, err := strconv.ParseFloat(string(x), 64)
		return f, err == nil && !math.IsNaN(f) && !math.IsInf(f, 0)
	case float64:
		return x, !math.IsNaN(x) && !math.IsInf(x, 0)
	case int:
		return float64(x), true
	}
	return 0, false
}

type statsAcc struct {
	n        int
	min, max float64
	sum      float64
}

func (s *statsAcc) add(v float64) {
	if s.n == 0 || v < s.min {
		s.min = v
	}
	if s.n == 0 || v > s.max {
		s.max = v
	}
	s.n++
	s.sum += v
}

func (s *statsAcc) stats() *ValueStats {
	if s.n == 0 {
		return nil
	}
	return &ValueStats{Count: s.n, Min: s.min, Max: s.max, Mean: s.sum / float64(s.n)}
}

func humanSeconds(s float64) string {
	switch {
	case s < 90:
		return fmt.Sprintf("%.0f s", s)
	case s < 90*60:
		return fmt.Sprintf("%.1f min", s/60)
	default:
		return fmt.Sprintf("%.1f h", s/3600)
	}
}

// reasonLabel says a rejection reason in everyday words, for the findings and the details.
func reasonLabel(reason string) string {
	switch reason {
	case event.ReasonMalformedJSON:
		return "value of the wrong type or unreadable"
	case event.ReasonUnknownField:
		return "unknown field"
	case event.ReasonUnsupportedSchema:
		return "unsupported schema version"
	case event.ReasonMissingField:
		return "missing value"
	case event.ReasonInvalidEventID:
		return "invalid event id"
	case event.ReasonInvalidDeviceID:
		return "invalid device id"
	case event.ReasonInvalidSiteID:
		return "invalid site id"
	case event.ReasonInvalidTimestamp:
		return "invalid time"
	case event.ReasonTimeInFuture:
		return "time in the future"
	case event.ReasonInvalidSequence:
		return "invalid sequence number"
	case event.ReasonOutOfRange:
		return "value out of range"
	case event.ReasonDuplicateInBatch:
		return "repeated within one batch"
	}
	return "(" + strings.ReplaceAll(reason, "_", " ") + ")"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// findings turns the numbers into the short sentences the app shows. The order is: problems first.
func findings(r *DataReport) []Finding {
	var out []Finding
	if rejected := 0; len(r.Rejections) > 0 {
		for _, g := range r.Rejections {
			rejected += g.Count
		}
		parts := make([]string, 0, len(r.Rejections))
		for _, g := range r.Rejections {
			parts = append(parts, fmt.Sprintf("%d %s", g.Count, g.Label))
		}
		out = append(out, Finding{"problem", fmt.Sprintf("%d of %d rows (%.1f%%) would be rejected by Signal Lab's own checks: %s.",
			rejected, r.Rows, 100*float64(rejected)/float64(r.Rows), strings.Join(parts, ", "))})
	}
	if r.DuplicateIDs > 0 {
		out = append(out, Finding{"warning", fmt.Sprintf("%d %s event_id already used by an earlier row. A service that ignores repeated ids will store %s once.",
			r.DuplicateIDs, plural(r.DuplicateIDs, "row has an", "rows have an"), plural(r.DuplicateIDs, "it", "them"))})
	}
	if r.DuplicateReadings > 0 {
		out = append(out, Finding{"warning", fmt.Sprintf("%d %s the same device and time as an earlier row.",
			r.DuplicateReadings, plural(r.DuplicateReadings, "row has", "rows have"))})
	}
	if r.OutOfOrder > 0 {
		out = append(out, Finding{"warning", fmt.Sprintf("%d %s older than an earlier row of the same device, so the file is not in time order for %s.",
			r.OutOfOrder, plural(r.OutOfOrder, "row is", "rows are"), plural(r.OutOfOrder, "it", "them"))})
	}
	if r.Gaps > 0 {
		worst := worstDevice(r.Devices, func(d DeviceReport) float64 { return d.LongestSeconds })
		out = append(out, Finding{"warning", fmt.Sprintf("%d unusually long %s in the readings (more than %d times the usual spacing). The longest is %s on %s, where readings usually come every %s.",
			r.Gaps, plural(r.Gaps, "silence", "silences"), gapFactor, humanSeconds(worst.LongestSeconds), worst.Device, humanSeconds(worst.UsualSeconds))})
	}
	if r.StuckRuns > 0 {
		worst := worstDevice(r.Devices, func(d DeviceReport) float64 { return float64(d.StuckRuns) })
		out = append(out, Finding{"warning", fmt.Sprintf("%d %s of %d or more identical values in a row, which can mean a stuck sensor (most on %s).",
			r.StuckRuns, plural(r.StuckRuns, "run", "runs"), stuckRunLength, worst.Device)})
	}
	if len(out) == 0 {
		out = append(out, Finding{"ok", "No problems found: every row passes Signal Lab's checks, no repeated rows, the times are in order and there are no long silences."})
	}
	return out
}

func worstDevice(ds []DeviceReport, score func(DeviceReport) float64) DeviceReport {
	var best DeviceReport
	for i, d := range ds {
		if i == 0 || score(d) > score(best) {
			best = d
		}
	}
	return best
}
