package appctl

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

type scenarioList struct {
	Scenarios []struct {
		Name   string         `json:"name"`
		Config map[string]any `json:"config"`
	} `json:"scenarios"`
}

func TestScenariosKeepSettingsButNeverSecrets(t *testing.T) {
	a := newApp(t, "")
	var out scenarioList
	if c := a.json("GET", "/app/api/scenarios", nil, &out); c != 200 || len(out.Scenarios) != 0 {
		t.Fatalf("empty: %d %+v", c, out)
	}
	cfg := map[string]any{"seed": 3, "devices": 5, "duration_s": 60, "interval_s": 1, "rate_per_s": 50, "batch_size": 10, "concurrency": 2,
		"retries": 3, "timeout_s": 10, "ramp_to_per_s": 200, "ramp_s": 5,
		"target_url": "http://127.0.0.1:9/hooks/ingest?key=SEKRETQUERY", "target_headers": map[string]string{"X-Api-Key": "SEKRETHEADER"}, "target_confirmed": true}
	if c, raw := a.do("PUT", "/app/api/scenarios", map[string]any{"name": "  My service  ", "config": cfg}); c != 200 {
		t.Fatalf("save: %d %s", c, raw)
	}
	raw, err := os.ReadFile(filepath.Join(a.dir, "scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SEKRETQUERY", "SEKRETHEADER"} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s was written to disk:\n%s", secret, raw)
		}
	}
	a.json("GET", "/app/api/scenarios", nil, &out)
	if len(out.Scenarios) != 1 || out.Scenarios[0].Name != "My service" {
		t.Fatalf("list: %+v", out)
	}
	c := out.Scenarios[0].Config
	if c["target_url"] != "http://127.0.0.1:9/hooks/ingest" || c["target_confirmed"] != false || c["ramp_to_per_s"] != float64(200) || c["payload_format"] != "batch" {
		t.Fatalf("saved config: %v", c)
	}
	// The same name (in any case) replaces it.
	cfg["seed"] = 4
	a.json("PUT", "/app/api/scenarios", map[string]any{"name": "my SERVICE", "config": cfg}, nil)
	a.json("GET", "/app/api/scenarios", nil, &out)
	if len(out.Scenarios) != 1 || out.Scenarios[0].Config["seed"] != float64(4) {
		t.Fatalf("replace: %+v", out)
	}
	if c, _ := a.do("DELETE", "/app/api/scenarios?name=MY%20service", nil); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	if c, _ := a.do("DELETE", "/app/api/scenarios?name=nothing", nil); c != 404 {
		t.Fatalf("delete missing: %d", c)
	}
	a.json("GET", "/app/api/scenarios", nil, &out)
	if len(out.Scenarios) != 0 {
		t.Fatalf("after delete: %+v", out)
	}
}

func TestScenariosAreChecked(t *testing.T) {
	a := newApp(t, "")
	good := map[string]any{"seed": 1, "devices": 2, "duration_s": 10, "interval_s": 1, "batch_size": 5, "concurrency": 1, "timeout_s": 5}
	for name, body := range map[string]map[string]any{
		"empty name":        {"name": " ", "config": good},
		"long name":         {"name": strings.Repeat("x", 61), "config": good},
		"control character": {"name": "a\nb", "config": good},
		"bad settings":      {"name": "x", "config": map[string]any{"devices": 0}},
		"unknown field":     {"name": "x", "config": good, "extra": 1},
		"bad ramp":          {"name": "x", "config": map[string]any{"seed": 1, "devices": 2, "duration_s": 10, "interval_s": 1, "batch_size": 5, "concurrency": 1, "timeout_s": 5, "rate_per_s": 0, "ramp_to_per_s": 50, "ramp_s": 5}},
		"remote too fast":   {"name": "x", "config": map[string]any{"seed": 1, "devices": 2, "duration_s": 10, "interval_s": 1, "batch_size": 5, "concurrency": 1, "timeout_s": 5, "rate_per_s": 5000, "target_url": "http://203.0.113.9/ingest"}},
	} {
		if c, raw := a.do("PUT", "/app/api/scenarios", body); c != 400 {
			t.Errorf("%s: %d %s, want 400", name, c, raw)
		}
	}
	// A remote address saves without its consent: it is asked for again when the scenario is used.
	remote := map[string]any{"seed": 1, "devices": 2, "duration_s": 10, "interval_s": 1, "batch_size": 5, "concurrency": 1, "timeout_s": 5, "rate_per_s": 100, "target_url": "https://example.com/ingest"}
	if c, raw := a.do("PUT", "/app/api/scenarios", map[string]any{"name": "remote", "config": remote}); c != 200 {
		t.Errorf("remote scenario: %d %s", c, raw)
	}
	for i := 0; i < maxScenarios; i++ {
		a.do("PUT", "/app/api/scenarios", map[string]any{"name": "s" + string(rune('A'+i%26)) + string(rune('a'+i/26)), "config": good})
	}
	if c, _ := a.do("PUT", "/app/api/scenarios", map[string]any{"name": "one too many", "config": good}); c != 400 {
		t.Errorf("more than %d scenarios: %d, want 400", maxScenarios, c)
	}
}

