// Package alert evaluates the MVP's deterministic threshold rules.
package alert

import (
	"time"

	"signallab/internal/event"
)

// Rule names are a closed set, so they are safe as metric labels.
type Rule string

const (
	RuleTemperatureHigh Rule = "temperature_high"
	RuleVibrationHigh   Rule = "vibration_high"
)

// Rules lists every rule, for pre-registering metric series.
var Rules = []Rule{RuleTemperatureHigh, RuleVibrationHigh}

// Thresholds are inclusive: a reading equal to the threshold fires the rule.
type Thresholds struct {
	TemperatureC float64
	VibrationMMS float64
}

// Alert is one rule firing for one event.
type Alert struct {
	ID        string    `json:"alert_id"`
	DeviceID  string    `json:"device_id"`
	EventID   string    `json:"event_id"`
	Rule      Rule      `json:"rule"`
	Threshold float64   `json:"threshold"`
	Observed  float64   `json:"observed"`
	EventTime time.Time `json:"event_time"`
	CreatedAt time.Time `json:"created_at"`
}

// ID returns the deterministic alert identifier for (event, rule). Because it is
// derived rather than random, an event yields at most one alert per rule and
// re-evaluating the same event can never create a second row.
func ID(eventID string, rule Rule) string { return eventID + ":" + string(rule) }

// Evaluate returns the alerts fired by ev, at most one per rule, in rule order.
func (t Thresholds) Evaluate(ev event.Event, now time.Time) []Alert {
	var out []Alert
	if ev.TemperatureC >= t.TemperatureC {
		out = append(out, t.build(ev, RuleTemperatureHigh, t.TemperatureC, ev.TemperatureC, now))
	}
	if ev.VibrationMMS >= t.VibrationMMS {
		out = append(out, t.build(ev, RuleVibrationHigh, t.VibrationMMS, ev.VibrationMMS, now))
	}
	return out
}

func (Thresholds) build(ev event.Event, rule Rule, threshold, observed float64, now time.Time) Alert {
	return Alert{
		ID: ID(ev.EventID, rule), DeviceID: ev.DeviceID, EventID: ev.EventID, Rule: rule,
		Threshold: threshold, Observed: observed, EventTime: ev.EventTime,
		CreatedAt: now.UTC().Truncate(time.Microsecond),
	}
}
