// Package event defines the versioned telemetry schema and its validation.
//
// Units: temperature_c is degrees Celsius; vibration_mm_s is RMS vibration
// velocity in millimetres per second. event_time is the source's UTC timestamp;
// received_at is assigned by the server on arrival and is never taken from the client.
package event

import (
	"encoding/json"
	"time"
)

// SchemaVersion is the only schema version this service accepts.
const SchemaVersion = 1

// Event is a validated, normalised reading. All timestamps are UTC with
// microsecond precision (the precision PostgreSQL stores).
type Event struct {
	SchemaVersion int       `json:"schema_version"`
	EventID       string    `json:"event_id"`
	DeviceID      string    `json:"device_id"`
	EventTime     time.Time `json:"event_time"`
	ReceivedAt    time.Time `json:"received_at"`
	Sequence      *int64    `json:"sequence,omitempty"`
	TemperatureC  float64   `json:"temperature_c"`
	VibrationMMS  float64   `json:"vibration_mm_s"`
	SiteID        string    `json:"site_id,omitempty"`
}

// Wire is the untrusted shape of one record in a request. Pointers distinguish
// "missing" from zero. received_at is deliberately absent: a client-supplied
// value is rejected as an unknown field rather than silently trusted.
type Wire struct {
	SchemaVersion *int     `json:"schema_version"`
	EventID       *string  `json:"event_id"`
	DeviceID      *string  `json:"device_id"`
	EventTime     *string  `json:"event_time"`
	Sequence      *int64   `json:"sequence"`
	TemperatureC  *float64 `json:"temperature_c"`
	VibrationMMS  *float64 `json:"vibration_mm_s"`
	SiteID        *string  `json:"site_id"`
}

// Marshal is a convenience for tests and tools.
func (e Event) Marshal() []byte {
	b, _ := json.Marshal(e)
	return b
}

// Lag is how long after the source timestamp the server received the event.
// Negative values mean the source clock is ahead of the server's.
func Lag(eventTime, receivedAt time.Time) time.Duration {
	return receivedAt.Sub(eventTime)
}
