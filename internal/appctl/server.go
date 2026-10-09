package appctl

import (
	"context"
	"crypto/subtle"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/lab"
	"signallab/internal/sqlitestore"
	"signallab/internal/store"
)

// TokenHeader and CookieName carry the per-launch secret. Browsers use the cookie (set when the
// panel is opened with ?token=...); scripts and the in-process replay use the header.
const (
	TokenHeader = "X-SignalLab-Token"
	CookieName  = "sl_token"
)

// Server is the HTTP front of the app: static panel, /app/api, and the ingest/query API.
type Server struct {
	Engine   *Engine
	UI       http.Handler
	Token    string
	Port     int
	DataDir  string
	Version  string
	Started  time.Time
	Log      *slog.Logger
	Shutdown func() // asks the process to exit gracefully
}

// Handler returns the complete handler tree, wrapped in the host and token checks.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /app/api/state", s.handleState)
	mux.HandleFunc("GET /app/api/replay", s.handleReplayStatus)
	mux.HandleFunc("POST /app/api/replay/start", s.handleReplayStart)
	mux.HandleFunc("POST /app/api/replay/stop", s.handleReplayStop)
	mux.HandleFunc("GET /app/api/replay/dataset", s.handleDatasetGet)
	mux.HandleFunc("PUT /app/api/replay/dataset", s.handleDatasetPut)
	mux.HandleFunc("DELETE /app/api/replay/dataset", s.handleDatasetDelete)
	mux.HandleFunc("GET /app/api/settings", s.handleSettingsGet)
	mux.HandleFunc("PUT /app/api/settings", s.handleSettingsPut)
	mux.HandleFunc("POST /app/api/settings/reset", s.handleSettingsReset)
	mux.HandleFunc("GET /app/api/data", s.handleData)
	mux.HandleFunc("GET /app/api/devices", s.handleDevices)
	mux.HandleFunc("GET /app/api/export", s.handleExport)
	mux.HandleFunc("POST /app/api/storage/clear", s.handleClear)
	mux.HandleFunc("POST /app/api/shutdown", s.handleShutdown)
	mux.Handle("/app/api/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown control endpoint")
	}))
	for _, p := range []string{"/api/", "/ws", "/metrics", "/readyz", "/healthz"} {
		mux.Handle(p, s.Engine.Handler())
	}
	mux.Handle("/", s.UI)
	return s.guard(mux)
}

// ---- security: host check (DNS rebinding), token, same-origin for writes ----

func (s *Server) allowedHost(h string) bool {
	port := strconv.Itoa(s.Port)
	for _, name := range []string{"127.0.0.1", "localhost", "[::1]"} {
		if h == name+":"+port {
			return true
		}
	}
	return false
}

func (s *Server) tokenOK(r *http.Request) (viaHeader, ok bool) {
	if h := r.Header.Get(TokenHeader); h != "" {
		return true, subtle.ConstantTimeCompare([]byte(h), []byte(s.Token)) == 1
	}
	if c, err := r.Cookie(CookieName); err == nil {
		return false, subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.Token)) == 1
	}
	return false, false
}

