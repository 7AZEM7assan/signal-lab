// Package api exposes the HTTP and WebSocket interface.
package api

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"sync/atomic"
	"time"

	"signallab/internal/alert"
	"signallab/internal/config"
	"signallab/internal/event"
	"signallab/internal/hub"
	"signallab/internal/metrics"
	"signallab/internal/store"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Ingestor is the write path (implemented by *pipeline.Pipeline).
type Ingestor interface {
	Enqueue(batchID string, evs []event.Event) error
	Depth() int
	Capacity() int
}

// Querier is the read path and readiness probe (implemented by *store.Store).
type Querier interface {
	Ping(ctx context.Context) error
	QueryEvents(ctx context.Context, q store.Query) ([]event.Event, *store.Page, error)
	QueryAlerts(ctx context.Context, q store.Query) ([]alert.Alert, *store.Page, error)
}

type Deps struct {
	Cfg     config.Config
	Ingest  Ingestor
	DB      Querier
	Hub     *hub.Hub
	Metrics *metrics.Metrics
	Log     *slog.Logger
	Now     func() time.Time // defaults to time.Now; injectable for tests
}

type Server struct {
	Deps
	limits   event.Limits
	draining atomic.Bool
}

// New builds the handler tree.
func New(d Deps) *Server {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Server{Deps: d, limits: event.Limits{
		TempMinC: d.Cfg.TempMinC, TempMaxC: d.Cfg.TempMaxC, VibMaxMMS: d.Cfg.VibMaxMMS, MaxFutureSkew: d.Cfg.MaxFutureSkew,
	}}
}

// SetDraining makes /readyz fail so a proxy stops routing here before shutdown completes.
func (s *Server) SetDraining() { s.draining.Store(true) }

// Handler returns the full handler including middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("POST /api/v1/events", s.handleIngest)
	mux.HandleFunc("GET /api/v1/events", s.handleQueryEvents)
	mux.HandleFunc("GET /api/v1/alerts", s.handleQueryAlerts)
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.Metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /ws", s.handleWS)
	return s.middleware(mux)
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{"database": "ok", "draining": "no"}
	ready := true
	if err := s.DB.Ping(r.Context()); err != nil {
		checks["database"] = "unavailable"
		ready = false
	}
	if s.draining.Load() {
		checks["draining"] = "yes"
		ready = false
	}
	status, code := "ready", http.StatusOK
	if !ready {
		status, code = "not_ready", http.StatusServiceUnavailable
	}
	writeJSON(w, code, map[string]any{"status": status, "checks": checks})
}

// ---- JSON helpers ----

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v) // a failed write means the client left
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]apiError{"error": {Code: code, Message: msg}})
}

// ---- middleware: request id, panic recovery, access log, metrics ----

type ctxKey struct{}

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID returns the request's ID (also used as the batch ID on ingest).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

type statusWriter struct {
	http.ResponseWriter
	status   int
	hijacked bool
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

// Hijack is required for WebSocket upgrades.
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.hijacked = true
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := r.Header.Get("X-Request-ID")
		if !requestIDPattern.MatchString(id) {
			id = newID()
		}
		w.Header().Set("X-Request-ID", id)
		sw := &statusWriter{ResponseWriter: w}
		r = r.WithContext(context.WithValue(r.Context(), ctxKey{}, id))

		defer func() {
			if rec := recover(); rec != nil {
				s.Log.Error("panic in handler", "request_id", id, "panic", rec)
				if sw.status == 0 {
					writeError(sw, http.StatusInternalServerError, "internal_error", "internal error")
				}
			}
			// r.Pattern is filled in by the mux during ServeHTTP; it is a bounded set of routes.
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			code := sw.status
			if code == 0 {
				code = http.StatusOK
			}
			s.Metrics.HTTPRequests.WithLabelValues(route, strconv.Itoa(code)).Inc()
			if route != "GET /ws" { // WebSocket lifetimes would swamp the latency histogram
				s.Metrics.HTTPDuration.WithLabelValues(route).Observe(time.Since(start).Seconds())
			}
			if route != "GET /metrics" && route != "GET /healthz" && route != "GET /readyz" { // keep scrape/probe noise out of logs
				s.Log.Info("request", "request_id", id, "route", route, "status", code, "duration_ms", time.Since(start).Milliseconds())
			}
		}()
		next.ServeHTTP(sw, r)
	})
}
