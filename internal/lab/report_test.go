package lab

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// minuteRows builds CSV lines for one device with a reading every minute from a start time.
func minuteRows(device string, start time.Time, n int, value func(i int) float64) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		ts := start.Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(&b, "%s,%s,%.1f,%.1f\n", ts, device, value(i), 1+float64(i%4)/10)
	}
	return b.String()
}

func TestReportOfACleanFileSaysSo(t *testing.T) {
	start := fixedNow.Add(-2 * time.Hour)
	csv := "timestamp,device,temperature,vibration\n" + minuteRows("press-01", start, 30, func(i int) float64 { return 20 + float64(i%7) })
	r := parse(t, "clean.csv", csv).Summary.Report
	if r == nil {
		t.Fatal("no report")
	}
	if r.Rows != 30 || r.DevicesTotal != 1 {
		t.Fatalf("rows/devices = %d/%d", r.Rows, r.DevicesTotal)
	}
	if len(r.Findings) != 1 || r.Findings[0].Level != "ok" {
		t.Fatalf("findings = %+v", r.Findings)
	}
	if r.Temperature == nil || r.Temperature.Min != 20 || r.Temperature.Max != 26 {
		t.Fatalf("temperature stats = %+v", r.Temperature)
	}
	if r.Devices[0].UsualSeconds != 60 {
		t.Fatalf("usual interval = %v", r.Devices[0].UsualSeconds)
	}
}

func TestReportFindsRejectedRowsWithExamples(t *testing.T) {
	start := fixedNow.Add(-2 * time.Hour)
	body := minuteRows("press-01", start, 10, func(i int) float64 { return 20 + float64(i) })
	// row 4 is far out of range, row 7 has a text temperature
	lines := strings.Split(strings.TrimSpace(body), "\n")
	lines[3] = start.Add(3*time.Minute).UTC().Format(time.RFC3339) + ",press-01,999,1.5"
	lines[6] = start.Add(6*time.Minute).UTC().Format(time.RFC3339) + ",press-01,hot,1.5"
	r := parse(t, "bad.csv", "timestamp,device,temperature,vibration\n"+strings.Join(lines, "\n")+"\n").Summary.Report
	total := 0
	rows := map[int]bool{}
	for _, g := range r.Rejections {
		total += g.Count
		for _, e := range g.Examples {
			rows[e.Row] = true
			if e.Detail == "" {
				t.Errorf("example without detail: %+v", e)
			}
		}
	}
	if total != 2 || !rows[4] || !rows[7] {
		t.Fatalf("rejections = %+v (rows %v)", r.Rejections, rows)
	}
	if r.Findings[0].Level != "problem" || !strings.Contains(r.Findings[0].Text, "2 of 10 rows") {
		t.Fatalf("first finding = %+v", r.Findings[0])
	}
	if strings.Contains(r.Findings[0].Text, "_") {
		t.Errorf("the finding should use everyday words, not reason codes: %s", r.Findings[0].Text)
	}
}

func TestReportExamplesAreCappedButCountsAreNot(t *testing.T) {
	start := fixedNow.Add(-2 * time.Hour)
	csv := "timestamp,device,temperature,vibration\n" + minuteRows("press-01", start, 12, func(i int) float64 { return 999 })
	r := parse(t, "all-bad.csv", csv).Summary.Report
	if len(r.Rejections) != 1 || r.Rejections[0].Count != 12 || len(r.Rejections[0].Examples) != maxRejectExamples {
		t.Fatalf("rejections = %+v", r.Rejections)
	}
}

