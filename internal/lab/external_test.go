package lab

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitDone(t *testing.T, run *Runner) Snapshot {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if s := run.Snapshot(); s.State != StateRunning {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("run did not finish")
	return Snapshot{}
}

// received is what a fake service of the user's own saw.
type received struct {
	mu      sync.Mutex
	bodies  []string
	ctypes  []string
	headers []http.Header
	paths   []string
}

func (r *received) handler(status int, reply string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		raw, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.bodies = append(r.bodies, string(raw))
		r.ctypes = append(r.ctypes, req.Header.Get("Content-Type"))
		r.headers = append(r.headers, req.Header.Clone())
		r.paths = append(r.paths, req.URL.RequestURI())
		r.mu.Unlock()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, reply)
	}
}

func external(url string) Config {
	c := small() // 3 devices x 60 steps = 180 records, unpaced
	c.Devices, c.DurationS, c.IntervalS = 2, 6, 1
	c.BatchSize, c.Concurrency = 5, 1
	c.TargetURL = url
	return c
}

func TestExternalPayloadFormats(t *testing.T) {
	for _, tc := range []struct {
		format, ctype string
		requests      int
	}{
		{"", "application/json", 3}, // default = batch
		{FormatBatch, "application/json", 3},
		{FormatArray, "application/json", 3},
		{FormatNDJSON, "application/x-ndjson", 3},
		{FormatSingle, "application/json", 12},
	} {
		t.Run(tc.format, func(t *testing.T) {
			var got received
			srv := httptest.NewServer(got.handler(http.StatusOK, `{"ok":true}`))
			defer srv.Close()
			c := external(srv.URL + "/ingest?key=SECRETKEY")
			c.PayloadFormat = tc.format
			c.TargetHeaders = map[string]string{"Authorization": "Bearer SECRETTOKEN", "X-Tenant": "acme"}
			run := NewRunner()
			if _, err := run.Start(c, Target{UserAgent: "SignalLab/test"}, 500); err != nil {
				t.Fatal(err)
			}
			snap := waitDone(t, run)
			if snap.State != StateDone || len(got.bodies) != tc.requests {
				t.Fatalf("state %s, %d requests, want %d (%+v)", snap.State, len(got.bodies), tc.requests, snap)
			}
			records := 0
			for i, b := range got.bodies {
				if got.ctypes[i] != tc.ctype {
					t.Fatalf("content type %q, want %q", got.ctypes[i], tc.ctype)
				}
				switch tc.format {
				case FormatArray:
					var arr []map[string]any
					if err := json.Unmarshal([]byte(b), &arr); err != nil {
						t.Fatalf("array body: %v: %s", err, b)
					}
					records += len(arr)
				case FormatNDJSON:
					for _, line := range strings.Split(strings.TrimSuffix(b, "\n"), "\n") {
						var one map[string]any
						if err := json.Unmarshal([]byte(line), &one); err != nil || one["device_id"] == nil {
							t.Fatalf("ndjson line %q: %v", line, err)
						}
						records++
					}
				case FormatSingle:
					var one map[string]any
					if err := json.Unmarshal([]byte(b), &one); err != nil || one["device_id"] == nil {
						t.Fatalf("single body %q: %v", b, err)
					}
					records++
				default:
					var env struct {
						Events []map[string]any `json:"events"`
					}
					if err := json.Unmarshal([]byte(b), &env); err != nil {
						t.Fatalf("batch body: %v", err)
					}
					records += len(env.Events)
				}
			}
			if records != 12 || snap.Accepted != 12 || snap.RecordsDone != 12 || snap.RequestErrors != 0 {
				t.Fatalf("records=%d accepted=%d done=%d errors=%d", records, snap.Accepted, snap.RecordsDone, snap.RequestErrors)
			}
			h := got.headers[0]
			if h.Get("Authorization") != "Bearer SECRETTOKEN" || h.Get("X-Tenant") != "acme" || h.Get("User-Agent") != "SignalLab/test" {
				t.Fatalf("headers were not sent as configured: %v", h)
			}
			if !strings.HasPrefix(h.Get("X-Request-ID"), "sl-") {
				t.Fatalf("request id: %q", h.Get("X-Request-ID"))
			}
			if got.paths[0] != "/ingest?key=SECRETKEY" {
				t.Fatalf("the address was changed: %q", got.paths[0])
			}
			if snap.StatusCounts["200"] != tc.requests || !snap.External || snap.Target != srv.URL {
				t.Fatalf("status counts %v, external=%v, target %q", snap.StatusCounts, snap.External, snap.Target)
			}
			// Nothing secret may be in what the panel receives.
			out, _ := json.Marshal(snap)
			for _, secret := range []string{"SECRETKEY", "SECRETTOKEN", "acme"} {
				if strings.Contains(string(out), secret) {
					t.Fatalf("the snapshot leaks %q: %s", secret, out)
				}
			}
			if snap.Config.TargetHeaders["Authorization"] != "(hidden)" {
				t.Fatalf("header values should be hidden: %v", snap.Config.TargetHeaders)
			}
		})
	}
}

