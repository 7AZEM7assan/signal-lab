package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"signallab/internal/alert"
	"signallab/internal/config"
	"signallab/internal/event"
	"signallab/internal/hub"
	"signallab/internal/metrics"
	"signallab/internal/pipeline"
	"signallab/internal/store"
)

var fixedNow = time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)

type fakeIngest struct {
	mu  sync.Mutex
	got [][]event.Event
	err error
}

func (f *fakeIngest) Enqueue(_ string, evs []event.Event) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.got = append(f.got, evs)
	return nil
}
func (f *fakeIngest) Depth() int    { return 3 }
func (f *fakeIngest) Capacity() int { return 10 }

type fakeDB struct {
	pingErr  error
	queryErr error
	events   []event.Event
	next     *store.Page
	lastQ    store.Query
}

func (f *fakeDB) Ping(context.Context) error { return f.pingErr }
func (f *fakeDB) QueryEvents(_ context.Context, q store.Query) ([]event.Event, *store.Page, error) {
	f.lastQ = q
	return f.events, f.next, f.queryErr
}
func (f *fakeDB) QueryAlerts(_ context.Context, q store.Query) ([]alert.Alert, *store.Page, error) {
	f.lastQ = q
	return nil, nil, f.queryErr
}

type harness struct {
	srv    *Server
	ts     *httptest.Server
	ingest *fakeIngest
	db     *fakeDB
	hub    *hub.Hub
	m      *metrics.Metrics
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	env["SIGNALLAB_DATABASE_URL"] = "postgres://unused"
	cfg, err := config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	m := metrics.New()
	h := &harness{ingest: &fakeIngest{}, db: &fakeDB{}, m: m, hub: hub.New(cfg.WSClientBuffer, cfg.WSMaxClients, m)}
	h.srv = New(Deps{Cfg: cfg, Ingest: h.ingest, DB: h.db, Hub: h.hub, Metrics: m,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return fixedNow }})
	h.ts = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.ts.Close)
	return h
}

func rec(id string, seq int, temp float64) string {
	return `{"schema_version":1,"event_id":"` + id + `","device_id":"press-01","event_time":"2025-01-15T08:00:00Z","sequence":` +
		itoa(seq) + `,"temperature_c":` + ftoa(temp) + `,"vibration_mm_s":2.5}`
}

func itoa(n int) string     { b, _ := json.Marshal(n); return string(b) }
func ftoa(f float64) string { b, _ := json.Marshal(f); return string(b) }

func post(t *testing.T, h *harness, body string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Post(h.ts.URL+"/api/v1/events", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestIngestAcceptsValidAndReportsInvalid(t *testing.T) {
	h := newHarness(t, map[string]string{})
	body := `{"events":[` + rec("a", 1, 60) + `,` + rec("b", 2, 999) + `,` + rec("a", 3, 61) + `,` + rec("c", 4, 62) + `]}`
	resp, out := post(t, h, body)
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", resp.StatusCode)
	}
	if out["received"] != 4.0 || out["accepted"] != 2.0 || out["rejected"] != 2.0 {
		t.Fatalf("unexpected counts: %v", out)
	}
	counts := out["rejection_counts"].(map[string]any)
	if counts["out_of_range"] != 1.0 || counts["duplicate_in_batch"] != 1.0 {
		t.Fatalf("rejection counts = %v", counts)
	}
	rej := out["rejections"].([]any)
	if first := rej[0].(map[string]any); first["index"] != 1.0 || first["reason"] != "out_of_range" {
		t.Fatalf("first rejection = %v", first)
	}
	if len(h.ingest.got) != 1 || len(h.ingest.got[0]) != 2 {
		t.Fatalf("expected one enqueue of 2 events, got %v", h.ingest.got)
	}
	if got := h.ingest.got[0][0]; !got.ReceivedAt.Equal(fixedNow) {
		t.Fatalf("received_at must be server time, got %v", got.ReceivedAt)
	}
	if resp.Header.Get("X-Request-ID") == "" || out["batch_id"] != resp.Header.Get("X-Request-ID") {
		t.Fatalf("batch_id should equal the request id: %v vs %q", out["batch_id"], resp.Header.Get("X-Request-ID"))
	}
}

func TestIngestRejectsBadEnvelopes(t *testing.T) {
	h := newHarness(t, map[string]string{"SIGNALLAB_MAX_BATCH_EVENTS": "2", "SIGNALLAB_QUEUE_CAPACITY": "10", "SIGNALLAB_MAX_BODY_BYTES": "2000"})
	for _, tc := range []struct {
		name, body string
		status     int
		code       string
	}{
		{"not json", `nope`, 400, "invalid_json"},
		{"wrong shape", `[1,2]`, 400, "invalid_json"},
		{"trailing data", `{"events":[` + rec("a", 1, 1) + `]} {}`, 400, "invalid_json"},
		{"empty batch", `{"events":[]}`, 400, "empty_batch"},
		{"missing events", `{}`, 400, "empty_batch"},
		{"too many events", `{"events":[` + rec("a", 1, 1) + `,` + rec("b", 2, 1) + `,` + rec("c", 3, 1) + `]}`, 413, "batch_too_large"},
		{"body too large", `{"events":[` + strings.Repeat(rec("a", 1, 1)+",", 30) + rec("z", 2, 1) + `]}`, 413, "payload_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, out := post(t, h, tc.body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d (%v)", resp.StatusCode, tc.status, out)
			}
			if code := out["error"].(map[string]any)["code"]; code != tc.code {
				t.Fatalf("code = %v, want %s", code, tc.code)
			}
		})
	}
	if len(h.ingest.got) != 0 {
		t.Fatal("nothing should be enqueued for rejected envelopes")
	}
}