func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Cache-Control", "no-store")
		// A web page on another site can make the browser send a request to 127.0.0.1, and DNS
		// rebinding can even make it same-origin. Only our own listener's names are accepted.
		if !s.allowedHost(r.Host) {
			writeErr(w, http.StatusForbidden, "bad_host", "unexpected Host header")
			return
		}
		// Liveness probes carry no data and are used by the launcher before it has the cookie.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		// Opening the panel with ?token=... sets the session cookie and drops the token from the URL.
		if r.Method == http.MethodGet && r.URL.Path == "/" {
			if t := r.URL.Query().Get("token"); t != "" {
				if subtle.ConstantTimeCompare([]byte(t), []byte(s.Token)) != 1 {
					writeErr(w, http.StatusUnauthorized, "bad_token", "the link's token is wrong or from an earlier launch")
					return
				}
				http.SetCookie(w, &http.Cookie{Name: CookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
				http.Redirect(w, r, "/", http.StatusSeeOther)
				return
			}
		}
		viaHeader, ok := s.tokenOK(r)
		if !ok {
			if r.URL.Path == "/" {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `<!doctype html><meta charset="utf-8"><title>Signal Lab</title><body style="font:16px system-ui;margin:2rem"><h1>Signal Lab</h1><p>This page needs the link printed when the app started (it contains a one-time token), or the desktop app window.</p>`)
				return
			}
			writeErr(w, http.StatusUnauthorized, "unauthorized", "missing or wrong token")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// Cookie-authenticated writes must come from our own page: same Origin and a header a
			// cross-site form cannot set. Requests using the token header are not browser-ambient.
			if !viaHeader {
				if o := r.Header.Get("Origin"); o != "" {
					if u, err := url.Parse(o); err != nil || u.Host != r.Host {
						writeErr(w, http.StatusForbidden, "bad_origin", "cross-origin request refused")
						return
					}
				}
				if r.Header.Get("X-Requested-With") != "signallab" {
					writeErr(w, http.StatusForbidden, "missing_header", "X-Requested-With: signallab is required")
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

// decodeStrict reads a small JSON body into v, rejecting unknown fields and trailing data.
func decodeStrict(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", "request body is not valid: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid_json", "unexpected data after the JSON object")
		return false
	}
	return true
}

// ---- handlers ----

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st, err := s.Engine.Store().Stats(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version": s.Version, "started_at": s.Started.UTC(), "data_dir": s.DataDir,
		"engine": s.Engine.Info(), "storage": st, "defaults": DefaultSettings(), "replay_defaults": lab.Defaults(),
		"dataset":       s.Engine.Runner.DatasetSummary(),
		"replay_limits": map[string]any{"external_batch": lab.MaxExternalBatch, "external_rate": lab.MaxExternalRate, "dataset_bytes": lab.MaxDatasetBytes, "dataset_rows": lab.MaxDatasetRows},
	})
}

func (s *Server) handleReplayStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Engine.Runner.Snapshot())
}

func (s *Server) handleReplayStart(w http.ResponseWriter, r *http.Request) {
	cfg := lab.Defaults() // omitted fields keep their defaults
	if !decodeStrict(w, r, &cfg) {
		return
	}
	// The built-in service needs the app's token. A service of the user's own never gets it: the
	// runner builds that target from the request itself and ignores these headers.
	target := lab.Target{
		BaseURL:   "http://127.0.0.1:" + strconv.Itoa(s.Port),
		Headers:   map[string]string{TokenHeader: s.Token},
		UserAgent: "SignalLab/" + s.Version,
	}
	snap, err := s.Engine.Runner.Start(cfg, target, s.Engine.MaxBatch())
	switch {
	case errors.Is(err, lab.ErrBusy):
		writeErr(w, http.StatusConflict, "replay_running", err.Error())
	case err != nil:
		writeErr(w, http.StatusBadRequest, "invalid_replay", err.Error())
	default:
		// Only the host is logged: an address may carry a key in its path or query string.
		s.Log.Info("replay started", "planned", snap.Planned, "batches", snap.Batches, "source", snap.Source, "target", snap.Target)
		writeJSON(w, http.StatusAccepted, snap)
	}
}

func (s *Server) handleReplayStop(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.Engine.Runner.Stop())
}

func (s *Server) handleDatasetGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"dataset": s.Engine.Runner.DatasetSummary()})
}

// handleDatasetPut imports a CSV, NDJSON or JSON file sent as the request body. It is kept in
// memory only, for replays that choose "use my own file".
func (s *Server) handleDatasetPut(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, lab.MaxDatasetBytes)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "file_too_large", fmt.Sprintf("the file is larger than %d MB", lab.MaxDatasetBytes>>20))
			return
		}
		writeErr(w, http.StatusBadRequest, "invalid_dataset", "could not read the file: "+err.Error())
		return
	}
	ds, err := lab.ParseDataset(cleanFileName(r.URL.Query().Get("name")), raw, s.Engine.Limits(), time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_dataset", err.Error())
		return
	}
	s.Engine.Runner.SetDataset(ds)
	s.Log.Info("file imported", "rows", ds.Summary.Rows, "format", ds.Summary.Format)
	writeJSON(w, http.StatusOK, map[string]any{"dataset": ds.Summary})
}

func (s *Server) handleDatasetDelete(w http.ResponseWriter, _ *http.Request) {
	s.Engine.Runner.SetDataset(nil)
	writeJSON(w, http.StatusOK, map[string]any{"dataset": nil})
}

// cleanFileName keeps only a short, printable base name for display.
func cleanFileName(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "" {
		return "imported file"
	}
	if r := []rune(name); len(r) > 80 {
		name = string(r[:80])
	}
	return name
}

