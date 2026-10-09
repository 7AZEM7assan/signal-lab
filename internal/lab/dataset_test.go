package lab

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"signallab/internal/event"
)

var limits = event.Limits{TempMinC: -50, TempMaxC: 250, VibMaxMMS: 100, MaxFutureSkew: 5 * time.Minute}

func parse(t *testing.T, name, content string) *Dataset {
	t.Helper()
	d, err := ParseDataset(name, []byte(content), limits, fixedNow)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return d
}

func TestCSVWithEverydayColumnNames(t *testing.T) {
	d := parse(t, "plant.csv", "Timestamp,Machine,Temp,Vibration,Operator\n"+
		"2025-01-15T08:00:00Z,press-01,61.5,2.1,ann\n"+
		"2025-01-15T08:00:02Z,press-01,62.0,2.2,ann\n"+
		"2025-01-15T08:00:00Z,pump-02,70.25,3.0,bob\n"+
		"2025-01-15T08:00:04Z,pump-02,999,3.1,bob\n") // out of range
	s := d.Summary
	if s.Format != "csv" || s.Rows != 4 || s.Devices != 2 {
		t.Fatalf("summary: %+v", s)
	}
	if s.Mapped["Timestamp"] != "event_time" || s.Mapped["Machine"] != "device_id" || s.Mapped["Temp"] != "temperature_c" || s.Mapped["Vibration"] != "vibration_mm_s" {
		t.Fatalf("mapped: %v", s.Mapped)
	}
	if len(s.Ignored) != 1 || s.Ignored[0] != "Operator" {
		t.Fatalf("ignored: %v", s.Ignored)
	}
	if s.FirstTime != "2025-01-15T08:00:00.000Z" || s.LastTime != "2025-01-15T08:00:04.000Z" {
		t.Fatalf("times: %s .. %s", s.FirstTime, s.LastTime)
	}
	if s.Problems[event.ReasonOutOfRange] != 1 || len(s.Problems) != 1 {
		t.Fatalf("problems should be exactly one out-of-range row: %v", s.Problems)
	}
	raw, _ := json.Marshal(d.Records[0])
	want := `{"device_id":"press-01","event_id":"press-01-1","event_time":"2025-01-15T08:00:00Z","schema_version":1,"temperature_c":61.5,"vibration_mm_s":2.1}`
	if string(raw) != want {
		t.Fatalf("record:\n got %s\nwant %s", raw, want)
	}
	if len(s.Derived) != 2 {
		t.Fatalf("event_id and schema_version are filled in, and the summary should say so: %v", s.Derived)
	}
}

func TestExactColumnNamesBeatAliasesAndRoundTripTheAppsOwnExport(t *testing.T) {
	// The app's CSV export has a received_at column, which is not part of what a client may send.
	d := parse(t, "export.csv", "event_id,device_id,site_id,event_time,received_at,sequence,temperature_c,vibration_mm_s\n"+
		"press-01-5,press-01,plant-a,2025-01-15T08:00:00.000Z,2025-01-15T08:00:01.000Z,5,60.5,2.5\n")
	if len(d.Summary.Problems) != 0 || len(d.Summary.Ignored) != 1 || d.Summary.Ignored[0] != "received_at" {
		t.Fatalf("an export from the app should import cleanly: %+v", d.Summary)
	}
	if d.Records[0]["event_id"] != "press-01-5" || d.Records[0]["site_id"] != "plant-a" || len(d.Summary.Derived) != 1 {
		t.Fatalf("record: %v, derived %v", d.Records[0], d.Summary.Derived)
	}
	// "temp" is an alias, but a real temperature_c column wins.
	d = parse(t, "both.csv", "event_time,device_id,temp,temperature_c,vibration_mm_s\n2025-01-15T08:00:00Z,a-1,1,2,3\n")
	if v, _ := json.Marshal(d.Records[0]["temperature_c"]); string(v) != "2" {
		t.Fatalf("temperature_c should come from the exact column: %s", v)
	}
	if len(d.Summary.Ignored) != 1 || d.Summary.Ignored[0] != "temp" {
		t.Fatalf("ignored: %v", d.Summary.Ignored)
	}
}

