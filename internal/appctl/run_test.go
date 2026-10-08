package appctl

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestRunNeverLogsTheTokenButHandsItToTheLauncher(t *testing.T) {
	logs := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	ready := make(chan Ready, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Options{Addr: "127.0.0.1:0", DataDir: t.TempDir(), Token: testToken, Version: "test", Log: log,
			UI:    http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "panel") }),
			Ready: func(r Ready) { ready <- r }})
	}()
	var r Ready
	select {
	case r = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("app did not start")
	}
	if r.Token != testToken || r.Port == 0 || r.URL() != "http://"+r.Addr || strings.Contains(r.URL(), "token") {
		t.Fatalf("ready info is wrong: %+v (url %q)", r, r.URL())
	}
	if !strings.Contains(r.LinkURL(), "?token="+testToken) {
		t.Fatalf("link must carry the token: %q", r.LinkURL())
	}

	// Exercise the paths that log: the link flow (including a wrong token), the API, a replay.
	hit := func(method, path string, body any, hdr map[string]string) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, r.URL()+path, rd)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	tok := map[string]string{TokenHeader: testToken}
	hit("GET", "/?token="+testToken, nil, nil)
	hit("GET", "/?token=not-the-token", nil, nil)
	hit("GET", "/app/api/state", nil, tok)
	hit("PUT", "/app/api/settings", map[string]any{"temp_alert_c": 90}, tok)
	hit("POST", "/app/api/replay/start", map[string]any{"devices": 2, "duration_s": 10, "interval_s": 1, "rate_per_s": 0}, tok)
	time.Sleep(500 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("app did not shut down")
	}

	out := logs.String()
	if !strings.Contains(out, "signal lab app started") || !strings.Contains(out, r.Addr) {
		t.Fatalf("expected the address in the log, got:\n%s", out)
	}
	for _, secret := range []string{testToken, "token=", "not-the-token", TokenHeader} {
		if strings.Contains(out, secret) {
			t.Fatalf("log contains %q:\n%s", secret, out)
		}
	}
}

func TestRunExplainsAnUnusableDataFolder(t *testing.T) {
	file := filepath.Join(t.TempDir(), "i-am-a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"parent is a file": filepath.Join(file, "data"),
		"path is a file":   file,
	} {
		err := Run(context.Background(), Options{Addr: "127.0.0.1:0", DataDir: dir, Token: testToken, UI: http.NotFoundHandler()})
		if err == nil || !strings.Contains(err.Error(), "data folder") || !strings.Contains(err.Error(), dir) {
			t.Errorf("%s: want an error naming the data folder, got %v", name, err)
		}
	}
	if err := CheckWritable(t.TempDir()); err != nil {
		t.Errorf("a normal folder must pass: %v", err)
	}
}

func TestRunExplainsAPortThatIsAlreadyInUse(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	dir := t.TempDir()
	err = Run(context.Background(), Options{Addr: busy.Addr().String(), DataDir: dir, Token: testToken, UI: http.NotFoundHandler()})
	if err == nil || !strings.Contains(err.Error(), "port in use") || !strings.Contains(err.Error(), busy.Addr().String()) {
		t.Fatalf("want a port-in-use explanation naming %s, got %v", busy.Addr(), err)
	}
	// Nothing should have been created on disk for a start that failed on the port.
	if _, statErr := os.Stat(filepath.Join(dir, DBFileName)); statErr == nil {
		t.Error("the database was created even though the port was busy")
	}
}