func (s *Server) handleSettingsGet(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"settings": s.Engine.Settings(), "defaults": DefaultSettings()})
}

func (s *Server) applySettings(w http.ResponseWriter, ns Settings) {
	rebuilt, err := s.Engine.Apply(ns)
	switch {
	case errors.Is(err, ErrReplayRunning):
		writeErr(w, http.StatusConflict, "replay_running", "stop the running replay before changing the queue or workers")
	case err != nil:
		writeErr(w, http.StatusBadRequest, "invalid_settings", err.Error())
	default:
		s.Log.Info("settings applied", "engine_rebuilt", rebuilt)
		writeJSON(w, http.StatusOK, map[string]any{"settings": s.Engine.Settings(), "engine_rebuilt": rebuilt})
	}
}

func (s *Server) handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	ns := s.Engine.Settings() // omitted fields keep their current value
	if !decodeStrict(w, r, &ns) {
		return
	}
	s.applySettings(w, ns)
}

func (s *Server) handleSettingsReset(w http.ResponseWriter, _ *http.Request) {
	s.applySettings(w, DefaultSettings())
}

func parseTime(v string, def time.Time) (time.Time, error) {
	if v == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q is not an RFC 3339 time", v)
	}
	return t.UTC(), nil
}

// dataQuery builds a store query. Unlike the public /api/v1 endpoints it does not cap the
// time range: the panel browses everything the local database holds.
func dataQuery(r *http.Request, defLimit, maxLimit int) (store.Query, string, error) {
	p := r.URL.Query()
	kind := p.Get("kind")
	if kind == "" {
		kind = "events"
	}
	if kind != "events" && kind != "alerts" {
		return store.Query{}, "", errors.New("kind must be events or alerts")
	}
	from, err := parseTime(p.Get("from"), time.Unix(0, 0).UTC())
	if err != nil {
		return store.Query{}, "", fmt.Errorf("from: %w", err)
	}
	to, err := parseTime(p.Get("to"), time.Now().UTC().Add(366*24*time.Hour))
	if err != nil {
		return store.Query{}, "", fmt.Errorf("to: %w", err)
	}
	if !to.After(from) {
		return store.Query{}, "", errors.New("to must be after from")
	}
	q := store.Query{DeviceID: p.Get("device_id"), From: from, To: to, Limit: defLimit}
	if q.DeviceID != "" && !event.ValidID(q.DeviceID) {
		return store.Query{}, "", errors.New("device_id is not a valid id")
	}
	if v := p.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return store.Query{}, "", fmt.Errorf("limit must be 1-%d", maxLimit)
		}
		q.Limit = n
	}
	if c := p.Get("cursor"); c != "" {
		if q.After, err = store.DecodeCursor(c); err != nil {
			return store.Query{}, "", err
		}
	}
	return q, kind, nil
}