func TestDirtyCellsAreKeptSoTheServiceUnderTestSeesThem(t *testing.T) {
	d := parse(t, "dirty.csv", "event_time,device_id,temperature_c,vibration_mm_s\n"+
		"2025-01-15T08:00:00Z,press-01,,2.0\n"+ // empty: missing value
		"2025-01-15T08:00:01Z,press-01,hot,2.0\n"+ // text where a number belongs
		"yesterday,press-01,60,2.0\n") // bad timestamp
	if _, present := d.Records[0]["temperature_c"]; present {
		t.Fatal("an empty cell should be a missing value")
	}
	if d.Records[1]["temperature_c"] != "hot" {
		t.Fatalf("text in a number column should stay text: %v", d.Records[1]["temperature_c"])
	}
	p := d.Summary.Problems
	if p[event.ReasonMissingField] != 1 || p[event.ReasonMalformedJSON] != 1 || p[event.ReasonInvalidTimestamp] != 1 {
		t.Fatalf("problems: %v", p)
	}
	// The planner copes with a missing or unparseable time (it used to assume a string).
	cfg := small()
	cfg.LateRate, cfg.DuplicateRate = 1, 1
	if plan, _ := PlanRecords(d.Records, cfg); len(plan) != 6 {
		t.Fatalf("expected 3 records plus 3 duplicates, got %d", len(plan))
	}
}

func TestNDJSONKeepsLargeNumbersExactly(t *testing.T) {
	d := parse(t, "events.ndjson", `{"event_id":"a-1","device_id":"a","event_time":"2025-01-15T08:00:00Z","sequence":9007199254740993,"temperature_c":50,"vibration_mm_s":1,"extra":"x"}`+"\n\n"+
		`{"event_id":"a-2","device_id":"a","event_time":"2025-01-15T08:00:01Z","sequence":9007199254740994,"temperature_c":51,"vibration_mm_s":1}`+"\n")
	if d.Summary.Format != "ndjson" || d.Summary.Rows != 2 || len(d.Summary.Ignored) != 1 || d.Summary.Ignored[0] != "extra" {
		t.Fatalf("summary: %+v", d.Summary)
	}
	raw, _ := json.Marshal(d.Records[0])
	if !strings.Contains(string(raw), `"sequence":9007199254740993`) {
		t.Fatalf("a 64-bit sequence must survive unchanged: %s", raw)
	}
	if len(d.Summary.Problems) != 0 {
		t.Fatalf("problems: %v", d.Summary.Problems)
	}
}

func TestJSONArrayAndBOM(t *testing.T) {
	d := parse(t, "a.json", `[{"time":"2025-01-15T08:00:00Z","device":"a-1","temperature":50,"vibration":1}]`)
	if d.Summary.Format != "json" || d.Summary.Rows != 1 || d.Summary.Mapped["time"] != "event_time" {
		t.Fatalf("summary: %+v", d.Summary)
	}
	d = parse(t, "bom.csv", "\ufeffevent_time,device_id,temperature_c,vibration_mm_s\n2025-01-15T08:00:00Z,a-1,50,1\n")
	if d.Summary.Rows != 1 || len(d.Summary.Problems) != 0 {
		t.Fatalf("a file with a byte order mark should import: %+v", d.Summary)
	}
}

