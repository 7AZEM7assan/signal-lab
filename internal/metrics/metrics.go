// Package metrics defines the service's Prometheus instruments.
//
// Every label is drawn from a small closed set (outcomes, validation reasons,
// alert rules, route patterns, DB operation names). Device and event IDs are
// never used as labels, to keep cardinality bounded.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"signallab/internal/alert"
	"signallab/internal/event"
)

// Ingest outcomes: attempt-level counters (a retried batch is counted again).
const (
	IngestAccepted         = "accepted"          // validated and placed on the in-memory queue
	IngestRejectedInvalid  = "rejected_invalid"  // failed validation
	IngestRejectedOverload = "rejected_overload" // valid, but refused because the queue was full
	IngestRejectedShutdown = "rejected_shutdown" // valid, but the service is draining
)

// Processing outcomes: what workers did with accepted events.
const (
	ProcessedStored    = "stored"
	ProcessedDuplicate = "duplicate"
	ProcessedFailed    = "failed"
)

// Failure stages for ProcessingFailures.
const (
	StagePersist      = "persist"       // a persist attempt failed (retried or given up)
	StageAlertPersist = "alert_persist" // alerts could not be written; events were still committed
	StageShutdownDrop = "shutdown_drop" // drain deadline hit; remaining events discarded
)

var (
	latencyBuckets = []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}
	lagBuckets     = []float64{.01, .1, .5, 1, 5, 30, 60, 300, 3600, 86400, 31536000}
)

// Metrics bundles all instruments on a private registry (so tests can create many).
type Metrics struct {
	Registry *prometheus.Registry

	IngestEvents       *prometheus.CounterVec
	ValidationFailures *prometheus.CounterVec
	ProcessedEvents    *prometheus.CounterVec
	ProcessingFailures *prometheus.CounterVec
	QueueWait          prometheus.Histogram
	ProcessingDuration prometheus.Histogram
	SourceLag          prometheus.Histogram
	AlertsCreated      *prometheus.CounterVec
	WSClients          prometheus.Gauge
	WSSlowDisconnects  prometheus.Counter
	DBDuration         *prometheus.HistogramVec
	DBErrors           *prometheus.CounterVec
	HTTPRequests       *prometheus.CounterVec
	HTTPDuration       *prometheus.HistogramVec
}

func New() *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		IngestEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_ingest_events_total", Help: "Events seen by the ingest API, by outcome (attempt-level).",
		}, []string{"outcome"}),
		ValidationFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_validation_failures_total", Help: "Records rejected by validation, by bounded reason.",
		}, []string{"reason"}),
		ProcessedEvents: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_processed_events_total", Help: "Accepted events after worker processing, by outcome.",
		}, []string{"outcome"}),
		ProcessingFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_processing_failures_total", Help: "Worker-side failures by stage.",
		}, []string{"stage"}),
		QueueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "signallab_queue_wait_seconds", Help: "Time an event waited in the queue before a worker took it.", Buckets: latencyBuckets,
		}),
		ProcessingDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "signallab_processing_duration_seconds", Help: "Duration of one persist transaction attempt for a worker batch.", Buckets: latencyBuckets,
		}),
		SourceLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "signallab_event_source_lag_seconds", Help: "received_at minus event_time for accepted events. Large for historical replays unless timestamps are rebased.", Buckets: lagBuckets,
		}),
		AlertsCreated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_alerts_created_total", Help: "Alerts persisted, by rule.",
		}, []string{"rule"}),
		WSClients: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "signallab_ws_clients", Help: "Currently connected WebSocket clients.",
		}),
		WSSlowDisconnects: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "signallab_ws_slow_disconnects_total", Help: "WebSocket clients disconnected for being too slow (send buffer full or write timeout).",
		}),
		DBDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "signallab_db_operation_duration_seconds", Help: "Database operation duration by operation.", Buckets: latencyBuckets,
		}, []string{"op"}),
		DBErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_db_errors_total", Help: "Database operation failures by operation.",
		}, []string{"op"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "signallab_http_requests_total", Help: "HTTP requests by route pattern and status code.",
		}, []string{"route", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "signallab_http_request_duration_seconds", Help: "HTTP request duration by route pattern.", Buckets: latencyBuckets,
		}, []string{"route"}),
	}
	m.Registry.MustRegister(
		collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.IngestEvents, m.ValidationFailures, m.ProcessedEvents, m.ProcessingFailures,
		m.QueueWait, m.ProcessingDuration, m.SourceLag, m.AlertsCreated,
		m.WSClients, m.WSSlowDisconnects, m.DBDuration, m.DBErrors, m.HTTPRequests, m.HTTPDuration,
	)
	m.preregister()
	return m
}

// preregister creates every bounded series at zero so dashboards and tests see them
// before the first event.
func (m *Metrics) preregister() {
	for _, o := range []string{IngestAccepted, IngestRejectedInvalid, IngestRejectedOverload, IngestRejectedShutdown} {
		m.IngestEvents.WithLabelValues(o)
	}
	for _, r := range event.Reasons {
		m.ValidationFailures.WithLabelValues(r)
	}
	for _, o := range []string{ProcessedStored, ProcessedDuplicate, ProcessedFailed} {
		m.ProcessedEvents.WithLabelValues(o)
	}
	for _, s := range []string{StagePersist, StageAlertPersist, StageShutdownDrop} {
		m.ProcessingFailures.WithLabelValues(s)
	}
	for _, r := range alert.Rules {
		m.AlertsCreated.WithLabelValues(string(r))
	}
	for _, op := range []string{"persist_batch", "query_events", "query_alerts", "ping"} {
		m.DBDuration.WithLabelValues(op)
		m.DBErrors.WithLabelValues(op)
	}
}
