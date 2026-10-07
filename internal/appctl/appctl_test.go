package appctl

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

type app struct {
	t    *testing.T
	eng  *Engine
	ts   *httptest.Server
	dir  string
	base string
}

func newApp(t *testing.T, dir string) *app {
	t.Helper()
	if dir == "" {
		dir = t.TempDir()
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	eng, err := NewEngine(dir, log)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(nil)
	port := ts.Listener.Addr().(*net.TCPAddr).Port
	srv := &Server{Engine: eng, Token: testToken, Port: port, DataDir: dir, Version: "test", Started: time.Now(), Log: log,
		UI: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, "panel") })}
	ts.Config.Handler = srv.Handler()
	ts.Start()
	a := &app{t: t, eng: eng, ts: ts, dir: dir, base: "http://127.0.0.1:" + strconv.Itoa(port)}
	t.Cleanup(func() { a.close() })
	return a
}

func (a *app) close() {
	if a.ts != nil {
		a.ts.Close()
		a.eng.Close(context.Background())
		a.ts = nil
	}
}

func (a *app) do(method, path string, body any) (int, []byte) {
	a.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.base+path, rd)
	req.Header.Set(TokenHeader, testToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (a *app) json(method, path string, body any, into any) int {
	a.t.Helper()
	code, raw := a.do(method, path, body)
	if into != nil {
		if err := json.Unmarshal(raw, into); err != nil {
			a.t.Fatalf("%s %s: bad JSON %q: %v", method, path, raw, err)
		}
	}
	return code
}

func (a *app) waitReplay() map[string]any {
	a.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var s map[string]any
		a.json("GET", "/app/api/replay", nil, &s)
		if s["state"] != "running" {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.t.Fatal("replay did not finish")
	return nil
}

// settle waits until nothing more is being stored: the queue is empty and the stored count has
// stopped changing. (An empty queue alone is not enough: workers take events off the queue
// before they commit them.)
func (a *app) settle() {
	a.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	last, stable := -1.0, 0
	for time.Now().Before(deadline) {
		e, _ := a.stats()
		if a.eng.Info().QueueDepth == 0 && e == last {
			if stable++; stable >= 6 {
				return
			}
		} else {
			stable = 0
		}
		last = e
		time.Sleep(25 * time.Millisecond)
	}
	a.t.Fatal("storage did not settle")
}

func (a *app) stats() (events, alerts float64) {
	a.t.Helper()
	var st struct {
		Storage struct{ Events, Alerts float64 } `json:"storage"`
	}
	a.json("GET", "/app/api/state", nil, &st)
	return st.Storage.Events, st.Storage.Alerts
}

func TestSecurityHostTokenAndWrites(t *testing.T) {
	a := newApp(t, "")
	get := func(host, path string, hdr map[string]string) int {
		req, _ := http.NewRequest("GET", a.base+path, nil)
		if host != "" {
			req.Host = host
		}
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	port := strings.TrimPrefix(a.base, "http://127.0.0.1:")
	if c := get("evil.example:"+port, "/readyz", nil); c != 403 {
		t.Errorf("foreign Host header: %d, want 403 (DNS rebinding guard)", c)
	}
	if c := get("", "/readyz", nil); c != 200 {
		t.Errorf("readyz needs no token: %d", c)
	}
	if c := get("", "/app/api/state", nil); c != 401 {
		t.Errorf("no token: %d, want 401", c)
	}
	if c := get("", "/api/v1/events?from=2025-01-01T00:00:00Z&to=2025-01-02T00:00:00Z", nil); c != 401 {
		t.Errorf("query API without token: %d, want 401", c)
	}
	if c := get("", "/metrics", nil); c != 401 {
		t.Errorf("metrics without token: %d, want 401", c)
	}
	if c := get("", "/app/api/state", map[string]string{TokenHeader: "wrong"}); c != 401 {
		t.Errorf("wrong token: %d", c)
	}
	if c := get("", "/app/api/state", map[string]string{TokenHeader: testToken}); c != 200 {
		t.Errorf("right token: %d", c)
	}
	if c := get("", "/?token=nope", nil); c != 401 {
		t.Errorf("wrong link token: %d", c)
	}
	// The link sets a strict, http-only cookie and redirects without the token in the URL.
	req, _ := http.NewRequest("GET", a.base+"/?token="+testToken, nil)
	resp, _ := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == CookieName {
			cookie = c
		}
	}
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/" || cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("token link: status %d location %q cookie %+v", resp.StatusCode, resp.Header.Get("Location"), cookie)
	}

	// Cookie-authenticated writes must look like they came from our own page.
	post := func(hdr map[string]string) int {
		req, _ := http.NewRequest("POST", a.base+"/app/api/replay/stop", nil)
		req.AddCookie(cookie)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post(nil); c != 403 {
		t.Errorf("cookie write without X-Requested-With: %d, want 403", c)
	}
	if c := post(map[string]string{"X-Requested-With": "signallab", "Origin": "https://evil.example"}); c != 403 {
		t.Errorf("cross-origin write: %d, want 403", c)
	}
	if c := post(map[string]string{"X-Requested-With": "signallab", "Origin": a.base}); c != 200 {
		t.Errorf("same-origin write: %d, want 200", c)
	}
}

func replayBody(extra map[string]any) map[string]any {
	b := map[string]any{"seed": 7, "devices": 4, "duration_s": 60, "interval_s": 1, "rate_per_s": 0, "batch_size": 25,
		"anomaly_rate": 0.1, "concurrency": 2}
	for k, v := range extra {
		b[k] = v
	}
	return b
}

func TestReplayStoresBrowsesExportsAndIsIdempotent(t *testing.T) {
	a := newApp(t, "")
	body := replayBody(map[string]any{"start": "2025-01-15T08:00:00Z", "sequence_start": 5000,
		"duplicate_rate": 0.1, "malformed_rate": 0.05, "rate_per_s": 300}) // ~0.9 s, so the second start below is certain to find it running
	var snap map[string]any
	if c := a.json("POST", "/app/api/replay/start", body, &snap); c != 202 {
		t.Fatalf("start: %d %v", c, snap)
	}
	if c := a.json("POST", "/app/api/replay/start", body, nil); c != 409 {
		t.Fatalf("second start while running: %d, want 409 (unless the run already finished)", c)
	}
	done := a.waitReplay()
	if done["state"] != "done" {
		t.Fatalf("replay ended as %v", done)
	}
	a.settle()
	events, alerts := a.stats()
	injected := done["injected"].(map[string]any)
	wantEvents := 4*60 - int(injected["malformed"].(float64))
	if int(events) != wantEvents {
		t.Fatalf("stored %v events, want %d (240 generated minus %v malformed; duplicates collapse to one row)", events, wantEvents, injected["malformed"])
	}
	if alerts == 0 {
		t.Fatal("anomaly rate 0.1 should have raised alerts")
	}
	if done["rejected"].(float64) == 0 {
		t.Fatal("malformed and in-batch duplicate records should be rejected")
	}

	// Resending the identical dataset stores nothing new: idempotency end to end.
	if c := a.json("POST", "/app/api/replay/start", body, nil); c != 202 {
		t.Fatalf("restart: %d", c)
	}
	a.waitReplay()
	a.settle()
	if e2, a2 := a.stats(); e2 != events || a2 != alerts {
		t.Fatalf("identical replay changed storage: events %v -> %v, alerts %v -> %v", events, e2, alerts, a2)
	}

	// Browsing with filter and pagination.
	var page struct {
		Items      []map[string]any `json:"items"`
		NextCursor string           `json:"next_cursor"`
	}
	a.json("GET", "/app/api/data?kind=events&device_id=press-01&limit=10", nil, &page)
	if len(page.Items) != 10 || page.NextCursor == "" {
		t.Fatalf("first page: %d items, cursor %q", len(page.Items), page.NextCursor)
	}
	for _, it := range page.Items {
		if it["device_id"] != "press-01" {
			t.Fatalf("device filter leaked %v", it)
		}
	}
	if c, _ := a.do("GET", "/app/api/data?kind=nope", nil); c != 400 {
		t.Errorf("bad kind: %d", c)
	}
	if c, _ := a.do("GET", "/app/api/data?from=2025-02-01T00:00:00Z&to=2025-01-01T00:00:00Z", nil); c != 400 {
		t.Errorf("reversed range: %d", c)
	}
	var devs struct {
		Devices []struct {
			DeviceID string `json:"device_id"`
			Events   int    `json:"events"`
		} `json:"devices"`
	}
	a.json("GET", "/app/api/devices", nil, &devs)
	if len(devs.Devices) != 4 || devs.Devices[0].DeviceID != "conveyor-05" && devs.Devices[0].DeviceID != "lathe-04" {
		t.Fatalf("devices: %+v", devs.Devices)
	}

	// Exports agree with the database in every format.
	code, csvBody := a.do("GET", "/app/api/export?kind=events&format=csv", nil)
	rows, err := csv.NewReader(bytes.NewReader(csvBody)).ReadAll()
	if code != 200 || err != nil || len(rows) != int(events)+1 || rows[0][0] != "event_id" {
		t.Fatalf("csv export: code %d err %v rows %d want %d", code, err, len(rows), int(events)+1)
	}
	_, nd := a.do("GET", "/app/api/export?kind=alerts&format=ndjson", nil)
	if n := len(strings.Split(strings.TrimSpace(string(nd)), "\n")); n != int(alerts) {
		t.Fatalf("ndjson alerts: %d lines, want %v", n, alerts)
	}
	var arr []map[string]any
	_, js := a.do("GET", "/app/api/export?kind=events&format=json&device_id=pump-02", nil)
	if err := json.Unmarshal(js, &arr); err != nil || len(arr) == 0 {
		t.Fatalf("json export: %v (%d rows)", err, len(arr))
	}
	if c, _ := a.do("GET", "/app/api/export?format=xml", nil); c != 400 {
		t.Errorf("bad format: %d", c)
	}
}

func TestSettingsApplyLiveAndPersist(t *testing.T) {
	dir := t.TempDir()
	a := newApp(t, dir)
	var out struct {
		Settings      Settings
		EngineRebuilt bool `json:"engine_rebuilt"`
	}
	// Raising the threshold takes effect immediately, without rebuilding the engine.
	if c := a.json("PUT", "/app/api/settings", map[string]any{"temp_alert_c": 200}, &out); c != 200 || out.EngineRebuilt || out.Settings.TempAlertC != 200 {
		t.Fatalf("threshold change: %d %+v", c, out)
	}
	a.json("POST", "/app/api/replay/start", replayBody(map[string]any{"anomaly_rate": 0.3, "devices": 2}), nil)
	a.waitReplay()
	a.settle()
	_, withHigh := a.stats()
	var al struct {
		Items []struct{ Rule string } `json:"items"`
	}
	a.json("GET", "/app/api/data?kind=alerts&limit=1000", nil, &al)
	for _, it := range al.Items {
		if it.Rule == "temperature_high" {
			t.Fatal("temperature alerts fired although the threshold was raised to 200 C")
		}
	}
	if withHigh == 0 {
		t.Fatal("expected vibration alerts to still fire")
	}

	// Changing the queue rebuilds the engine, keeps the data, and survives a restart.
	if c := a.json("PUT", "/app/api/settings", map[string]any{"queue_capacity": 300, "workers": 2}, &out); c != 200 || !out.EngineRebuilt {
		t.Fatalf("queue change: %d %+v", c, out)
	}
	if info := a.eng.Info(); info.QueueCapacity != 300 || info.EngineStarts != 1 {
		t.Fatalf("engine info after rebuild: %+v", info)
	}
	before, _ := a.stats()
	if before == 0 {
		t.Fatal("data was lost when the engine was rebuilt")
	}
	a.close()
	b := newApp(t, dir)
	if s := b.eng.Settings(); s.QueueCapacity != 300 || s.Workers != 2 || s.TempAlertC != 200 {
		t.Fatalf("settings were not restored: %+v", s)
	}
	if after, _ := b.stats(); after != before {
		t.Fatalf("data after restart: %v, want %v", after, before)
	}

	// Invalid values are refused with a reason; unknown fields too.
	for name, body := range map[string]map[string]any{
		"zero workers": {"workers": 0}, "negative delay": {"lab_worker_delay_ms": -1},
		"zero threshold": {"vib_alert_mm_s": 0}, "unknown field": {"nope": 1},
	} {
		if c := b.json("PUT", "/app/api/settings", body, nil); c != 400 {
			t.Errorf("%s: %d, want 400", name, c)
		}
	}
	if c := b.json("POST", "/app/api/settings/reset", nil, &out); c != 200 || out.Settings != DefaultSettings() {
		t.Fatalf("reset: %d %+v", c, out)
	}
}

func TestStructuralSettingsAndClearAreRefusedDuringReplay(t *testing.T) {
	a := newApp(t, "")
	a.json("PUT", "/app/api/settings", map[string]any{"lab_worker_delay_ms": 200, "worker_batch_size": 5, "workers": 1}, nil)
	a.json("POST", "/app/api/replay/start", replayBody(map[string]any{"duration_s": 120, "rate_per_s": 200}), nil)
	if c := a.json("PUT", "/app/api/settings", map[string]any{"queue_capacity": 500}, nil); c != 409 {
		t.Errorf("structural change during a replay: %d, want 409", c)
	}
	if c := a.json("POST", "/app/api/storage/clear", map[string]any{"confirm": "DELETE"}, nil); c != 409 {
		t.Errorf("clear during a replay: %d, want 409", c)
	}
	var snap map[string]any
	a.json("POST", "/app/api/replay/stop", nil, &snap)
	if snap["state"] != "stopped" && snap["state"] != "done" {
		t.Fatalf("after stop: %v", snap["state"])
	}
}

func TestClearRequiresConfirmationAndLeavesAWorkingApp(t *testing.T) {
	a := newApp(t, "")
	a.json("POST", "/app/api/replay/start", replayBody(nil), nil)
	a.waitReplay()
	a.settle()
	if e, _ := a.stats(); e == 0 {
		t.Fatal("setup stored nothing")
	}
	if c := a.json("POST", "/app/api/storage/clear", map[string]any{}, nil); c != 400 {
		t.Errorf("clear without confirmation: %d", c)
	}
	if c := a.json("POST", "/app/api/storage/clear", map[string]any{"confirm": "yes"}, nil); c != 400 {
		t.Errorf("clear with wrong confirmation: %d", c)
	}
	if e, _ := a.stats(); e == 0 {
		t.Fatal("data was deleted without a valid confirmation")
	}
	var out map[string]float64
	if c := a.json("POST", "/app/api/storage/clear", map[string]any{"confirm": "DELETE"}, &out); c != 200 || out["deleted_events"] == 0 {
		t.Fatalf("clear: %d %v", c, out)
	}
	if e, al := a.stats(); e != 0 || al != 0 {
		t.Fatalf("after clear: %v events %v alerts", e, al)
	}
	// Ingest still works afterwards.
	a.json("POST", "/app/api/replay/start", replayBody(nil), nil)
	a.waitReplay()
	a.settle()
	if e, _ := a.stats(); e == 0 {
		t.Fatal("nothing was stored after clearing")
	}
}

func TestInvalidReplayConfigIsRejectedWithReason(t *testing.T) {
	a := newApp(t, "")
	for name, body := range map[string]map[string]any{
		"devices":      {"devices": 0},
		"fault rate":   {"duplicate_rate": 2},
		"batch size":   {"batch_size": 100000},
		"unknown":      {"flux_capacitor": 1},
		"huge dataset": {"devices": 200, "duration_s": 1000000, "interval_s": 1},
	} {
		code, raw := a.do("POST", "/app/api/replay/start", body)
		if code != 400 {
			t.Errorf("%s: %d %s, want 400", name, code, raw)
		}
	}
}

func TestShutdownEndpointTriggersCallback(t *testing.T) {
	a := newApp(t, "")
	called := make(chan struct{}, 1)
	// Rebuild the handler's server with a callback.
	srv := &Server{Engine: a.eng, Token: testToken, Port: a.ts.Listener.Addr().(*net.TCPAddr).Port, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		UI: http.NotFoundHandler(), Shutdown: func() { called <- struct{}{} }}
	a.ts.Config.Handler = srv.Handler()
	if c := a.json("POST", "/app/api/shutdown", nil, nil); c != 202 {
		t.Fatalf("shutdown: %d", c)
	}
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("shutdown callback was not called")
	}
}

func TestListenLoopbackRefusesNetworkAddresses(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "192.168.1.5:0", ":0", "example.com:80"} {
		if ln, err := ListenLoopback(addr); err == nil {
			ln.Close()
			t.Errorf("%s: expected refusal", addr)
		}
	}
	ln, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
}
