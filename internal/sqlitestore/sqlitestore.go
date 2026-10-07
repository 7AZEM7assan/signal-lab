// Package sqlitestore is the embedded, single-file database used by the desktop app.
//
// It implements the same persistence and query behaviour as internal/store (PostgreSQL):
// idempotent inserts (a repeated event_id or (device_id, sequence) is skipped, not an error),
// alerts written under a savepoint so an alert failure never blocks events, and keyset
// pagination ordered by (time, id). It uses modernc.org/sqlite, a pure-Go driver, so the app
// cross-compiles without a C toolchain.
//
// Timestamps are stored as integer microseconds since the Unix epoch (UTC). That keeps
// ordering and cursor comparisons exact and independent of text formatting.
package sqlitestore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/metrics"
	"signallab/internal/store"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
    event_id       TEXT    PRIMARY KEY,
    device_id      TEXT    NOT NULL,
    site_id        TEXT,
    schema_version INTEGER NOT NULL,
    event_time     INTEGER NOT NULL,
    received_at    INTEGER NOT NULL,
    sequence       INTEGER,
    temperature_c  REAL    NOT NULL,
    vibration_mm_s REAL    NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS events_device_sequence_uq ON events (device_id, sequence) WHERE sequence IS NOT NULL;
CREATE INDEX IF NOT EXISTS events_device_time_idx ON events (device_id, event_time, event_id);
CREATE INDEX IF NOT EXISTS events_time_idx ON events (event_time, event_id);

CREATE TABLE IF NOT EXISTS alerts (
    alert_id   TEXT    PRIMARY KEY,
    event_id   TEXT    NOT NULL REFERENCES events (event_id),
    device_id  TEXT    NOT NULL,
    rule       TEXT    NOT NULL,
    threshold  REAL    NOT NULL,
    observed   REAL    NOT NULL,
    event_time INTEGER NOT NULL,
    created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS alerts_device_time_idx ON alerts (device_id, event_time, alert_id);
CREATE INDEX IF NOT EXISTS alerts_time_idx ON alerts (event_time, alert_id);
`

// Store is a SQLite-backed persistence and query layer.
type Store struct {
	db      *sql.DB
	path    string
	timeout time.Duration
	m       *metrics.Metrics
}

// Open opens (creating if needed) the database file at path and applies the schema.
func Open(path string, timeout time.Duration, m *metrics.Metrics) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data directory: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(NORMAL)")
	dsn := "file:" + filepath.ToSlash(path) + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db, path: path, timeout: timeout, m: m}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Path returns the database file path.
func (s *Store) Path() string { return s.path }

func (s *Store) observe(op string, start time.Time, err error) {
	s.m.DBDuration.WithLabelValues(op).Observe(time.Since(start).Seconds())
	if err != nil {
		s.m.DBErrors.WithLabelValues(op).Inc()
	}
}

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) (err error) {
	start := time.Now()
	defer func() { s.observe("ping", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	return s.db.PingContext(ctx)
}

func micro(t time.Time) int64 { return t.UnixMicro() }

func fromMicro(us int64) time.Time { return time.UnixMicro(us).UTC() }

// PersistBatch stores events and the alerts they trigger in one transaction, with the same
// semantics as store.Store.PersistBatch: duplicates are skipped, alerts are written under a
// savepoint, and a failure of anything else rolls the whole batch back.
func (s *Store) PersistBatch(ctx context.Context, evs []event.Event, th alert.Thresholds) (res store.Result, err error) {
	start := time.Now()
	defer func() { s.observe("persist_batch", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return store.Result{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	ins, err := tx.PrepareContext(ctx, `
		INSERT INTO events (event_id, device_id, site_id, schema_version, event_time, received_at, sequence, temperature_c, vibration_mm_s)
		VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return store.Result{}, fmt.Errorf("prepare insert: %w", err)
	}
	defer ins.Close()
	for _, ev := range evs {
		var seq any
		if ev.Sequence != nil {
			seq = *ev.Sequence
		}
		r, err := ins.ExecContext(ctx, ev.EventID, ev.DeviceID, ev.SiteID, ev.SchemaVersion,
			micro(ev.EventTime), micro(ev.ReceivedAt), seq, ev.TemperatureC, ev.VibrationMMS)
		if err != nil {
			return store.Result{}, fmt.Errorf("insert events: %w", err)
		}
		if n, _ := r.RowsAffected(); n == 1 {
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
	if err := tx.Commit(); err != nil {
		return store.Result{}, fmt.Errorf("commit: %w", err)
	}
	return res, nil
}

func insertAlertsInSavepoint(ctx context.Context, tx *sql.Tx, alerts []alert.Alert) error {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT alerts_sp"); err != nil {
		return err
	}
	rollback := func(cause error) error {
		_, _ = tx.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO alerts_sp")
		_, _ = tx.ExecContext(context.WithoutCancel(ctx), "RELEASE alerts_sp")
		return cause
	}
	st, err := tx.PrepareContext(ctx, `
		INSERT INTO alerts (alert_id, event_id, device_id, rule, threshold, observed, event_time, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`)
	if err != nil {
		return rollback(err)
	}
	defer st.Close()
	for _, a := range alerts {
		if _, err := st.ExecContext(ctx, a.ID, a.EventID, a.DeviceID, string(a.Rule), a.Threshold, a.Observed,
			micro(a.EventTime), micro(a.CreatedAt)); err != nil {
			return rollback(err)
		}
	}
	_, err = tx.ExecContext(ctx, "RELEASE alerts_sp")
	return err
}