func TestIngestOverloadReturns429WithRetryAfter(t *testing.T) {
	h := newHarness(t, map[string]string{})
	h.ingest.err = pipeline.ErrQueueFull
	resp, out := post(t, h, `{"events":[`+rec("a", 1, 60)+`]}`)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q", resp.Header.Get("Retry-After"))
	}
	if out["accepted"] != 0.0 || out["error"].(map[string]any)["code"] != "queue_full" {
		t.Fatalf("unexpected body: %v", out)
	}
	if out["queue"].(map[string]any)["capacity"] != 10.0 {
		t.Fatalf("queue state missing: %v", out)
	}
}

func TestIngestWhileDrainingReturns503(t *testing.T) {
	h := newHarness(t, map[string]string{})
	h.ingest.err = pipeline.ErrClosed
	resp, _ := post(t, h, `{"events":[`+rec("a", 1, 60)+`]}`)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("status = %d, Retry-After = %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestIngestUnexpectedErrorIs500(t *testing.T) {
	h := newHarness(t, map[string]string{})
	h.ingest.err = errors.New("boom")
	if resp, _ := post(t, h, `{"events":[`+rec("a", 1, 60)+`]}`); resp.StatusCode != 500 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
}

func get(t *testing.T, h *harness, path string) (*http.Response, map[string]any) {
	t.Helper()
	resp, err := http.Get(h.ts.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp, out
}

func TestQueryParameterValidation(t *testing.T) {
	h := newHarness(t, map[string]string{"SIGNALLAB_QUERY_MAX_LIMIT": "50", "SIGNALLAB_QUERY_DEFAULT_LIMIT": "10", "SIGNALLAB_QUERY_MAX_RANGE": "1h"})
	base := "from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z"
	for name, qs := range map[string]string{
		"missing from":      "to=2025-01-15T08:30:00Z",
		"missing to":        "from=2025-01-15T08:00:00Z",
		"bad from":          "from=yesterday&to=2025-01-15T08:30:00Z",
		"from after to":     "from=2025-01-15T09:00:00Z&to=2025-01-15T08:00:00Z",
		"range too wide":    "from=2025-01-15T00:00:00Z&to=2025-01-15T08:00:00Z",
		"limit zero":        base + "&limit=0",
		"limit too large":   base + "&limit=51",
		"limit not numeric": base + "&limit=abc",
		"bad device":        base + "&device_id=a/b",
		"bad cursor":        base + "&cursor=!!!",
	} {
		t.Run(name, func(t *testing.T) {
			for _, path := range []string{"/api/v1/events?", "/api/v1/alerts?"} {
				if resp, _ := get(t, h, path+qs); resp.StatusCode != http.StatusBadRequest {
					t.Fatalf("%s: status = %d, want 400", path, resp.StatusCode)
				}
			}
		})
	}
}

func TestQueryEventsPassesParametersAndCursor(t *testing.T) {
	h := newHarness(t, map[string]string{})
	h.db.events = []event.Event{{EventID: "a", DeviceID: "press-01", EventTime: fixedNow}}
	h.db.next = &store.Page{Time: fixedNow, ID: "a"}
	resp, out := get(t, h, "/api/v1/events?device_id=press-01&limit=5&from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z")
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %v", resp.StatusCode, out)
	}
	if h.db.lastQ.DeviceID != "press-01" || h.db.lastQ.Limit != 5 {
		t.Fatalf("query not passed through: %+v", h.db.lastQ)
	}
	cursor, _ := out["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor missing")
	}
	// Follow the cursor: the server must decode it into the keyset position.
	if _, _ = get(t, h, "/api/v1/events?from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z&cursor="+cursor); h.db.lastQ.After == nil || h.db.lastQ.After.ID != "a" {
		t.Fatalf("cursor not decoded: %+v", h.db.lastQ.After)
	}
}

func TestQueryReturnsEmptyArrayNotNull(t *testing.T) {
	h := newHarness(t, map[string]string{})
	_, out := get(t, h, "/api/v1/events?from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z")
	if evs, ok := out["events"].([]any); !ok || len(evs) != 0 {
		t.Fatalf("events should be an empty array, got %v", out["events"])
	}
	_, out = get(t, h, "/api/v1/alerts?from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z")
	if al, ok := out["alerts"].([]any); !ok || len(al) != 0 {
		t.Fatalf("alerts should be an empty array, got %v", out["alerts"])
	}
}

