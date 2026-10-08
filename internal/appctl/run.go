package appctl

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// Options configure Run.
type Options struct {
	Addr    string // loopback address; port 0 picks a free one
	DataDir string
	Token   string // empty generates a random one
	Version string
	UI      http.Handler
	Log     *slog.Logger
	// Ready is called once the listener is up. It is the only place the token leaves Run besides
	// the HTTP cookie flow: callers decide where to put it (a terminal, or a pipe to the launcher),
	// and nothing in this package logs it.
	Ready func(Ready)
}

// Ready describes a started server.
type Ready struct {
	Addr  string // host:port, e.g. 127.0.0.1:51734
	Port  int
	Token string
}

// URL is the address without any secret, safe to log.
func (r Ready) URL() string { return "http://" + r.Addr }

// LinkURL is the address plus the one-time token: opening it in a browser starts a session.
// It is a credential; print it to the person who owns the machine, never to a log file.
func (r Ready) LinkURL() string { return r.URL() + "/?token=" + r.Token }

// NewToken returns a random 256-bit hex token.
func NewToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// Run serves the app until ctx is cancelled or a shutdown is requested through the API, then
// shuts down in order: stop accepting requests, stop any replay, drain the queue to the
// database, close live connections and the database.
func Run(ctx context.Context, o Options) error {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Token == "" {
		t, err := NewToken()
		if err != nil {
			return err
		}
		o.Token = t
	}
	if len(o.Token) < 16 {
		return errors.New("token must be at least 16 characters")
	}
	if err := CheckWritable(o.DataDir); err != nil {
		return err
	}
	// Listen first so a busy port is reported before anything is created on disk.
	ln, err := ListenLoopback(o.Addr)
	if err != nil {
		return err
	}
	eng, err := NewEngine(o.DataDir, o.Log)
	if err != nil {
		_ = ln.Close()
		return fmt.Errorf("could not open the database in %q: %w", o.DataDir, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	ctx, stop := context.WithCancel(ctx)
	defer stop()
	srv := &Server{Engine: eng, UI: o.UI, Token: o.Token, Port: port, DataDir: o.DataDir, Version: o.Version,
		Started: time.Now(), Log: o.Log, Shutdown: stop}
	httpServer := &http.Server{
		Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second,
		IdleTimeout: 60 * time.Second, // no WriteTimeout: exports and WebSockets are long-lived
	}
	errc := make(chan error, 1)
	go func() { errc <- httpServer.Serve(ln) }()
	o.Log.Info("signal lab app started", "addr", ln.Addr().String(), "data_dir", o.DataDir, "version", o.Version)
	if o.Ready != nil {
		o.Ready(Ready{Addr: "127.0.0.1:" + strconv.Itoa(port), Port: port, Token: o.Token})
	}

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			eng.Close(context.Background())
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		o.Log.Info("shutdown requested")
	}
	sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	eng.Runner.Stop()
	if err := httpServer.Shutdown(sctx); err != nil {
		o.Log.Warn("http shutdown incomplete", "error", err)
	}
	eng.Close(sctx)
	o.Log.Info("shutdown complete")
	return nil
}

// CheckWritable makes sure dir exists and files can be created in it, and says why if not.
// Without this a read-only folder surfaces later as an obscure SQLite error.
func CheckWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("the data folder %q cannot be created: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, ".write-test-*")
	if err != nil {
		return fmt.Errorf("the data folder %q is not writable: %w", dir, err)
	}
	name := f.Name()
	_ = f.Close()
	if err := os.Remove(name); err != nil {
		return fmt.Errorf("the data folder %q is not writable: %w", dir, err)
	}
	return nil
}
