package api

import (
	"fmt"
	"net/http"
	"strconv"
	"time"

	"signallab/internal/alert"
	"signallab/internal/event"
	"signallab/internal/store"
)

// parseQuery validates the shared query parameters of the two list endpoints:
// device_id (optional), from and to (required, RFC 3339, half-open [from, to)),
// limit (1..QueryMaxLimit) and cursor (opaque, from a previous response).
func (s *Server) parseQuery(r *http.Request) (store.Query, error) {
	v := r.URL.Query()
	q := store.Query{Limit: s.Cfg.QueryDefaultLimit}

	if d := v.Get("device_id"); d != "" {
		if !event.ValidID(d) {
			return q, fmt.Errorf("device_id is not a valid identifier")
		}
		q.DeviceID = d
	}
	var err error
	if q.From, err = parseTimeParam(v.Get("from"), "from"); err != nil {
		return q, err
	}
	if q.To, err = parseTimeParam(v.Get("to"), "to"); err != nil {
		return q, err
	}
	switch {
	case !q.From.Before(q.To):
		return q, fmt.Errorf("from must be before to")
	case q.To.Sub(q.From) > s.Cfg.QueryMaxRange:
		return q, fmt.Errorf("time range exceeds the maximum of %s", s.Cfg.QueryMaxRange)
	}
	if l := v.Get("limit"); l != "" {
		n, err := strconv.Atoi(l)
		if err != nil || n < 1 || n > s.Cfg.QueryMaxLimit {
			return q, fmt.Errorf("limit must be an integer between 1 and %d", s.Cfg.QueryMaxLimit)
		}
		q.Limit = n
	}
	if c := v.Get("cursor"); c != "" {
		if q.After, err = store.DecodeCursor(c); err != nil {
			return q, err
		}
	}
	return q, nil
}

func parseTimeParam(raw, name string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, fmt.Errorf("%s is required (RFC 3339 timestamp)", name)
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be an RFC 3339 timestamp", name)
	}
	return t.UTC(), nil
}

type eventsResponse struct {
	Events     []event.Event `json:"events"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type alertsResponse struct {
	Alerts     []alert.Alert `json:"alerts"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

func (s *Server) handleQueryEvents(w http.ResponseWriter, r *http.Request) {
	q, err := s.parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	evs, next, err := s.DB.QueryEvents(r.Context(), q)
	if err != nil {
		s.Log.Error("query events failed", "request_id", RequestID(r.Context()), "error", err)
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database query failed")
		return
	}
	resp := eventsResponse{Events: evs}
	if resp.Events == nil {
		resp.Events = []event.Event{}
	}
	if next != nil {
		resp.NextCursor = store.EncodeCursor(*next)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleQueryAlerts(w http.ResponseWriter, r *http.Request) {
	q, err := s.parseQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_parameter", err.Error())
		return
	}
	alerts, next, err := s.DB.QueryAlerts(r.Context(), q)
	if err != nil {
		s.Log.Error("query alerts failed", "request_id", RequestID(r.Context()), "error", err)
		writeError(w, http.StatusServiceUnavailable, "database_unavailable", "database query failed")
		return
	}
	resp := alertsResponse{Alerts: alerts}
	if resp.Alerts == nil {
		resp.Alerts = []alert.Alert{}
	}
	if next != nil {
		resp.NextCursor = store.EncodeCursor(*next)
	}
	writeJSON(w, http.StatusOK, resp)
}
