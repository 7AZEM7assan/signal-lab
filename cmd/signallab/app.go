package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"signallab/internal/appctl"
	"signallab/internal/appui"
)

// runApp starts the self-contained local app: embedded SQLite, the control panel and the same
// ingest service, bound to loopback only. The desktop shell runs this as a child process; it
// also works on its own in a terminal.
func runApp(args []string) error {
	fs := flag.NewFlagSet("app", flag.ContinueOnError)
	addr := fs.String("addr", envOr("SIGNALLAB_APP_ADDR", "127.0.0.1:0"), "loopback address to listen on (port 0 picks a free port)")
	dir := fs.String("data-dir", os.Getenv("SIGNALLAB_APP_DATA"), "folder for the database and settings (default: the per-user config folder)")
	readyJSON := fs.Bool("ready-json", false, "print one JSON line when ready (used by the desktop shell)")
	logLevel := fs.String("log-level", envOr("SIGNALLAB_LOG_LEVEL", "info"), "debug, info, warn or error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return fmt.Errorf("cannot find a per-user folder; pass --data-dir: %w", err)
		}
		*dir = filepath.Join(base, "signal-lab")
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q", *logLevel)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return appctl.Run(ctx, appctl.Options{
		Addr: *addr, DataDir: *dir, Token: os.Getenv("SIGNALLAB_APP_TOKEN"), Version: version,
		UI: appui.Handler(), Log: log,
		Ready: func(r appctl.Ready) {
			if *readyJSON {
				_ = json.NewEncoder(os.Stdout).Encode(readyMessage(r, *dir))
				return
			}
			fmt.Printf("\nSignal Lab is running.\n\n  Open:  %s\n  Data:  %s\n\nThis link contains a one-time token and only works on this computer. Press Ctrl+C to stop.\n\n", r.LinkURL(), *dir)
		},
	})
}

// readyMessage is the single machine-readable line the desktop shell reads from stdout. The token
// is a separate field so the shell can use it, while url, addr and port are safe to log. The
// shell must not write this line to a log file as is (it redacts the token).
func readyMessage(r appctl.Ready, dataDir string) map[string]any {
	return map[string]any{"event": "ready", "addr": r.Addr, "port": r.Port, "url": r.URL(), "token": r.Token, "data_dir": dataDir}
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
