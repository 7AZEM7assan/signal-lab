// Package config loads and validates service settings from SIGNALLAB_* environment
// variables. Invalid settings fail startup with every problem listed at once.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds every tunable of the service. Defaults suit a local demo.
type Config struct {
	HTTPAddr    string
	DatabaseURL string
	DBMaxConns  int
	DBTimeout   time.Duration // per-query / per-transaction timeout

	// Pipeline: bounded queue and worker pool.
	QueueCapacity  int
	Workers        int
	WorkerBatch    int // max events a worker persists in one transaction
	PersistRetries int // total attempts per worker batch (>=1)
	PersistBackoff time.Duration
	LabWorkerDelay time.Duration // artificial per-batch delay; fault-injection knob for overload demos

	// Ingestion limits and plausibility bounds.
	MaxBodyBytes   int64
	MaxBatchEvents int
	MaxFutureSkew  time.Duration
	TempMinC       float64
	TempMaxC       float64
	VibMaxMMS      float64

	// Alert thresholds (inclusive: value >= threshold fires).
	TempAlertC float64
	VibAlertMS float64

	// Query API.
	QueryDefaultLimit int
	QueryMaxLimit     int
	QueryMaxRange     time.Duration

	// WebSocket hub.
	WSClientBuffer   int
	WSMaxClients     int
	WSWriteTimeout   time.Duration
	WSAllowedOrigins []string

	ShutdownTimeout time.Duration
	AutoMigrate     bool
	LogLevel        string
	LogFormat       string // "json" or "text"
	PprofAddr       string // empty disables pprof; keep it on loopback
}

const prefix = "SIGNALLAB_"

// Load reads the environment. lookup is os.LookupEnv in production and a fake in tests.
func Load(lookup func(string) (string, bool)) (Config, error) {
	r := &reader{lookup: lookup}
	c := Config{
		HTTPAddr:    r.str("HTTP_ADDR", ":8080"),
		DatabaseURL: r.str("DATABASE_URL", ""),
		DBMaxConns:  r.integer("DB_MAX_CONNS", 10),
		DBTimeout:   r.duration("DB_QUERY_TIMEOUT", 5*time.Second),

		QueueCapacity:  r.integer("QUEUE_CAPACITY", 1000),
		Workers:        r.integer("WORKERS", 4),
		WorkerBatch:    r.integer("WORKER_BATCH_SIZE", 100),
		PersistRetries: r.integer("PERSIST_ATTEMPTS", 10),
		PersistBackoff: r.duration("PERSIST_BACKOFF", 200*time.Millisecond),
		LabWorkerDelay: r.duration("LAB_WORKER_DELAY", 0),

		MaxBodyBytes:   int64(r.integer("MAX_BODY_BYTES", 1<<20)),
		MaxBatchEvents: r.integer("MAX_BATCH_EVENTS", 500),
		MaxFutureSkew:  r.duration("MAX_FUTURE_SKEW", 5*time.Minute),
		TempMinC:       r.float("TEMP_MIN_C", -50),
		TempMaxC:       r.float("TEMP_MAX_C", 250),
		VibMaxMMS:      r.float("VIB_MAX_MM_S", 100),

		TempAlertC: r.float("TEMP_ALERT_C", 85),
		VibAlertMS: r.float("VIB_ALERT_MM_S", 7.1),

		QueryDefaultLimit: r.integer("QUERY_DEFAULT_LIMIT", 100),
		QueryMaxLimit:     r.integer("QUERY_MAX_LIMIT", 1000),
		QueryMaxRange:     r.duration("QUERY_MAX_RANGE", 24*time.Hour),

		WSClientBuffer:   r.integer("WS_CLIENT_BUFFER", 1024),
		WSMaxClients:     r.integer("WS_MAX_CLIENTS", 256),
		WSWriteTimeout:   r.duration("WS_WRITE_TIMEOUT", 5*time.Second),
		WSAllowedOrigins: r.list("WS_ALLOWED_ORIGINS"),

		ShutdownTimeout: r.duration("SHUTDOWN_TIMEOUT", 15*time.Second),
		AutoMigrate:     r.boolean("AUTO_MIGRATE", false),
		LogLevel:        r.str("LOG_LEVEL", "info"),
		LogFormat:       r.str("LOG_FORMAT", "json"),
		PprofAddr:       r.str("PPROF_ADDR", ""),
	}
	errs := r.errs
	errs = append(errs, c.validate()...)
	return c, errors.Join(errs...)
}