func TestRunHistoryAndReportOfAFinishedReplay(t *testing.T) {
	a := newApp(t, "")
	if c, _ := a.do("GET", "/app/api/replay/report", nil); c != 404 {
		t.Fatalf("report before any run: %d, want 404", c)
	}
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer own.Close()
	body := replayBody(map[string]any{"devices": 5, "duration_s": 20, "interval_s": 1, "batch_size": 10, "malformed_rate": 0.1,
		"target_url": own.URL + "/hooks/SEKRETPATH?key=SEKRETQUERY", "target_headers": map[string]string{"X-Api-Key": "SEKRETHEADER"}})
	if c := a.json("POST", "/app/api/replay/start", body, nil); c != 202 {
		t.Fatalf("start: %d", c)
	}
	if done := a.waitReplay(); done["state"] != "done" {
		t.Fatalf("run: %v", done)
	}
	var hist struct {
		Runs []struct {
			ID       string  `json:"id"`
			Accepted int     `json:"accepted"`
			Planned  int     `json:"planned"`
			Target   string  `json:"target"`
			External bool    `json:"external"`
			Elapsed  float64 `json:"elapsed_s"`
		} `json:"runs"`
	}
	eventually(t, "the run to reach the history", func() bool { a.json("GET", "/app/api/history", nil, &hist); return len(hist.Runs) == 1 })
	h := hist.Runs[0]
	if h.Planned != 100 || h.Accepted != 100 || !h.External || h.ID == "" || strings.Contains(h.Target, "SEKRET") {
		t.Fatalf("history: %+v", h)
	}
	disk, _ := os.ReadFile(filepath.Join(a.dir, "run-history.json"))
	for _, secret := range []string{"SEKRETPATH", "SEKRETQUERY", "SEKRETHEADER"} {
		if strings.Contains(string(disk), secret) {
			t.Fatalf("%s was written to the history file", secret)
		}
	}

	code, md := a.do("GET", "/app/api/replay/report", nil)
	text := string(md)
	if code != 200 {
		t.Fatalf("report: %d %s", code, md)
	}
	for _, want := range []string{"# Signal Lab replay report", "## Setup", "## Results", "## Run over time", "Records sent | 100 of 100 planned", "Signal Lab test on "} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %q:\n%s", want, text)
		}
	}
	code, js := a.do("GET", "/app/api/replay/report?format=json", nil)
	if code != 200 || !strings.Contains(string(js), `"timeline"`) {
		t.Fatalf("json report: %d %s", code, js)
	}
	for _, secret := range []string{"SEKRETPATH", "SEKRETQUERY", "SEKRETHEADER"} {
		if strings.Contains(text, secret) || strings.Contains(string(js), secret) {
			t.Fatalf("%s leaked into a report", secret)
		}
	}
	if c, _ := a.do("GET", "/app/api/replay/report?format=pdf", nil); c != 400 {
		t.Fatalf("unknown format: %d", c)
	}

	if c, _ := a.do("DELETE", "/app/api/history", nil); c != 200 {
		t.Fatalf("clear: %d", c)
	}
	a.json("GET", "/app/api/history", nil, &hist)
	if len(hist.Runs) != 0 {
		t.Fatalf("history after clearing: %+v", hist)
	}
}

func TestAScenarioMayLeaveSettingsOutAndGetsTheDefaults(t *testing.T) {
	a := newApp(t, "")
	if c, raw := a.do("PUT", "/app/api/scenarios", map[string]any{"name": "small", "config": map[string]any{"devices": 3, "rate_per_s": 20}}); c != 200 {
		t.Fatalf("save: %d %s", c, raw)
	}
	var out scenarioList
	a.json("GET", "/app/api/scenarios", nil, &out)
	c := out.Scenarios[0].Config
	if c["devices"] != float64(3) || c["rate_per_s"] != float64(20) || c["timeout_s"] != float64(10) || c["batch_size"] != float64(20) {
		t.Fatalf("config = %v", c)
	}
}
