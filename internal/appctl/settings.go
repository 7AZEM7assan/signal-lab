// Package appctl is the control plane of the desktop app: it owns the embedded database, the
// ingest pipeline and the replay runner, serves the control panel, and exposes the small
// /app/api used by the panel (settings, replay, data browsing and export, storage).
//
// It reuses the same api, pipeline, hub and event packages as the PostgreSQL service, so the
// app behaves like the service with a different store.
package appctl

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"signallab/internal/alert"
	"signallab/internal/config"
)

// Settings are the values a user may change from the panel.
type Settings struct {
	TempAlertC       float64 `json:"temp_alert_c"`
	VibAlertMMS      float64 `json:"vib_alert_mm_s"`
	QueueCapacity    int     `json:"queue_capacity"`
	Workers          int     `json:"workers"`
	WorkerBatchSize  int     `json:"worker_batch_size"`
	LabWorkerDelayMS int     `json:"lab_worker_delay_ms"`
}

// DefaultSettings match the service's defaults.
func DefaultSettings() Settings {
	return Settings{TempAlertC: 85, VibAlertMMS: 7.1, QueueCapacity: 1000, Workers: 4, WorkerBatchSize: 100}
}

// Thresholds converts to the alert package's type.
func (s Settings) Thresholds() alert.Thresholds {
	return alert.Thresholds{TemperatureC: s.TempAlertC, VibrationMMS: s.VibAlertMMS}
}

// structural reports whether changing from old to s needs the ingest engine to be rebuilt
// (the thresholds can be changed live; the queue and workers cannot).
func (s Settings) structural(old Settings) bool {
	return s.QueueCapacity != old.QueueCapacity || s.Workers != old.Workers ||
		s.WorkerBatchSize != old.WorkerBatchSize || s.LabWorkerDelayMS != old.LabWorkerDelayMS
}

// Config validates the settings through the service's own config loader and returns the
// full service configuration. The reused validation means the app accepts and rejects
// exactly what the service does.
func (s Settings) Config() (config.Config, error) {
	if s.LabWorkerDelayMS < 0 || s.LabWorkerDelayMS > 10_000 {
		return config.Config{}, errors.New("lab worker delay must be 0-10000 ms")
	}
	if s.TempAlertC <= 0 || s.VibAlertMMS <= 0 {
		return config.Config{}, errors.New("alert thresholds must be positive")
	}
	env := map[string]string{
		"SIGNALLAB_DATABASE_URL":      "sqlite", // unused: only satisfies the service's required setting
		"SIGNALLAB_TEMP_ALERT_C":      strconv.FormatFloat(s.TempAlertC, 'f', -1, 64),
		"SIGNALLAB_VIB_ALERT_MM_S":    strconv.FormatFloat(s.VibAlertMMS, 'f', -1, 64),
		"SIGNALLAB_QUEUE_CAPACITY":    strconv.Itoa(s.QueueCapacity),
		"SIGNALLAB_WORKERS":           strconv.Itoa(s.Workers),
		"SIGNALLAB_WORKER_BATCH_SIZE": strconv.Itoa(s.WorkerBatchSize),
		"SIGNALLAB_LAB_WORKER_DELAY":  (time.Duration(s.LabWorkerDelayMS) * time.Millisecond).String(),
		// A batch must fit in the queue, so cap it at the queue size.
		"SIGNALLAB_MAX_BATCH_EVENTS": strconv.Itoa(min(500, max(1, s.QueueCapacity))),
		"SIGNALLAB_QUERY_MAX_LIMIT":  "1000",
		"SIGNALLAB_LOG_FORMAT":       "text",
	}
	return config.Load(func(k string) (string, bool) { v, ok := env[k]; return v, ok })
}

func settingsPath(dir string) string { return filepath.Join(dir, "settings.json") }

// LoadSettings reads settings.json, returning defaults when it does not exist yet.
func LoadSettings(dir string) (Settings, error) {
	s := DefaultSettings()
	raw, err := os.ReadFile(settingsPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return DefaultSettings(), fmt.Errorf("settings.json is not valid: %w", err)
	}
	if _, err := s.Config(); err != nil {
		return DefaultSettings(), fmt.Errorf("settings.json has invalid values: %w", err)
	}
	return s, nil
}

// SaveSettings writes settings.json atomically (temp file in the same directory, then rename).
func SaveSettings(dir string, s Settings) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "settings-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), settingsPath(dir))
}