// QueryEvents returns up to q.Limit events (ordered by event_time, event_id) and a cursor when more exist.
func (s *Store) QueryEvents(ctx context.Context, q store.Query) (out []store.StoredEvent, next *store.Page, err error) {
	start := time.Now()
	defer func() { s.observe("query_events", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	text, args := buildQuery(`SELECT event_id, device_id, COALESCE(site_id, ''), schema_version, event_time, received_at, sequence, temperature_c, vibration_mm_s FROM events`,
		"event_time", "event_id", q)
	rows, err := s.db.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ev store.StoredEvent
		var et, ra int64
		var seq sql.NullInt64
		if err := rows.Scan(&ev.EventID, &ev.DeviceID, &ev.SiteID, &ev.SchemaVersion, &et, &ra, &seq, &ev.TemperatureC, &ev.VibrationMMS); err != nil {
			return nil, nil, err
		}
		ev.EventTime, ev.ReceivedAt = fromMicro(et), fromMicro(ra)
		if seq.Valid {
			v := seq.Int64
			ev.Sequence = &v
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		next = &store.Page{Time: last.EventTime, ID: last.EventID}
	}
	return out, next, nil
}

// QueryAlerts is QueryEvents for alerts (ordered by event_time, alert_id).
func (s *Store) QueryAlerts(ctx context.Context, q store.Query) (out []alert.Alert, next *store.Page, err error) {
	start := time.Now()
	defer func() { s.observe("query_alerts", start, err) }()
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	text, args := buildQuery(`SELECT alert_id, device_id, event_id, rule, threshold, observed, event_time, created_at FROM alerts`,
		"event_time", "alert_id", q)
	rows, err := s.db.QueryContext(ctx, text, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var a alert.Alert
		var rule string
		var et, ca int64
		if err := rows.Scan(&a.ID, &a.DeviceID, &a.EventID, &rule, &a.Threshold, &a.Observed, &et, &ca); err != nil {
			return nil, nil, err
		}
		a.Rule = alert.Rule(rule)
		a.EventTime, a.CreatedAt = fromMicro(et), fromMicro(ca)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		next = &store.Page{Time: last.EventTime, ID: last.ID}
	}
	return out, next, nil
}

// buildQuery appends WHERE/ORDER/LIMIT. Column names are compile-time constants; every value is a bind parameter.
func buildQuery(base, timeCol, idCol string, q store.Query) (string, []any) {
	where := timeCol + " >= ? AND " + timeCol + " < ?"
	args := []any{micro(q.From), micro(q.To)}
	if q.DeviceID != "" {
		where += " AND device_id = ?"
		args = append(args, q.DeviceID)
	}
	if q.After != nil {
		where += " AND (" + timeCol + ", " + idCol + ") > (?, ?)"
		args = append(args, micro(q.After.Time), q.After.ID)
	}
	args = append(args, q.Limit+1)
	return base + " WHERE " + where + " ORDER BY " + timeCol + ", " + idCol + " LIMIT ?", args
}

// Stats summarises what is stored, for the storage panel.
type Stats struct {
	Events     int64      `json:"events"`
	Alerts     int64      `json:"alerts"`
	Devices    int64      `json:"devices"`
	OldestTime *time.Time `json:"oldest_event_time,omitempty"`
	NewestTime *time.Time `json:"newest_event_time,omitempty"`
	DBBytes    int64      `json:"db_bytes"`
	Path       string     `json:"path"`
}

// Stats counts rows and reports the on-disk size (database file plus its write-ahead log).
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	st := Stats{Path: s.path}
	var oldest, newest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COUNT(DISTINCT device_id), MIN(event_time), MAX(event_time) FROM events`).
		Scan(&st.Events, &st.Devices, &oldest, &newest)
	if err != nil {
		return Stats{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts`).Scan(&st.Alerts); err != nil {
		return Stats{}, err
	}
	if oldest.Valid {
		t := fromMicro(oldest.Int64)
		st.OldestTime = &t
	}
	if newest.Valid {
		t := fromMicro(newest.Int64)
		st.NewestTime = &t
	}
	for _, p := range []string{s.path, s.path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			st.DBBytes += fi.Size()
		} else if !errors.Is(err, os.ErrNotExist) {
			return Stats{}, err
		}
	}
	return st, nil
}

// Clear deletes every event and alert and compacts the file. It returns the rows removed.
func (s *Store) Clear(ctx context.Context) (events, alerts int64, err error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	ra, err := tx.ExecContext(ctx, `DELETE FROM alerts`)
	if err != nil {
		return 0, 0, err
	}
	re, err := tx.ExecContext(ctx, `DELETE FROM events`)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	events, _ = re.RowsAffected()
	alerts, _ = ra.RowsAffected()
	// Reclaim space; best effort, the data is already gone.
	_, _ = s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	_, _ = s.db.ExecContext(ctx, `VACUUM`)
	return events, alerts, nil
}

// Devices lists every device that has stored events, sorted by name, with its event count.
func (s *Store) Devices(ctx context.Context) ([]DeviceCount, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT device_id, COUNT(*) FROM events GROUP BY device_id ORDER BY device_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DeviceCount{}
	for rows.Next() {
		var d DeviceCount
		if err := rows.Scan(&d.DeviceID, &d.Events); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// DeviceCount is one row of Devices.
type DeviceCount struct {
	DeviceID string `json:"device_id"`
	Events   int64  `json:"events"`
}
