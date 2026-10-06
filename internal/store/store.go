// Package store persists events and alerts in PostgreSQL.
//
// All SQL is parameterised. Every operation runs under a context deadline so a
// stalled database cannot hang a worker or an HTTP handler indefinitely.
package store

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/metrics"
)

type Store struct {
	pool    *pgxpool.Pool
	timeout time.Duration
	m       *metrics.Metrics
}

// New wraps an existing pool. timeout bounds each operation.
func New(pool *pgxpool.Pool, timeout time.Duration, m *metrics.Metrics) *Store {
	return &Store{pool: pool, timeout: timeout, m: m}
}

// Open creates a pool. It does not dial: connections are made lazily, so the
// service can start before the database is reachable and report that via /readyz.
func Open(ctx context.Context, url string, maxConns int) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("invalid database url: %w", err)
	}
	cfg.MaxConns = int32(maxConns)
	cfg.HealthCheckPeriod = 15 * time.Second
	return pgxpool.NewWithConfig(ctx, cfg)
}

// Pool exposes the pool for migrations and shutdown.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// observe records duration and failure for one database operation.
func (s *Store) observe(op string, start time.Time, err error) {
	s.m.DBDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	if err != nil {
		s.m.DBErrors.WithLabelValues(op).Inc()
	}
}

// Ping checks connectivity for readiness.
func (s *Store) Ping(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { s.observe("ping", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.pool.Ping(ctx)
}

// Result describes one PersistBatch call.
type Result struct {
	Inserted []event.Event // newly stored events (duplicates omitted), in input order
	Alerts   []alert.Alert // alerts committed for the inserted events
	// AlertErr is non-nil when alert rows could not be written. The events were
	// still committed: alert failure never blocks event persistence.
	AlertErr error
}

// PersistBatch stores events and the alerts they trigger in one transaction.
//
// Transaction boundary: events are inserted first (ON CONFLICT DO NOTHING, so
// duplicates are skipped, not errors). Alerts for the newly inserted events are
// written inside a savepoint: if that statement fails the savepoint is rolled
// back, the events still commit, and the failure is reported in Result.AlertErr.
// A failure of anything else rolls the whole transaction back and returns an error.
func (s *Store) PersistBatch(ctx context.Context, evs []event.Event, th alert.Thresholds) (res Result, err error) {
	start := time.Now()
	defer func() { s.observe("persist_batch", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }() // no-op after Commit

	insertedIDs, err := insertEvents(ctx, tx, evs)
	if err != nil {
		return Result{}, fmt.Errorf("insert events: %w", err)
	}
	// If the same event_id appears twice in one worker batch only one row can be
	// inserted; count the first occurrence as the inserted one.
	taken := make(map[string]bool, len(insertedIDs))
	for _, ev := range evs {
		if insertedIDs[ev.EventID] && !taken[ev.EventID] {
			taken[ev.EventID] = true
			res.Inserted = append(res.Inserted, ev)
		}
	}

	var alerts []alert.Alert
	now := time.Now()
	for _, ev := range res.Inserted {
		alerts = append(alerts, th.Evaluate(ev, now)...)
	}
	if len(alerts) > 0 {
		if aerr := insertAlertsInSavepoint(ctx, tx, alerts); aerr != nil {
			res.AlertErr = aerr
		} else {
			res.Alerts = alerts
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

func insertEvents(ctx context.Context, tx pgx.Tx, evs []event.Event) (map[string]bool, error) {
	n := len(evs)
	ids, devices, sites := make([]string, n), make([]string, n), make([]string, n)
	versions, seqs := make([]int32, n), make([]int64, n)
	eventTimes, receivedAt := make([]time.Time, n), make([]time.Time, n)
	temps, vibs := make([]float64, n), make([]float64, n)
	for i, ev := range evs {
		ids[i], devices[i], sites[i] = ev.EventID, ev.DeviceID, ev.SiteID
		versions[i] = int32(ev.SchemaVersion)
		seqs[i] = -1 // sentinel for "no sequence": valid sequences are >= 0
		if ev.Sequence != nil {
			seqs[i] = *ev.Sequence
		}
		eventTimes[i], receivedAt[i] = ev.EventTime, ev.ReceivedAt
		temps[i], vibs[i] = ev.TemperatureC, ev.VibrationMMS
	}
	rows, err := tx.Query(ctx, `
		INSERT INTO events (event_id, device_id, site_id, schema_version, event_time, received_at, sequence, temperature_c, vibration_mm_s)
		SELECT t.event_id, t.device_id, NULLIF(t.site_id, ''), t.schema_version, t.event_time, t.received_at, NULLIF(t.sequence, -1), t.temperature_c, t.vibration_mm_s
		FROM unnest($1::text[], $2::text[], $3::text[], $4::int[], $5::timestamptz[], $6::timestamptz[], $7::bigint[], $8::float8[], $9::float8[])
		     AS t(event_id, device_id, site_id, schema_version, event_time, received_at, sequence, temperature_c, vibration_mm_s)
		ON CONFLICT DO NOTHING
		RETURNING event_id`,
		ids, devices, sites, versions, eventTimes, receivedAt, seqs, temps, vibs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	inserted := make(map[string]bool, n)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		inserted[id] = true
	}
	return inserted, rows.Err()
}

func insertAlertsInSavepoint(ctx context.Context, tx pgx.Tx, alerts []alert.Alert) error {
	sp, err := tx.Begin(ctx) // pgx implements nested Begin as SAVEPOINT
	if err != nil {
		return err
	}
	n := len(alerts)
	ids, evIDs, devs, rules := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	thr, obs := make([]float64, n), make([]float64, n)
	evTimes, created := make([]time.Time, n), make([]time.Time, n)
	for i, a := range alerts {
		ids[i], evIDs[i], devs[i], rules[i] = a.ID, a.EventID, a.DeviceID, string(a.Rule)
		thr[i], obs[i], evTimes[i], created[i] = a.Threshold, a.Observed, a.EventTime, a.CreatedAt
	}
	_, err = sp.Exec(ctx, `
		INSERT INTO alerts (alert_id, event_id, device_id, rule, threshold, observed, event_time, created_at)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::float8[], $6::float8[], $7::timestamptz[], $8::timestamptz[])
		ON CONFLICT DO NOTHING`,
		ids, evIDs, devs, rules, thr, obs, evTimes, created)
	if err != nil {
		_ = sp.Rollback(context.WithoutCancel(ctx))
		return err
	}
	return sp.Commit(ctx)
}

// ---- queries ----

// Page is the keyset position after the last row of the previous page.
type Page struct {
	Time time.Time
	ID   string
}

// Query selects rows by optional device and a half-open time range [From, To).
// Results are ordered by (event_time, id). Limit is applied by the caller's policy;
// one extra row is fetched internally to learn whether more exist.
type Query struct {
	DeviceID string
	From, To time.Time
	Limit    int
	After    *Page
}

// EncodeCursor turns a keyset position into an opaque token.
func EncodeCursor(p Page) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(p.Time.UnixMicro(), 10) + "|" + p.ID))
}

// DecodeCursor reverses EncodeCursor.
func DecodeCursor(s string) (*Page, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("malformed cursor")
	}
	us, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return nil, errors.New("malformed cursor")
	}
	micro, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return nil, errors.New("malformed cursor")
	}
	return &Page{Time: time.UnixMicro(micro).UTC(), ID: id}, nil
}