func TestQueryDatabaseFailureIs503(t *testing.T) {
	h := newHarness(t, map[string]string{})
	h.db.queryErr = errors.New("connection refused")
	resp, out := get(t, h, "/api/v1/events?from=2025-01-15T08:00:00Z&to=2025-01-15T08:30:00Z")
	if resp.StatusCode != 503 || out["error"].(map[string]any)["code"] != "database_unavailable" {
		t.Fatalf("status = %d body = %v", resp.StatusCode, out)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t, map[string]string{})
	if resp, _ := get(t, h, "/healthz"); resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	if resp, _ := get(t, h, "/readyz"); resp.StatusCode != 200 {
		t.Fatalf("readyz = %d", resp.StatusCode)
	}
	h.db.pingErr = errors.New("down")
	resp, out := get(t, h, "/readyz")
	if resp.StatusCode != 503 || out["checks"].(map[string]any)["database"] != "unavailable" {
		t.Fatalf("readyz with db down = %d %v", resp.StatusCode, out)
	}
	if resp, _ := get(t, h, "/healthz"); resp.StatusCode != 200 { // liveness must not depend on the database
		t.Fatalf("healthz must stay 200 when the database is down, got %d", resp.StatusCode)
	}
	h.db.pingErr = nil
	h.srv.SetDraining()
	if resp, _ := get(t, h, "/readyz"); resp.StatusCode != 503 {
		t.Fatalf("readyz while draining = %d", resp.StatusCode)
	}
}

func TestMetricsEndpointExposesBoundedSeries(t *testing.T) {
	h := newHarness(t, map[string]string{})
	post(t, h, `{"events":[`+rec("a", 1, 999)+`]}`)
	resp, err := http.Get(h.ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	text := string(b)
	for _, want := range []string{
		`signallab_ingest_events_total{outcome="rejected_invalid"} 1`,
		`signallab_validation_failures_total{reason="out_of_range"} 1`,
		`signallab_http_requests_total{code="202",route="POST /api/v1/events"} 1`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
	if strings.Contains(text, "press-01") {
		t.Error("device IDs must never appear in metrics")
	}
}

func TestRequestIDIsHonouredWhenSane(t *testing.T) {
	h := newHarness(t, map[string]string{})
	for in, wantKept := range map[string]bool{"trace-123": true, "bad id with spaces": false} {
		req, _ := http.NewRequest("GET", h.ts.URL+"/healthz", nil)
		req.Header.Set("X-Request-ID", in)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("X-Request-ID"); (got == in) != wantKept {
			t.Errorf("input %q -> %q", in, got)
		}
	}
}

func wsURL(h *harness, path string) string { return "ws" + strings.TrimPrefix(h.ts.URL, "http") + path }

func readWS(t *testing.T, c *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWebSocketReceivesHelloThenBroadcasts(t *testing.T) {
	h := newHarness(t, map[string]string{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, wsURL(h, "/ws"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()

	if msg := readWS(t, c); msg["type"] != "hello" {
		t.Fatalf("first frame should be hello, got %v", msg)
	}
	h.hub.Broadcast("press-01", []byte(`{"type":"event","data":{"event_id":"x"}}`))
	if msg := readWS(t, c); msg["type"] != "event" {
		t.Fatalf("expected event frame, got %v", msg)
	}
}

func TestWebSocketDeviceFilterAndValidation(t *testing.T) {
	h := newHarness(t, map[string]string{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, resp, err := websocket.Dial(ctx, wsURL(h, "/ws?device_id=a/b"), nil); err == nil || resp == nil || resp.StatusCode != 400 {
		t.Fatalf("invalid device_id should fail the handshake with 400, got err=%v resp=%v", err, resp)
	}

	c, _, err := websocket.Dial(ctx, wsURL(h, "/ws?device_id=pump-02"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	readWS(t, c) // hello
	h.hub.Broadcast("press-01", []byte(`{"type":"event","data":{"n":1}}`))
	h.hub.Broadcast("pump-02", []byte(`{"type":"event","data":{"n":2}}`))
	if msg := readWS(t, c); msg["data"].(map[string]any)["n"] != 2.0 {
		t.Fatalf("filtered client should only see pump-02, got %v", msg)
	}
}

func TestWebSocketRefusedWhenHubFull(t *testing.T) {
	h := newHarness(t, map[string]string{"SIGNALLAB_WS_MAX_CLIENTS": "1"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c1, _, err := websocket.Dial(ctx, wsURL(h, "/ws"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.CloseNow()
	readWS(t, c1)
	if _, resp, err := websocket.Dial(ctx, wsURL(h, "/ws"), nil); err == nil || resp == nil || resp.StatusCode != 503 {
		t.Fatalf("second client should be refused with 503, got err=%v resp=%v", err, resp)
	}
}
