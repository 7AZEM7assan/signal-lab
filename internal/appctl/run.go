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
	// Ready is called once the listener is up with the URL (including the token) to open.
	Ready func(url string)
}

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
	if err := os.MkdirAll(o.DataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	eng, err := NewEngine(o.DataDir, o.Log)
	if err != nil {
		return fmt.Errorf("start engine: %w", err)
	}
	ln, err := ListenLoopback(o.Addr)
	if err != nil {
		eng.Close(context.Background())
		return err
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
		o.Ready("http://127.0.0.1:" + strconv.Itoa(port) + "/?token=" + o.Token)
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
