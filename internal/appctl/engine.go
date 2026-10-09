package appctl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"signallab/internal/api"
	"signallab/internal/config"
	"signallab/internal/event"
	"signallab/internal/hub"
	"signallab/internal/lab"
	"signallab/internal/metrics"
	"signallab/internal/pipeline"
	"signallab/internal/sqlitestore"
)

// DBFileName is the database file inside the data directory.
const DBFileName = "signallab.db"

// instance is one generation of the ingest engine. Changing structural settings builds a new one.
type instance struct {
	cfg     config.Config
	store   *sqlitestore.Store
	hub     *hub.Hub
	pipe    *pipeline.Pipeline
	srv     *api.Server
	handler http.Handler
}

// Engine owns the database, the ingest pipeline and the replay runner.
type Engine struct {
	dir string
	log *slog.Logger

	mu       sync.RWMutex // guards cur, settings, restarts
	cur      *instance
	settings Settings
	restarts int

	Runner *lab.Runner
}

// NewEngine opens the database in dir and starts the pipeline with the saved settings.
func NewEngine(dir string, log *slog.Logger) (*Engine, error) {
	s, err := LoadSettings(dir)
	if err != nil {
		log.Warn("using default settings", "reason", err)
	}
	e := &Engine{dir: dir, log: log, settings: s, Runner: lab.NewRunner()}
	inst, err := e.build(s)
	if err != nil {
		return nil, err
	}
	e.cur = inst
	return e, nil
}

func (e *Engine) build(s Settings) (*instance, error) {
	cfg, err := s.Config()
	if err != nil {
		return nil, err
	}
	m := metrics.New()
	st, err := sqlitestore.Open(filepath.Join(e.dir, DBFileName), cfg.DBTimeout, m)
	if err != nil {
		return nil, err
	}
	h := hub.New(cfg.WSClientBuffer, cfg.WSMaxClients, m)
	pipe := pipeline.New(pipeline.Config{
		Capacity: cfg.QueueCapacity, Workers: cfg.Workers, BatchSize: cfg.WorkerBatch,
		Attempts: cfg.PersistRetries, Backoff: cfg.PersistBackoff, LabDelay: cfg.LabWorkerDelay,
		Thresholds: s.Thresholds(),
	}, st, h, m, e.log)
	srv := api.New(api.Deps{Cfg: cfg, Ingest: pipe, DB: st, Hub: h, Metrics: m, Log: e.log})
	return &instance{cfg: cfg, store: st, hub: h, pipe: pipe, srv: srv, handler: srv.Handler()}, nil
}

func (e *Engine) current() *instance {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cur
}

// Handler is the ingest/query/WebSocket/metrics handler of the current instance. It is
// looked up per request so a rebuilt engine takes over without restarting the HTTP server.
func (e *Engine) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { e.current().handler.ServeHTTP(w, r) })
}

// Store returns the current database.
func (e *Engine) Store() *sqlitestore.Store { return e.current().store }

// Settings returns the settings in effect.
func (e *Engine) Settings() Settings {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.settings
}

// Info is a snapshot for the panel's status line.
type Info struct {
	Settings      Settings `json:"settings"`
	QueueDepth    int      `json:"queue_depth"`
	QueueCapacity int      `json:"queue_capacity"`
	WSClients     int      `json:"ws_clients"`
	EngineStarts  int      `json:"engine_restarts"`
	MaxBatch      int      `json:"max_batch_events"`
}

func (e *Engine) Info() Info {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return Info{Settings: e.settings, QueueDepth: e.cur.pipe.Depth(), QueueCapacity: e.cur.pipe.Capacity(),
		WSClients: e.cur.hub.Len(), EngineStarts: e.restarts, MaxBatch: e.cur.cfg.MaxBatchEvents}
}

// Limits are the plausibility bounds the built-in service applies to readings. They are used to
// preview how an imported file would fare against the Signal Lab schema.
func (e *Engine) Limits() event.Limits {
	c := e.current().cfg
	return event.Limits{TempMinC: c.TempMinC, TempMaxC: c.TempMaxC, VibMaxMMS: c.VibMaxMMS, MaxFutureSkew: c.MaxFutureSkew}
}

// MaxBatch is the service's per-request event limit.
func (e *Engine) MaxBatch() int { return e.current().cfg.MaxBatchEvents }

// ErrReplayRunning is returned when an operation needs the engine quiet.
var ErrReplayRunning = errors.New("stop the running replay first")

// Apply validates and saves new settings. Thresholds take effect immediately; changes to the
// queue or workers rebuild the engine (draining the queue first), which briefly disconnects
// live WebSocket clients. The returned bool says whether the engine was rebuilt.
func (e *Engine) Apply(ns Settings) (bool, error) {
	if _, err := ns.Config(); err != nil {
		return false, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	structural := ns.structural(e.settings)
	if structural && e.Runner.Snapshot().State == lab.StateRunning {
		return false, ErrReplayRunning
	}
	if err := SaveSettings(e.dir, ns); err != nil {
		return false, fmt.Errorf("save settings: %w", err)
	}
	if !structural {
		e.cur.pipe.SetThresholds(ns.Thresholds())
		e.settings = ns
		return false, nil
	}
	if err := e.swapLocked(ns, nil); err != nil {
		return false, err
	}
	return true, nil
}

// Quiesce drains the queue, runs fn with the engine stopped, and starts a fresh engine. It is
// used for operations (clearing data) that must not race with queued events.
func (e *Engine) Quiesce(fn func(*sqlitestore.Store) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.Runner.Snapshot().State == lab.StateRunning {
		return ErrReplayRunning
	}
	return e.swapLocked(e.settings, fn)
}

// swapLocked stops the current instance (draining accepted events to disk), optionally runs
// fn against its closed-pipeline store, and then builds and installs a new instance.
func (e *Engine) swapLocked(ns Settings, fn func(*sqlitestore.Store) error) error {
	old := e.cur
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	old.srv.SetDraining()
	if err := old.pipe.Close(ctx); err != nil {
		e.log.Error("queue drain hit the deadline while rebuilding the engine; remaining accepted events were discarded", "error", err)
	}
	old.hub.CloseAll()
	var fnErr error
	if fn != nil {
		fnErr = fn(old.store)
	}
	_ = old.store.Close()

	inst, err := e.build(ns)
	if err != nil {
		// Fall back to the previous settings so the app keeps working.
		if inst2, err2 := e.build(e.settings); err2 == nil {
			e.cur = inst2
		}
		return errors.Join(fnErr, err)
	}
	e.cur, e.settings = inst, ns
	e.restarts++
	return fnErr
}

// Close drains the queue and releases everything. The engine must not be used afterwards.
func (e *Engine) Close(ctx context.Context) {
	e.Runner.Stop()
	e.mu.Lock()
	defer e.mu.Unlock()
	old := e.cur
	old.srv.SetDraining()
	if err := old.pipe.Close(ctx); err != nil {
		e.log.Error("queue drain hit the shutdown deadline; remaining accepted events were discarded", "error", err)
	} else {
		e.log.Info("queue drained")
	}
	old.hub.CloseAll()
	_ = old.store.Close()
}