func TestImportErrorsAreSpecific(t *testing.T) {
	for name, tc := range map[string]struct{ content, want string }{
		"empty":           {"", "empty"},
		"spaces":          {"  \n\n", "empty"},
		"header only":     {"event_time,device_id,temperature_c,vibration_mm_s\n", "no rows"},
		"missing columns": {"event_time,device_id\n2025-01-15T08:00:00Z,a\n", "temperature_c, vibration_mm_s"},
		"bad ndjson line": {`{"event_time":"x","device_id":"a","temperature_c":1,"vibration_mm_s":1}` + "\nnot json\n", "line 2"},
		"bad json":        {`[{"a":1},`, "item"},
		"csv quoting":     {"event_time,device_id,temperature_c,vibration_mm_s\n\"unterminated,a,1,2\n", "line"},
	} {
		_, err := ParseDataset(name, []byte(tc.content), limits, fixedNow)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %v should mention %q", name, err, tc.want)
		}
	}
}

func TestRebasedMovesTheLatestReadingToNow(t *testing.T) {
	d := parse(t, "old.csv", "event_time,device_id,temperature_c,vibration_mm_s\n"+
		"2020-01-01T00:00:00Z,a-1,50,1\n2020-01-01T00:00:10Z,a-1,50,1\nnot-a-time,a-1,50,1\n")
	out := d.Rebased(fixedNow)
	if out[1]["event_time"] != FormatTime(fixedNow) || out[0]["event_time"] != FormatTime(fixedNow.Add(-10*time.Second)) {
		t.Fatalf("rebased times: %v, %v", out[0]["event_time"], out[1]["event_time"])
	}
	if out[2]["event_time"] != "not-a-time" {
		t.Fatal("an unparseable time should be left alone")
	}
	if d.Records[0]["event_time"] != "2020-01-01T00:00:00Z" {
		t.Fatal("rebasing must not change the imported data")
	}
	// Nothing to shift: the same records come back.
	none := parse(t, "none.csv", "event_time,device_id,temperature_c,vibration_mm_s\nx,a-1,50,1\n")
	if got := none.Rebased(fixedNow); got[0]["event_time"] != "x" {
		t.Fatalf("no parseable times: %v", got[0])
	}
}