func TestExternalAcceptsLargeBatchesAndUsesAnAckWhenThereIsOne(t *testing.T) {
	var got received
	srv := httptest.NewServer(got.handler(http.StatusAccepted, `{"accepted":3,"rejected":2,"rejection_counts":{"out_of_range":2}}`))
	defer srv.Close()
	c := external(srv.URL)
	c.BatchSize = 1000 // above the built-in service's limit of 500
	run := NewRunner()
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	snap := waitDone(t, run)
	if len(got.bodies) != 1 {
		t.Fatalf("12 records in one batch of 1000 should be one request, got %d", len(got.bodies))
	}
	// The reply is not for 12 records (3+2 != 12), so it is not trusted: every record counts as taken.
	if snap.Accepted != 12 || snap.Rejected != 0 {
		t.Fatalf("accepted=%d rejected=%d", snap.Accepted, snap.Rejected)
	}

	// A reply that adds up to the batch (here: another Signal Lab service) is used as it is.
	var got2 received
	srv2 := httptest.NewServer(got2.handler(http.StatusAccepted, `{"accepted":10,"rejected":2,"rejection_counts":{"out_of_range":2}}`))
	defer srv2.Close()
	c.TargetURL = srv2.URL
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	snap = waitDone(t, run)
	if snap.Accepted != 10 || snap.Rejected != 2 || snap.RejectionCounts["out_of_range"] != 2 {
		t.Fatalf("ack not used: %+v", snap)
	}
}

func TestExternalRetriesTooManyRequestsAndRecordsErrors(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		mu.Lock()
		first := !seen[id]
		seen[id] = true
		mu.Unlock()
		if first { // every request is turned away once, then taken
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	run := NewRunner()
	c := external(srv.URL)
	c.Retries = 3
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	snap := waitDone(t, run)
	if snap.Throttled != 3 || snap.Retries != 3 || snap.Accepted != 12 || snap.GaveUpRecords != 0 || snap.RequestErrors != 0 {
		t.Fatalf("429 handling: %+v", snap)
	}
	if snap.StatusCounts["429"] != 3 || snap.StatusCounts["204"] != 3 {
		t.Fatalf("status counts: %v", snap.StatusCounts)
	}
}

func TestExternalErrorStatusIsReportedWithAnExcerpt(t *testing.T) {
	var got received
	srv := httptest.NewServer(got.handler(http.StatusInternalServerError, "{\"error\":\"database\\nis down\"}\x00\x01"+strings.Repeat("x", 500)))
	defer srv.Close()
	run := NewRunner()
	if _, err := run.Start(external(srv.URL), Target{}, 500); err != nil {
		t.Fatal(err)
	}
	snap := waitDone(t, run)
	if snap.RequestErrors != 3 || snap.ErroredRecords != 12 || snap.Accepted != 0 || snap.StatusCounts["500"] != 3 {
		t.Fatalf("errors: %+v", snap)
	}
	e := snap.FirstError
	if e == nil || e.Status != 500 || e.Batch != 1 || !strings.Contains(e.Message, "500") {
		t.Fatalf("first error: %+v", e)
	}
	if !strings.HasPrefix(e.Body, `{"error":"database\nis down"}`) || len(e.Body) > 310 || strings.ContainsAny(e.Body, "\x00\x01\n") || !strings.HasSuffix(e.Body, "…") {
		t.Fatalf("body excerpt should be one clean, shortened line: %q", e.Body)
	}
}

func TestExternalDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere.Add(1) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := external(srv.URL)
	c.TargetHeaders = map[string]string{"Authorization": "Bearer x"}
	run := NewRunner()
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	snap := waitDone(t, run)
	if elsewhere.Load() != 0 {
		t.Fatal("a redirect was followed: the headers would have gone to another address")
	}
	if snap.StatusCounts["307"] != 3 || snap.RequestErrors != 3 || snap.FirstError == nil || snap.FirstError.Status != 307 {
		t.Fatalf("a redirect should be reported as an error: %+v", snap)
	}
}