func TestReportFindsDuplicatesOutOfOrderGapsAndStuckSensors(t *testing.T) {
	start := fixedNow.Add(-5 * time.Hour)
	var rows []string
	add := func(minute int, device string, temp float64) {
		rows = append(rows, fmt.Sprintf("%s,%s,%.1f,%.1f", start.Add(time.Duration(minute)*time.Minute).UTC().Format(time.RFC3339), device, temp, 1+float64(minute%4)/10))
	}
	for i := 0; i < 20; i++ {
		add(i, "pump-01", 20+float64(i%5))
	}
	add(5, "pump-01", 21)     // goes back in time (out of order) and repeats the 5th minute (same device and time)
	add(120, "pump-01", 22)   // a silence of about 100 minutes after minute 19
	for i := 0; i < 15; i++ { // a second device whose temperature never changes
		add(i, "mill-02", 55)
	}
	r := parse(t, "messy.csv", "timestamp,device,temperature,vibration\n"+strings.Join(rows, "\n")+"\n").Summary.Report
	if r.DuplicateReadings != 1 {
		t.Errorf("duplicate readings = %d, want 1", r.DuplicateReadings)
	}
	if r.OutOfOrder != 1 {
		t.Errorf("out of order = %d, want 1", r.OutOfOrder)
	}
	if r.Gaps != 1 {
		t.Errorf("gaps = %d, want 1", r.Gaps)
	}
	if r.StuckRuns < 1 {
		t.Errorf("stuck runs = %d, want at least 1", r.StuckRuns)
	}
	var pump DeviceReport
	for _, d := range r.Devices {
		if d.Device == "pump-01" {
			pump = d
		}
	}
	if pump.LongestSeconds < 99*60 || pump.LongestSeconds > 101*60 || pump.UsualSeconds != 60 {
		t.Errorf("pump gap = %+v", pump)
	}
	text := ""
	for _, f := range r.Findings {
		if f.Level == "ok" {
			t.Errorf("an ok finding next to problems: %+v", f)
		}
		text += f.Text + "\n"
	}
	for _, want := range []string{"same device and time", "older than an earlier row", "silence", "stuck sensor"} {
		if !strings.Contains(text, want) {
			t.Errorf("findings do not mention %q:\n%s", want, text)
		}
	}
}

func TestReportCountsRepeatedEventIDsOnlyWhenTheFileHasThem(t *testing.T) {
	start := fixedNow.Add(-time.Hour)
	ts := func(m int) string { return start.Add(time.Duration(m) * time.Minute).UTC().Format(time.RFC3339) }
	withIDs := "event_id,timestamp,device,temperature,vibration\n" +
		"a," + ts(0) + ",press-01,20,1\n" + "b," + ts(1) + ",press-01,21,1\n" + "a," + ts(2) + ",press-01,22,1\n"
	if r := parse(t, "ids.csv", withIDs).Summary.Report; r.DuplicateIDs != 1 {
		t.Errorf("duplicate ids = %d, want 1", r.DuplicateIDs)
	}
	// Without an id column the app makes the ids up from the device and row, so they cannot repeat.
	noIDs := "timestamp,device,temperature,vibration\n" + "" + ts(0) + ",press-01,20,1\n" + ts(1) + ",press-01,21,1\n"
	if r := parse(t, "noids.csv", noIDs).Summary.Report; r.DuplicateIDs != 0 {
		t.Errorf("duplicate ids = %d, want 0", r.DuplicateIDs)
	}
}

func TestReportKeepsOnlyTheMostAffectedDevices(t *testing.T) {
	start := fixedNow.Add(-2 * time.Hour)
	var b strings.Builder
	b.WriteString("timestamp,device,temperature,vibration\n")
	for d := 0; d < 30; d++ {
		for i := 0; i < 3; i++ {
			fmt.Fprintf(&b, "%s,dev-%02d,%d,1\n", start.Add(time.Duration(i)*time.Minute).UTC().Format(time.RFC3339), d, 20+i)
		}
	}
	r := parse(t, "many.csv", b.String()).Summary.Report
	if r.DevicesTotal != 30 || len(r.Devices) != maxReportDevices {
		t.Fatalf("devices = %d total, %d listed", r.DevicesTotal, len(r.Devices))
	}
}

func TestReportOfALargeFileIsQuick(t *testing.T) {
	const rows = 50_000 // the limit is 200,000 (about 0.8 s without the race detector); 50,000 keeps `go test -race` quick
	start := fixedNow.Add(-72 * time.Hour)
	var b strings.Builder
	b.WriteString("timestamp,device,temperature,vibration\n")
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&b, "%s,dev-%02d,%d,%d\n", start.Add(time.Duration(i/20)*time.Second).UTC().Format(time.RFC3339), i%20, 20+i%37, 1+i%11)
	}
	t0 := time.Now()
	d := parse(t, "big.csv", b.String())
	took := time.Since(t0)
	if d.Summary.Rows != rows || d.Summary.Report == nil || d.Summary.Report.DevicesTotal != 20 {
		t.Fatalf("summary = rows %d, report %v", d.Summary.Rows, d.Summary.Report != nil)
	}
	t.Logf("import and data check of %d rows took %v", rows, took)
	if took > 30*time.Second {
		t.Fatalf("took %v", took)
	}
}