func TestRunnerReplaysTheImportedFileWithFaultsAndRebasing(t *testing.T) {
	var got received
	srv := httptest.NewServer(got.handler(http.StatusOK, ""))
	defer srv.Close()
	d := parse(t, "plant.csv", "timestamp,device,temp,vibration\n"+
		"2020-01-01T00:00:00Z,a-1,50,1\n2020-01-01T00:00:01Z,a-1,51,1\n2020-01-01T00:00:02Z,a-1,52,1\n2020-01-01T00:00:03Z,a-1,53,1\n")

	run := NewRunner()
	c := small()
	c.UseDataset, c.RebaseTime, c.DuplicateRate = true, true, 1
	c.TargetURL = srv.URL
	c.BatchSize = 100
	if _, err := run.Start(c, Target{}, 500); err == nil || !strings.Contains(err.Error(), "no file has been imported") {
		t.Fatalf("replaying without a file should say so: %v", err)
	}
	run.SetDataset(d)
	if run.DatasetSummary() == nil || run.DatasetSummary().Rows != 4 {
		t.Fatal("the imported file should be described")
	}
	snap, err := run.Start(c, Target{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Planned != 8 || snap.Source != "file plant.csv" || snap.Injected.Duplicates != 4 {
		t.Fatalf("4 rows plus 4 injected duplicates: %+v", snap)
	}
	snap = waitDone(t, run)
	if snap.Accepted != 8 {
		t.Fatalf("accepted: %+v", snap)
	}
	var env struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(got.bodies[0]), &env); err != nil || len(env.Events) != 8 {
		t.Fatalf("body: %v", err)
	}
	lastTime, _ := time.Parse(time.RFC3339, env.Events[7]["event_time"].(string))
	if d := time.Since(lastTime); d < 0 || d > time.Minute {
		t.Fatalf("with rebasing the newest reading should be about now, got %v ago", d)
	}
	if env.Events[0]["event_id"] != "a-1-1" || env.Events[0]["device_id"] != "a-1" {
		t.Fatalf("first record: %v", env.Events[0])
	}
}

func TestTooManyRowsIsRefusedWithAHint(t *testing.T) {
	var b strings.Builder
	b.WriteString("event_time,device_id,temperature_c,vibration_mm_s\n")
	for i := 0; i <= MaxDatasetRows; i++ {
		b.WriteString("2025-01-15T08:00:00Z,a-1,50,1\n")
	}
	_, err := ParseDataset("big.csv", []byte(b.String()), limits, fixedNow)
	if err == nil || !strings.Contains(err.Error(), "split it") {
		t.Fatalf("a file over the row limit should be refused with advice: %v", err)
	}
}

func TestColumnsInAnotherLanguageNeedMappingAndThenWork(t *testing.T) {
	csv := "Zeit,Maschine,Grad,Schwingung,Notiz\n2025-01-15T08:00:00Z,press-01,61.5,2.1,x\n2025-01-15T08:00:02Z,press-01,62,2.2,y\n"
	_, err := ParseDataset("werk.csv", []byte(csv), limits, fixedNow)
	var need *NeedsMapping
	if !errors.As(err, &need) || len(need.Missing) != 4 || len(need.Columns) != 5 {
		t.Fatalf("expected a NeedsMapping listing 4 missing fields and 5 columns, got %v", err)
	}
	d, err := ParseDatasetMapped("werk.csv", []byte(csv), limits, fixedNow, map[string]string{
		"Zeit": "event_time", "Maschine": "device_id", "Grad": "temperature_c", "Schwingung": "vibration_mm_s", "Notiz": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(d.Records[0])
	want := `{"device_id":"press-01","event_id":"press-01-1","event_time":"2025-01-15T08:00:00Z","schema_version":1,"temperature_c":61.5,"vibration_mm_s":2.1}`
	if string(raw) != want {
		t.Fatalf("numbers must be typed by the chosen field:\n got %s\nwant %s", raw, want)
	}
	s := d.Summary
	if s.Fields["temperature_c"] != "Grad" || s.Mapped["Zeit"] != "event_time" || len(s.Ignored) != 1 || s.Ignored[0] != "Notiz" || len(s.Problems) != 0 {
		t.Fatalf("summary: %+v", s)
	}
}

func TestExplicitChoiceBeatsAnAliasAndIsCheckedForMistakes(t *testing.T) {
	// Both "timestamp" (an alias) and "Zeit" exist; the user says Zeit is the time.
	csv := "timestamp,Zeit,device,temp,vibration\n1,2025-01-15T08:00:00Z,a,1,1\n"
	d, err := ParseDatasetMapped("x.csv", []byte(csv), limits, fixedNow, map[string]string{"Zeit": "event_time"})
	if err != nil || d.Summary.Fields["event_time"] != "Zeit" {
		t.Fatalf("the explicit choice must win over the alias: %v %+v", err, d)
	}
	for name, ov := range map[string]map[string]string{
		"unknown column":   {"Nope": "event_time"},
		"unknown field":    {"Zeit": "colour"},
		"same field twice": {"Zeit": "event_time", "device": "event_time"},
	} {
		if _, err := ParseDatasetMapped("x.csv", []byte(csv), limits, fixedNow, ov); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	// JSON files can be mapped the same way.
	js := `[{"t":"2025-01-15T08:00:00Z","m":"a","g":1.5,"v":2}]`
	if _, err := ParseDataset("x.json", []byte(js), limits, fixedNow); err == nil {
		t.Fatal("unrecognised JSON keys need mapping too")
	}
	if d, err := ParseDatasetMapped("x.json", []byte(js), limits, fixedNow, map[string]string{"t": "event_time", "m": "device_id", "g": "temperature_c", "v": "vibration_mm_s"}); err != nil || d.Summary.Rows != 1 {
		t.Fatalf("json with a choice: %v", err)
	}
}
