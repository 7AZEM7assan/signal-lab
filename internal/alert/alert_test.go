package alert

import (
	"testing"
	"time"

	"signallab/internal/event"
)

var (
	th  = Thresholds{TemperatureC: 85, VibrationMMS: 7.1}
	now = time.Date(2025, 1, 15, 9, 0, 0, 0, time.UTC)
)

func ev(temp, vib float64) event.Event {
	return event.Event{EventID: "e1", DeviceID: "press-01", EventTime: now.Add(-time.Hour), TemperatureC: temp, VibrationMMS: vib}
}

func TestTemperatureBoundary(t *testing.T) {
	for _, tc := range []struct {
		temp  float64
		fires bool
	}{{84.99, false}, {85, true}, {85.01, true}} {
		got := th.Evaluate(ev(tc.temp, 0), now)
		if (len(got) == 1) != tc.fires {
			t.Errorf("temp %v: fired=%v, want %v", tc.temp, len(got) == 1, tc.fires)
		}
	}
}

func TestVibrationBoundary(t *testing.T) {
	for _, tc := range []struct {
		vib   float64
		fires bool
	}{{7.09, false}, {7.1, true}, {7.11, true}} {
		got := th.Evaluate(ev(20, tc.vib), now)
		if (len(got) == 1) != tc.fires {
			t.Errorf("vib %v: fired=%v, want %v", tc.vib, len(got) == 1, tc.fires)
		}
	}
}

func TestOneAlertPerRulePerEvent(t *testing.T) {
	got := th.Evaluate(ev(90, 9), now)
	if len(got) != 2 || got[0].Rule != RuleTemperatureHigh || got[1].Rule != RuleVibrationHigh {
		t.Fatalf("expected one alert per rule in rule order, got %+v", got)
	}
	again := th.Evaluate(ev(90, 9), now.Add(time.Minute))
	if got[0].ID != again[0].ID || got[1].ID != again[1].ID {
		t.Fatal("alert IDs must be deterministic for the same event and rule")
	}
	if got[0].ID == got[1].ID {
		t.Fatal("different rules on one event must have different IDs")
	}
}

func TestAlertFields(t *testing.T) {
	a := th.Evaluate(ev(91.2, 0), now)[0]
	if a.ID != "e1:temperature_high" || a.DeviceID != "press-01" || a.EventID != "e1" || a.Threshold != 85 || a.Observed != 91.2 {
		t.Fatalf("unexpected alert: %+v", a)
	}
	if !a.CreatedAt.Equal(now) || !a.EventTime.Equal(now.Add(-time.Hour)) {
		t.Fatalf("timestamps wrong: %+v", a)
	}
}

func TestNoAlertBelowThresholds(t *testing.T) {
	if got := th.Evaluate(ev(60, 2), now); len(got) != 0 {
		t.Fatalf("expected none, got %+v", got)
	}
}
