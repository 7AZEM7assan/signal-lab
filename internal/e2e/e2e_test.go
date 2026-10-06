// Package e2e exercises the real pipeline, store, hub and HTTP API together against
// PostgreSQL (in an isolated schema). It needs SIGNALLAB_TEST_DATABASE_URL; see testdb.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"signallab/internal/alert"
	"signallab/internal/api"
	"signallab/internal/config"
	"signallab/internal/hub"
	"signallab/internal/metrics"
	"signallab/internal/pipeline"
	"signallab/internal/store"
	"signallab/internal/testdb"
)

type stack struct {
	ts   *httptest.Server
	pipe *pipeline.Pipeline
	hub  *hub.Hub
	st   *store.Store
}

func newStack(t *testing.T) *stack {
	t.Helper()
	pool := testdb.New(t)
	cfg, err := config.Load(func(k string) (string, bool) {
		if k == "SIGNALLAB_DATABASE_URL" {
			return "postgres://unused", true
		}
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := metrics.New()
	st := store.New(pool, 5*time.Second, m)
	h := hub.New(cfg.WSClientBuffer, cfg.WSMaxClients, m)
	pipe := pipeline.New(pipeline.Config{
		Capacity: 100, Workers: 2, BatchSize: 20, Attempts: 2, Backoff: 10 * time.Millisecond,
		Thresholds: alert.Thresholds{TemperatureC: cfg.TempAlertC, VibrationMMS: cfg.VibAlertMS},
	}, st, h, m, log)
	srv := api.New(api.Deps{Cfg: cfg, Ingest: pipe, DB: st, Hub: h, Metrics: m, Log: log,
		Now: func() time.Time { return time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC) }})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = pipe.Close(ctx)
		h.CloseAll()
	})
	return &stack{ts: ts, pipe: pipe, hub: h, st: st}
}

func rec(id string, seq int, temp, vib string) string {
	return `{"schema_version":1,"event_id":"` + id + `","device_id":"press-01","event_time":"2025-01-15T08:00:0` +
		string(rune('0'+seq)) + `Z","sequence":` + string(rune('0'+seq)) + `,"temperature_c":` + temp + `,"vibration_mm_s":` + vib + `}`
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIngestPersistsAlertsAndBroadcasts(t *testing.T) {
	s := newStack(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.ts.URL, "http")+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if _, _, err := ws.Read(ctx); err != nil { // hello: subscription is registered
		t.Fatal(err)
	}

	body := `{"events":[` + rec("e1", 1, "60", "2") + `,` + rec("e2", 2, "85", "2") + `,` + rec("e3", 3, "60", "7.1") + `]}`
	resp, err := http.Post(s.ts.URL+"/api/v1/events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	// 3 event frames + 2 alert frames, in no guaranteed order between workers.
	types := map[string]int{}
	for i := 0; i < 5; i++ {
		_, b, err := ws.Read(ctx)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		var m struct{ Type string }
		_ = json.Unmarshal(b, &m)
		types[m.Type]++
	}
	if types["event"] != 3 || types["alert"] != 2 {
		t.Fatalf("frames = %v, want 3 events and 2 alerts", types)
	}

	q := "?device_id=press-01&from=2025-01-15T08:00:00Z&to=2025-01-15T08:01:00Z"
	if evs := getJSON(t, s.ts.URL+"/api/v1/events"+q)["events"].([]any); len(evs) != 3 {
		t.Fatalf("events via HTTP: %d, want 3", len(evs))
	}
	alerts := getJSON(t, s.ts.URL+"/api/v1/alerts"+q)["alerts"].([]any)
	if len(alerts) != 2 {
		t.Fatalf("alerts via HTTP: %d, want 2", len(alerts))
	}
	if r := alerts[0].(map[string]any)["rule"]; r != "temperature_high" {
		t.Fatalf("first alert rule = %v", r)
	}
}

func TestReplayedBatchDoesNotDuplicateRows(t *testing.T) {
	s := newStack(t)
	body := `{"events":[` + rec("e1", 1, "99", "2") + `]}`
	for i := 0; i < 2; i++ {
		resp, err := http.Post(s.ts.URL+"/api/v1/events", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.pipe.Close(ctx); err != nil { // drain, so both sends are processed
		t.Fatal(err)
	}
	q := "?from=2025-01-15T08:00:00Z&to=2025-01-15T08:01:00Z"
	if n := len(getJSON(t, s.ts.URL+"/api/v1/events"+q)["events"].([]any)); n != 1 {
		t.Fatalf("events = %d, want 1", n)
	}
	if n := len(getJSON(t, s.ts.URL+"/api/v1/alerts"+q)["alerts"].([]any)); n != 1 {
		t.Fatalf("alerts = %d, want 1", n)
	}
}

func TestReadinessReflectsDatabase(t *testing.T) {
	s := newStack(t)
	if resp, err := http.Get(s.ts.URL + "/readyz"); err != nil || resp.StatusCode != 200 {
		t.Fatalf("readyz should be 200 with a live database: %v %v", resp, err)
	}
	s.st.Pool().Close() // simulate the database becoming unreachable
	resp, err := http.Get(s.ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz with a closed pool = %d, want 503", resp.StatusCode)
	}
}
