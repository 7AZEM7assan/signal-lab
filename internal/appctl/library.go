package appctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"signallab/internal/lab"
)

// Two small files in the data folder: the replay settings the user saved by name (scenarios.json) and a short
// history of finished replays (run-history.json). Neither holds anything secret: header values are never saved,
// and an address is saved without its query string and any credentials in it.
const (
	scenariosFile   = "scenarios.json"
	historyFile     = "run-history.json"
	maxScenarios    = 50
	maxRuns         = 30
	maxScenarioName = 60
)

var libMu sync.Mutex

func readJSONFile(dir, name string, v any) error {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, v)
}

// writeJSONFile writes atomically: a temporary file, then a rename.
func writeJSONFile(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, strings.TrimSuffix(name, ".json")+"-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, name))
}

// ---- scenarios ----

// Scenario is a set of replay settings saved under a name.
type Scenario struct {
	Name   string     `json:"name"`
	Saved  time.Time  `json:"saved_at"`
	Config lab.Config `json:"config"`
}

type scenarioFile struct {
	Scenarios []Scenario `json:"scenarios"`
}

// LoadScenarios returns the saved scenarios, newest first. A missing or unreadable file gives none.
func LoadScenarios(dir string) []Scenario {
	libMu.Lock()
	defer libMu.Unlock()
	return loadScenarios(dir)
}

func loadScenarios(dir string) []Scenario {
	var f scenarioFile
	if readJSONFile(dir, scenariosFile, &f) != nil {
		return nil
	}
	sort.SliceStable(f.Scenarios, func(i, j int) bool { return f.Scenarios[i].Saved.After(f.Scenarios[j].Saved) })
	return f.Scenarios
}

// CleanScenario checks a scenario and removes what must not be kept: header values, the consent tick, and
// the query string, fragment and credentials of the address. maxBatch is the engine's batch limit.
func CleanScenario(s Scenario, maxBatch int) (Scenario, error) {
	s.Name = strings.TrimSpace(s.Name)
	if n := utf8.RuneCountInString(s.Name); n < 1 || n > maxScenarioName {
		return s, fmt.Errorf("the name must be 1-%d characters", maxScenarioName)
	}
	for _, r := range s.Name {
		if unicode.IsControl(r) {
			return s, errors.New("the name must not contain control characters")
		}
	}
	c := s.Config
	c.TargetHeaders, c.TargetConfirmed = nil, false
	if c.TargetURL != "" {
		u, err := lab.ParseTarget(strings.TrimSpace(c.TargetURL))
		if err != nil {
			return s, err
		}
		u.RawQuery, u.Fragment, u.User = "", "", nil
		c.TargetURL = u.String()
		if c.PayloadFormat == "" {
			c.PayloadFormat = lab.FormatBatch
		}
	}
	probe := c
	probe.TargetConfirmed = true // consent is asked for again at run time; it is not what is being checked here
	limit := maxBatch
	if c.External() {
		limit = lab.MaxExternalBatch
	}
	if err := probe.Validate(limit); err != nil {
		return s, err
	}
	s.Config = c
	return s, nil
}

// SaveScenario stores s (already cleaned), replacing one with the same name.
func SaveScenario(dir string, s Scenario) error {
	libMu.Lock()
	defer libMu.Unlock()
	all := loadScenarios(dir)
	out := []Scenario{s}
	for _, o := range all {
		if !strings.EqualFold(o.Name, s.Name) {
			out = append(out, o)
		}
	}
	if len(out) > maxScenarios {
		return fmt.Errorf("at most %d scenarios can be saved; delete one first", maxScenarios)
	}
	return writeJSONFile(dir, scenariosFile, scenarioFile{Scenarios: out})
}

// DeleteScenario removes the scenario with that name; it reports whether there was one.
func DeleteScenario(dir, name string) (bool, error) {
	libMu.Lock()
	defer libMu.Unlock()
	all := loadScenarios(dir)
	var out []Scenario
	for _, o := range all {
		if !strings.EqualFold(o.Name, strings.TrimSpace(name)) {
			out = append(out, o)
		}
	}
	if len(out) == len(all) {
		return false, nil
	}
	return true, writeJSONFile(dir, scenariosFile, scenarioFile{Scenarios: out})
}

// ---- run history ----

// RunRecord is what is kept of a finished replay: the settings (header values already hidden), the final
// numbers and what the per-second record picked out. The per-second points themselves are not kept.
type RunRecord struct {
	ID             string              `json:"id"`
	StartedAt      time.Time           `json:"started_at"`
	State          lab.State           `json:"state"`
	Source         string              `json:"source"`
	Target         string              `json:"target"`
	External       bool                `json:"external"`
	Config         *lab.Config         `json:"config,omitempty"`
	Planned        int                 `json:"planned"`
	RecordsDone    int                 `json:"records_done"`
	Accepted       int                 `json:"accepted"`
	Rejected       int                 `json:"rejected"`
	Throttled      int                 `json:"throttled"`
	Retries        int                 `json:"retries"`
	GaveUp         int                 `json:"gave_up_records"`
	RequestErrors  int                 `json:"request_errors"`
	ElapsedS       float64             `json:"elapsed_s"`
	ThroughputPerS float64             `json:"throughput_per_s"`
	Latency        map[string]any      `json:"latency,omitempty"`
	StatusCounts   map[string]int      `json:"status_counts,omitempty"`
	Summary        lab.TimelineSummary `json:"summary"`
	FirstError     *lab.ErrorSample    `json:"first_error,omitempty"`
}

type historyDoc struct {
	Runs []RunRecord `json:"runs"`
}

// NewRunRecord builds the record of a finished run.
func NewRunRecord(s lab.Snapshot, tl lab.Timeline) RunRecord {
	var started time.Time
	if s.StartedAt != nil {
		started = s.StartedAt.UTC()
	}
	return RunRecord{
		ID: started.Format("20060102T150405.000Z"), StartedAt: started, State: s.State, Source: s.Source, Target: s.Target,
		External: s.External, Config: s.Config, Planned: s.Planned, RecordsDone: s.RecordsDone, Accepted: s.Accepted,
		Rejected: s.Rejected, Throttled: s.Throttled, Retries: s.Retries, GaveUp: s.GaveUpRecords, RequestErrors: s.RequestErrors,
		ElapsedS: s.ElapsedS, ThroughputPerS: s.ThroughputPerS, Latency: s.Latency, StatusCounts: s.StatusCounts,
		Summary: tl.Summary, FirstError: s.FirstError,
	}
}

// LoadHistory returns the finished runs, newest first.
func LoadHistory(dir string) []RunRecord {
	libMu.Lock()
	defer libMu.Unlock()
	return loadHistory(dir)
}

func loadHistory(dir string) []RunRecord {
	var d historyDoc
	if readJSONFile(dir, historyFile, &d) != nil {
		return nil
	}
	return d.Runs
}

// AddRun puts a run at the top of the history and keeps the newest maxRuns.
func AddRun(dir string, r RunRecord) error {
	libMu.Lock()
	defer libMu.Unlock()
	runs := append([]RunRecord{r}, loadHistory(dir)...)
	if len(runs) > maxRuns {
		runs = runs[:maxRuns]
	}
	return writeJSONFile(dir, historyFile, historyDoc{Runs: runs})
}

// ClearHistory forgets every finished run.
func ClearHistory(dir string) error {
	libMu.Lock()
	defer libMu.Unlock()
	if err := os.Remove(filepath.Join(dir, historyFile)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