func (s *Server) handleData(w http.ResponseWriter, r *http.Request) {
	q, kind, err := dataQuery(r, 100, 1000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	db := s.Engine.Store()
	var next *store.Page
	resp := map[string]any{"kind": kind}
	if kind == "events" {
		var evs []event.Event
		if evs, next, err = db.QueryEvents(r.Context(), q); evs == nil {
			evs = []event.Event{}
		}
		resp["items"] = evs
	} else {
		var al []alert.Alert
		if al, next, err = db.QueryAlerts(r.Context(), q); al == nil {
			al = []alert.Alert{}
		}
		resp["items"] = al
	}
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", err.Error())
		return
	}
	if next != nil {
		resp["next_cursor"] = store.EncodeCursor(*next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleDevices(w http.ResponseWriter, r *http.Request) {
	d, err := s.Engine.Store().Devices(r.Context())
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "storage_unavailable", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": d})
}

// handleExport streams every matching row as CSV, NDJSON or one JSON array, page by page, so
// memory stays flat however large the database is.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	q, kind, err := dataQuery(r, 1000, 1000)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	q.Limit, q.After = 1000, nil
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "csv"
	}
	var ext, ctype string
	switch format {
	case "csv":
		ext, ctype = "csv", "text/csv; charset=utf-8"
	case "ndjson":
		ext, ctype = "ndjson", "application/x-ndjson"
	case "json":
		ext, ctype = "json", "application/json"
	default:
		writeErr(w, http.StatusBadRequest, "invalid_parameter", "format must be csv, ndjson or json")
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="signal-lab-%s-%s.%s"`, kind, time.Now().UTC().Format("20060102-150405"), ext))

	rows := func(yield func(csvRow []string, obj any) error) error {
		db := s.Engine.Store()
		for {
			var next *store.Page
			if kind == "events" {
				evs, n, err := db.QueryEvents(r.Context(), q)
				if err != nil {
					return err
				}
				next = n
				for _, e := range evs {
					if err := yield(eventRow(e), e); err != nil {
						return err
					}
				}
			} else {
				al, n, err := db.QueryAlerts(r.Context(), q)
				if err != nil {
					return err
				}
				next = n
				for _, a := range al {
					if err := yield(alertRow(a), a); err != nil {
						return err
					}
				}
			}
			if next == nil {
				return nil
			}
			q.After = next
		}
	}
	var werr error
	switch format {
	case "csv":
		cw := csv.NewWriter(w)
		header := []string{"event_id", "device_id", "site_id", "event_time", "received_at", "sequence", "temperature_c", "vibration_mm_s"}
		if kind == "alerts" {
			header = []string{"alert_id", "device_id", "event_id", "rule", "threshold", "observed", "event_time", "created_at"}
		}
		_ = cw.Write(header)
		werr = rows(func(row []string, _ any) error { return cw.Write(row) })
		cw.Flush()
	case "ndjson":
		enc := json.NewEncoder(w)
		werr = rows(func(_ []string, obj any) error { return enc.Encode(obj) })
	case "json":
		_, _ = io.WriteString(w, "[")
		first := true
		werr = rows(func(_ []string, obj any) error {
			if !first {
				_, _ = io.WriteString(w, ",")
			}
			first = false
			b, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			_, err = w.Write(b)
			return err
		})
		_, _ = io.WriteString(w, "]\n")
	}
	if werr != nil && !errors.Is(werr, context.Canceled) {
		s.Log.Error("export failed part-way", "kind", kind, "format", format, "error", werr)
	}
}

func eventRow(e event.Event) []string {
	seq := ""
	if e.Sequence != nil {
		seq = strconv.FormatInt(*e.Sequence, 10)
	}
	return []string{e.EventID, e.DeviceID, e.SiteID, e.EventTime.Format(time.RFC3339Nano), e.ReceivedAt.Format(time.RFC3339Nano), seq,
		strconv.FormatFloat(e.TemperatureC, 'f', -1, 64), strconv.FormatFloat(e.VibrationMMS, 'f', -1, 64)}
}

func alertRow(a alert.Alert) []string {
	return []string{a.ID, a.DeviceID, a.EventID, string(a.Rule), strconv.FormatFloat(a.Threshold, 'f', -1, 64),
		strconv.FormatFloat(a.Observed, 'f', -1, 64), a.EventTime.Format(time.RFC3339Nano), a.CreatedAt.Format(time.RFC3339Nano)}
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Confirm string `json:"confirm"`
	}
	if !decodeStrict(w, r, &body) {
		return
	}
	if body.Confirm != "DELETE" {
		writeErr(w, http.StatusBadRequest, "confirmation_required", `send {"confirm":"DELETE"} to delete all events and alerts`)
		return
	}
	var events, alerts int64
	err := s.Engine.Quiesce(func(st *sqlitestore.Store) error {
		var err error
		events, alerts, err = st.Clear(r.Context())
		return err
	})
	switch {
	case errors.Is(err, ErrReplayRunning):
		writeErr(w, http.StatusConflict, "replay_running", "stop the running replay before clearing data")
	case err != nil:
		writeErr(w, http.StatusInternalServerError, "clear_failed", err.Error())
	default:
		s.Log.Info("data cleared", "events", events, "alerts", alerts)
		writeJSON(w, http.StatusOK, map[string]any{"deleted_events": events, "deleted_alerts": alerts})
	}
}

func (s *Server) handleShutdown(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "shutting_down"})
	if s.Shutdown != nil {
		go s.Shutdown()
	}
}

// ListenLoopback opens a listener on a loopback address only; the app never serves the network.
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("refusing to listen on %q: the app only serves loopback addresses (127.0.0.1, ::1, localhost)", host)
	}
	ln, err := net.Listen("tcp", addr)
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, fmt.Errorf("port in use: %s is already used by another program (close it, or let Signal Lab pick a free port by using port 0): %w", addr, err)
	}
	if err != nil {
		return nil, fmt.Errorf("cannot listen on %s: %w", addr, err)
	}
	return ln, nil
}