func (c Config) validate() []error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	if c.DatabaseURL == "" {
		bad("%sDATABASE_URL is required", prefix)
	}
	positive := map[string]int{
		"DB_MAX_CONNS": c.DBMaxConns, "QUEUE_CAPACITY": c.QueueCapacity, "WORKERS": c.Workers,
		"WORKER_BATCH_SIZE": c.WorkerBatch, "PERSIST_ATTEMPTS": c.PersistRetries,
		"MAX_BATCH_EVENTS": c.MaxBatchEvents, "QUERY_DEFAULT_LIMIT": c.QueryDefaultLimit,
		"QUERY_MAX_LIMIT": c.QueryMaxLimit, "WS_CLIENT_BUFFER": c.WSClientBuffer, "WS_MAX_CLIENTS": c.WSMaxClients,
	}
	for name, v := range positive {
		if v < 1 {
			bad("%s%s must be >= 1 (got %d)", prefix, name, v)
		}
	}
	if c.MaxBodyBytes < 1 {
		bad("%sMAX_BODY_BYTES must be >= 1", prefix)
	}
	// A batch is enqueued all-or-nothing, so one larger than the queue could never be accepted.
	if c.MaxBatchEvents > c.QueueCapacity {
		bad("%sMAX_BATCH_EVENTS (%d) must not exceed %sQUEUE_CAPACITY (%d)", prefix, c.MaxBatchEvents, prefix, c.QueueCapacity)
	}
	if c.QueryDefaultLimit > c.QueryMaxLimit {
		bad("%sQUERY_DEFAULT_LIMIT must not exceed %sQUERY_MAX_LIMIT", prefix, prefix)
	}
	if c.TempMinC >= c.TempMaxC {
		bad("%sTEMP_MIN_C must be below %sTEMP_MAX_C", prefix, prefix)
	}
	if c.VibMaxMMS <= 0 {
		bad("%sVIB_MAX_MM_S must be > 0", prefix)
	}
	if c.TempAlertC < c.TempMinC || c.TempAlertC > c.TempMaxC {
		bad("%sTEMP_ALERT_C must lie within the temperature bounds", prefix)
	}
	if c.VibAlertMS < 0 || c.VibAlertMS > c.VibMaxMMS {
		bad("%sVIB_ALERT_MM_S must lie within [0, VIB_MAX_MM_S]", prefix)
	}
	for name, d := range map[string]time.Duration{
		"DB_QUERY_TIMEOUT": c.DBTimeout, "QUERY_MAX_RANGE": c.QueryMaxRange,
		"WS_WRITE_TIMEOUT": c.WSWriteTimeout, "SHUTDOWN_TIMEOUT": c.ShutdownTimeout,
	} {
		if d <= 0 {
			bad("%s%s must be > 0", prefix, name)
		}
	}
	if c.PersistBackoff < 0 || c.LabWorkerDelay < 0 || c.MaxFutureSkew < 0 {
		bad("durations PERSIST_BACKOFF, LAB_WORKER_DELAY and MAX_FUTURE_SKEW must not be negative")
	}
	if c.LogFormat != "json" && c.LogFormat != "text" {
		bad("%sLOG_FORMAT must be json or text", prefix)
	}
	switch strings.ToLower(c.LogLevel) {
	case "debug", "info", "warn", "error":
	default:
		bad("%sLOG_LEVEL must be debug, info, warn or error", prefix)
	}
	return errs
}

// OS loads from the process environment.
func OS() (Config, error) { return Load(os.LookupEnv) }

type reader struct {
	lookup func(string) (string, bool)
	errs   []error
}

func (r *reader) raw(name string) (string, bool) {
	v, ok := r.lookup(prefix + name)
	v = strings.TrimSpace(v)
	return v, ok && v != ""
}

func (r *reader) str(name, def string) string {
	if v, ok := r.raw(name); ok {
		return v
	}
	return def
}

func (r *reader) integer(name string, def int) int {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s%s: %q is not an integer", prefix, name, v))
		return def
	}
	return n
}

func (r *reader) float(name string, def float64) float64 {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s%s: %q is not a number", prefix, name, v))
		return def
	}
	return f
}

func (r *reader) duration(name string, def time.Duration) time.Duration {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s%s: %q is not a duration (e.g. 500ms, 5s)", prefix, name, v))
		return def
	}
	return d
}

func (r *reader) boolean(name string, def bool) bool {
	v, ok := r.raw(name)
	if !ok {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.errs = append(r.errs, fmt.Errorf("%s%s: %q is not a boolean", prefix, name, v))
		return def
	}
	return b
}

func (r *reader) list(name string) []string {
	v, ok := r.raw(name)
	if !ok {
		return nil
	}
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