// StoredEvent is an event as read back, including the server-assigned received_at.
type StoredEvent = event.Event

// QueryEvents returns up to q.Limit events and, when more exist, a cursor for the next page.
func (s *Store) QueryEvents(ctx context.Context, q Query) (out []StoredEvent, next *Page, err error) {
	start := time.Now()
	defer func() { s.observe("query_events", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	sqlText, args := buildQuery(`SELECT event_id, device_id, COALESCE(site_id, ''), schema_version, event_time, received_at, sequence, temperature_c, vibration_mm_s FROM events`,
		"event_time", "event_id", q)
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ev StoredEvent
		if err := rows.Scan(&ev.EventID, &ev.DeviceID, &ev.SiteID, &ev.SchemaVersion, &ev.EventTime, &ev.ReceivedAt, &ev.Sequence, &ev.TemperatureC, &ev.VibrationMMS); err != nil {
			return nil, nil, err
		}
		ev.EventTime, ev.ReceivedAt = ev.EventTime.UTC(), ev.ReceivedAt.UTC()
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		next = &Page{Time: last.EventTime, ID: last.EventID}
	}
	return out, next, nil
}

// QueryAlerts is QueryEvents for alerts (ordered by event_time, alert_id).
func (s *Store) QueryAlerts(ctx context.Context, q Query) (out []alert.Alert, next *Page, err error) {
	start := time.Now()
	defer func() { s.observe("query_alerts", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	sqlText, args := buildQuery(`SELECT alert_id, device_id, event_id, rule, threshold, observed, event_time, created_at FROM alerts`,
		"event_time", "alert_id", q)
	rows, err := s.pool.Query(ctx, sqlText, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a alert.Alert
		var rule string
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.EventID, &rule, &a.Threshold, &a.Observed, &a.EventTime, &a.CreatedAt); err != nil {
			return nil, nil, err
		}
		a.Rule = alert.Rule(rule)
		a.EventTime, a.CreatedAt = a.EventTime.UTC(), a.CreatedAt.UTC()
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		next = &Page{Time: last.EventTime, ID: last.ID}
	}
	return out, next, nil
}

// buildQuery appends WHERE/ORDER/LIMIT to base. Column names are compile-time
// constants from this package; every value is a bind parameter.
func buildQuery(base, timeCol, idCol string, q Query) (string, []any) {
	var where []string
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	where = append(where, timeCol+" >= "+arg(q.From), timeCol+" < "+arg(q.To))
	if q.DeviceID != "" {
		where = append(where, "device_id = "+arg(q.DeviceID))
	}
	if q.After != nil {
		where = append(where, "("+timeCol+", "+idCol+") > ("+arg(q.After.Time)+", "+arg(q.After.ID)+")")
	}
	return base + " WHERE " + strings.Join(where, " AND ") +
		" ORDER BY " + timeCol + ", " + idCol + " LIMIT " + arg(q.Limit+1), args
}