func TestExternalTransportErrorsDoNotRevealTheAddress(t *testing.T) {
	c := external("http://127.0.0.1:1/private/path?key=TOPSECRET") // nothing listens on port 1
	run := NewRunner()
	if _, err := run.Start(c, Target{}, 500); err != nil {
		t.Fatal(err)
	}
	snap := waitDone(t, run)
	if snap.FirstError == nil || snap.RequestErrors != 3 {
		t.Fatalf("expected request errors: %+v", snap)
	}
	out, _ := json.Marshal(snap)
	for _, secret := range []string{"TOPSECRET", "private/path"} {
		if strings.Contains(string(out), secret) {
			t.Fatalf("snapshot reveals %q: %s", secret, out)
		}
	}
	if snap.Target != "http://127.0.0.1:1" {
		t.Fatalf("display address: %q", snap.Target)
	}
}

func TestExternalNeverReceivesTheAppToken(t *testing.T) {
	var got received
	srv := httptest.NewServer(got.handler(http.StatusOK, ""))
	defer srv.Close()
	run := NewRunner()
	// The caller passes the built-in service's token header, as the app does for its own service.
	tok := Target{BaseURL: "http://127.0.0.1:9", Headers: map[string]string{"X-SignalLab-Token": "APPTOKEN"}}
	if _, err := run.Start(external(srv.URL), tok, 500); err != nil {
		t.Fatal(err)
	}
	waitDone(t, run)
	for _, h := range got.headers {
		if h.Get("X-SignalLab-Token") != "" {
			t.Fatal("the app's access token was sent to a service of the user's own")
		}
	}
}

func TestSingleFormatSendsOneRecordPerRequestWhatEverTheBatchSize(t *testing.T) {
	var got received
	srv := httptest.NewServer(got.handler(http.StatusOK, ""))
	defer srv.Close()
	c := external(srv.URL)
	c.PayloadFormat, c.BatchSize = FormatSingle, 50
	run := NewRunner()
	snap, err := run.Start(c, Target{}, 500)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Batches != 12 || snap.Config.BatchSize != 1 {
		t.Fatalf("single should mean 12 requests of 1: %+v", snap)
	}
	waitDone(t, run)
}

