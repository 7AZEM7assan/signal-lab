package appctl

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"

	"signallab/internal/lab"
)

// handleReplayReport serves the report of the current or latest replay as a download.
// ?format=md (the default) or json.
func (s *Server) handleReplayReport(w http.ResponseWriter, r *http.Request) {
	snap := s.Engine.Runner.Snapshot()
	if snap.State == "idle" || snap.StartedAt == nil {
		writeErr(w, http.StatusNotFound, "no_run", "no replay has run yet")
		return
	}
	info := ReportInfo{Version: s.Version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Snap: snap, Timeline: s.Engine.Runner.Timeline()}
	if ds := s.Engine.Runner.DatasetSummary(); ds != nil && snap.Source == "file "+ds.Name {
		info.Dataset = ds
	}
	stamp := snap.StartedAt.UTC().Format("20060102-150405")
	switch f := r.URL.Query().Get("format"); f {
	case "", "md":
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="signal-lab-report-`+stamp+`.md"`)
		_, _ = io.WriteString(w, info.Markdown())
	case "json":
		w.Header().Set("Content-Disposition", `attachment; filename="signal-lab-report-`+stamp+`.json"`)
		writeJSON(w, http.StatusOK, map[string]any{
			"signal_lab": info.Version, "platform": info.Platform, "run": info.Snap, "timeline": info.Timeline, "data_check": datasetReport(info),
		})
	default:
		writeErr(w, http.StatusBadRequest, "bad_format", "format must be md or json")
	}
}

func datasetReport(ri ReportInfo) any {
	if ri.Dataset == nil {
		return nil
	}
	return ri.Dataset.Report
}

func (s *Server) handleHistoryGet(w http.ResponseWriter, _ *http.Request) {
	runs := LoadHistory(s.DataDir)
	if runs == nil {
		runs = []RunRecord{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

func (s *Server) handleHistoryDelete(w http.ResponseWriter, _ *http.Request) {
	if err := ClearHistory(s.DataDir); err != nil {
		writeErr(w, http.StatusInternalServerError, "history_failed", "could not clear the history: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": []RunRecord{}})
}

func (s *Server) handleScenariosGet(w http.ResponseWriter, _ *http.Request) {
	list := LoadScenarios(s.DataDir)
	if list == nil {
		list = []Scenario{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"scenarios": list})
}

// handleScenarioPut saves replay settings under a name: {"name": "...", "config": {...}}.
func (s *Server) handleScenarioPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	in := Scenario{Config: lab.Defaults()} // settings left out take the same defaults as when a replay starts
	if err := dec.Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_scenario", "the scenario is not valid JSON: "+err.Error())
		return
	}
	clean, err := CleanScenario(in, s.Engine.MaxBatch())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_scenario", err.Error())
		return
	}
	clean.Saved = time.Now().UTC()
	if err := SaveScenario(s.DataDir, clean); err != nil {
		writeErr(w, http.StatusBadRequest, "scenario_not_saved", err.Error())
		return
	}
	s.Log.Info("scenario saved")
	writeJSON(w, http.StatusOK, map[string]any{"scenario": clean})
}

func (s *Server) handleScenarioDelete(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		writeErr(w, http.StatusBadRequest, "invalid_scenario", "say which scenario with ?name=")
		return
	}
	found, err := DeleteScenario(s.DataDir, name)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "scenario_not_deleted", err.Error())
		return
	}
	if !found {
		writeErr(w, http.StatusNotFound, "no_scenario", fmt.Sprintf("there is no scenario called %q", name))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": name})
}
