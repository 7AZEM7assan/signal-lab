package config

import (
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	c, err := Load(env(map[string]string{"SIGNALLAB_DATABASE_URL": "postgres://x"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.QueueCapacity != 1000 || c.Workers != 4 || c.MaxBatchEvents != 500 || c.TempAlertC != 85 || c.VibAlertMS != 7.1 {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if c.PprofAddr != "" {
		t.Fatal("pprof must be disabled by default")
	}
}

func TestOverrides(t *testing.T) {
	c, err := Load(env(map[string]string{
		"SIGNALLAB_DATABASE_URL": "postgres://x", "SIGNALLAB_QUEUE_CAPACITY": "50", "SIGNALLAB_MAX_BATCH_EVENTS": "50",
		"SIGNALLAB_LAB_WORKER_DELAY": "250ms", "SIGNALLAB_WS_ALLOWED_ORIGINS": "a.example, b.example", "SIGNALLAB_AUTO_MIGRATE": "true",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.QueueCapacity != 50 || c.LabWorkerDelay != 250*time.Millisecond || !c.AutoMigrate || len(c.WSAllowedOrigins) != 2 {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestInvalidSettingsAreAllReported(t *testing.T) {
	_, err := Load(env(map[string]string{
		"SIGNALLAB_QUEUE_CAPACITY": "10", "SIGNALLAB_MAX_BATCH_EVENTS": "20", // batch larger than queue
		"SIGNALLAB_WORKERS": "zero", "SIGNALLAB_SHUTDOWN_TIMEOUT": "soon", "SIGNALLAB_LOG_FORMAT": "xml",
	}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"DATABASE_URL is required", "MAX_BATCH_EVENTS", "WORKERS", "SHUTDOWN_TIMEOUT", "LOG_FORMAT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got:\n%v", want, err)
		}
	}
}

func TestThresholdMustBeWithinBounds(t *testing.T) {
	_, err := Load(env(map[string]string{"SIGNALLAB_DATABASE_URL": "x", "SIGNALLAB_VIB_ALERT_MM_S": "500"}))
	if err == nil || !strings.Contains(err.Error(), "VIB_ALERT_MM_S") {
		t.Fatalf("expected threshold error, got %v", err)
	}
}