func TestValidateTarget(t *testing.T) {
	ok := func(mut func(*Config)) Config {
		c := external("https://api.example.com/v1/telemetry")
		c.TargetConfirmed, c.RatePerS = true, 100
		mut(&c)
		return c
	}
	if err := ok(func(*Config) {}).Validate(5000); err != nil {
		t.Fatalf("a normal configuration should validate: %v", err)
	}
	bad := map[string]func(*Config){
		"empty after spaces": func(c *Config) { c.TargetURL = "   "; c.TargetHeaders = map[string]string{"X": "1"} },
		"ftp":                func(c *Config) { c.TargetURL = "ftp://example.com/x" },
		"no scheme":          func(c *Config) { c.TargetURL = "example.com/x" },
		"credentials":        func(c *Config) { c.TargetURL = "https://user:pw@example.com/x" },
		"fragment":           func(c *Config) { c.TargetURL = "https://example.com/x#frag" },
		"not confirmed":      func(c *Config) { c.TargetConfirmed = false },
		"unpaced":            func(c *Config) { c.RatePerS = 0 },
		"too fast":           func(c *Config) { c.RatePerS = MaxExternalRate + 1 },
		"format":             func(c *Config) { c.PayloadFormat = "xml" },
		"header name":        func(c *Config) { c.TargetHeaders = map[string]string{"Bad Name": "x"} },
		"header value":       func(c *Config) { c.TargetHeaders = map[string]string{"X-A": "line\r\nInjected: 1"} },
		"reserved header":    func(c *Config) { c.TargetHeaders = map[string]string{"Host": "evil.example"} },
		"long header":        func(c *Config) { c.TargetHeaders = map[string]string{"X-A": strings.Repeat("a", 5000)} },
		"many headers": func(c *Config) {
			c.TargetHeaders = map[string]string{}
			for i := 0; i < 25; i++ {
				c.TargetHeaders["X-H"+strings.Repeat("a", i+1)] = "1"
			}
		},
	}
	for name, mut := range bad {
		if err := ok(mut).Validate(5000); err == nil {
			t.Errorf("%s: expected a validation error", name)
		}
	}
	// This computer needs neither confirmation nor a rate cap.
	for _, u := range []string{"http://localhost:3000/x", "http://127.0.0.1:8080", "http://[::1]:9000/ingest", "http://api.localhost/x"} {
		c := external(u)
		c.RatePerS = 0
		if err := c.Validate(5000); err != nil {
			t.Errorf("%s should be allowed without confirmation: %v", u, err)
		}
	}
	// Destination fields without an address are a mistake, not something to ignore silently.
	c := small()
	c.TargetHeaders = map[string]string{"Authorization": "x"}
	if err := c.Validate(500); err == nil {
		t.Error("headers without an address should be refused")
	}
	c = small()
	c.PayloadFormat = FormatArray
	if err := c.Validate(500); err == nil {
		t.Error("a payload format without an address should be refused")
	}
}

func TestDisplayURLAndLoopback(t *testing.T) {
	for in, want := range map[string]string{
		"https://api.example.com/v1/t?key=abc":        "https://api.example.com",
		"http://localhost:3000":                       "http://localhost:3000",
		"https://u:p@host.example/x":                  "https://host.example",
		"https://hooks.example.com/services/T0/B0/XY": "https://hooks.example.com",
		"nonsense": "",
	} {
		if got := DisplayURL(in); got != want {
			t.Errorf("DisplayURL(%q) = %q, want %q", in, got, want)
		}
	}
	for host, want := range map[string]bool{"localhost": true, "LOCALHOST": true, "127.0.0.1": true, "127.5.5.5": true, "::1": true, "[::1]": true, "a.localhost": true,
		"example.com": false, "192.168.1.5": false, "10.0.0.1": false, "localhost.evil.com": false} {
		if IsLoopbackHost(host) != want {
			t.Errorf("IsLoopbackHost(%q) should be %v", host, want)
		}
	}
}

func TestRedactedHidesHeaderValuesAndQuery(t *testing.T) {
	c := external("https://api.example.com/v1?key=abc")
	c.TargetHeaders = map[string]string{"Authorization": "Bearer abc"}
	r := c.Redacted()
	if strings.Contains(r.TargetURL, "abc") || r.TargetHeaders["Authorization"] != "(hidden)" {
		t.Fatalf("not redacted: %+v", r)
	}
	if c.TargetHeaders["Authorization"] != "Bearer abc" || !strings.Contains(c.TargetURL, "key=abc") {
		t.Fatal("redaction must not change the original")
	}
}
