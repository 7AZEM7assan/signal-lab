// Command signallab runs the telemetry service, applies migrations, or probes health.
//
//	signallab [serve]      run the service (default)
//	signallab migrate      apply database migrations and exit
//	signallab healthcheck  GET /readyz on localhost; exit 0 if ready (used by Docker)
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"signallab/internal/alert"
	"signallab/internal/api"
	"signallab/internal/config"
	"signallab/internal/hub"
	"signallab/internal/metrics"
	"signallab/internal/pipeline"
	"signallab/internal/store"
)

func main() {
	cmd := "serve"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	cfg, err := config.OS()
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid configuration:\n%v\n", err)
		os.Exit(2)
	}
	log := newLogger(cfg)

	switch cmd {
	case "serve":
		err = serve(cfg, log)
	case "migrate":
		err = migrate(cfg, log)
	case "healthcheck":
		err = healthcheck(cfg)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q (use serve, migrate or healthcheck)\n", cmd)
		os.Exit(2)
	}
	if err != nil {
		log.Error("exiting with error", "command", cmd, "error", err)
		os.Exit(1)
	}
}

func newLogger(cfg config.Config) *slog.Logger {
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel)) // validated by config
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "text" {
		return slog.New(slog.NewTextHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, opts))
}

func serve(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	m := metrics.New()
	pool, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer pool.Close()
	st := store.New(pool, cfg.DBTimeout, m)

	if cfg.AutoMigrate {
		if err := runMigrations(ctx, st, log); err != nil {
			return err
		}
	}

	h := hub.New(cfg.WSClientBuffer, cfg.WSMaxClients, m)
	pipe := pipeline.New(pipeline.Config{
		Capacity: cfg.QueueCapacity, Workers: cfg.Workers, BatchSize: cfg.WorkerBatch,
		Attempts: cfg.PersistRetries, Backoff: cfg.PersistBackoff, LabDelay: cfg.LabWorkerDelay,
		Thresholds: alert.Thresholds{TemperatureC: cfg.TempAlertC, VibrationMMS: cfg.VibAlertMS},
	}, st, h, m, log)
	srv := api.New(api.Deps{Cfg: cfg, Ingest: pipe, DB: st, Hub: h, Metrics: m, Log: log})

	httpServer := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second, // hijacked WebSocket connections are exempt
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 2)
	go func() { errc <- httpServer.ListenAndServe() }()

	var pprofServer *http.Server
	if cfg.PprofAddr != "" {
		pprofServer = startPprof(cfg.PprofAddr, log, errc)
	}

	log.Info("signallab started", "addr", cfg.HTTPAddr, "queue_capacity", cfg.QueueCapacity, "workers", cfg.Workers,
		"temp_alert_c", cfg.TempAlertC, "vib_alert_mm_s", cfg.VibAlertMS)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		log.Info("shutdown requested")
	}

	// Shutdown order matters:
	//  1. fail /readyz so proxies stop sending traffic,
	//  2. stop accepting HTTP requests and wait for in-flight ones,
	//  3. drain the queue to the database while WebSocket clients are still connected,
	//  4. close WebSocket clients, then the database pool (deferred).
	sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer cancel()
	srv.SetDraining()
	if err := httpServer.Shutdown(sctx); err != nil {
		log.Warn("http shutdown incomplete", "error", err)
	}
	if pprofServer != nil {
		_ = pprofServer.Shutdown(sctx)
	}
	if err := pipe.Close(sctx); err != nil {
		log.Error("queue drain hit the shutdown deadline; remaining accepted events were discarded", "error", err)
	} else {
		log.Info("queue drained")
	}
	h.CloseAll()
	log.Info("shutdown complete")
	return nil
}

// startPprof serves net/http/pprof on its own listener, off by default. Bind it
// to loopback (e.g. 127.0.0.1:6060): profiles expose internals and must not be public.
func startPprof(addr string, log *slog.Logger, errc chan<- error) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	s := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("pprof server: %w", err)
		}
	}()
	log.Warn("pprof enabled; keep this address private", "addr", addr)
	return s
}

func migrate(cfg config.Config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	pool, err := store.Open(ctx, cfg.DatabaseURL, 2)
	if err != nil {
		return err
	}
	defer pool.Close()
	return runMigrations(ctx, store.New(pool, cfg.DBTimeout, metrics.New()), log)
}

// runMigrations waits up to 30s for the database (compose starts services together),
// then applies pending migrations.
func runMigrations(ctx context.Context, st *store.Store, log *slog.Logger) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := st.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("database not reachable: %w", err)
		}
		log.Info("waiting for database", "error", err)
		select {
		case <-time.After(time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	applied, err := store.Migrate(ctx, st.Pool())
	if err != nil {
		return err
	}
	log.Info("migrations complete", "applied", applied)
	return nil
}

func healthcheck(cfg config.Config) error {
	_, port, err := net.SplitHostPort(cfg.HTTPAddr)
	if err != nil {
		return fmt.Errorf("cannot derive port from %q: %w", cfg.HTTPAddr, err)
	}
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + net.JoinHostPort("127.0.0.1", strings.TrimSpace(port)) + "/readyz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}
