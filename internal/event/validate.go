package event

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
)

// Rejection reasons. This set is closed so it is safe as a metric label.
const (
	ReasonMalformedJSON     = "malformed_json"
	ReasonUnknownField      = "unknown_field"
	ReasonUnsupportedSchema = "unsupported_schema_version"
	ReasonMissingField      = "missing_field"
	ReasonInvalidEventID    = "invalid_event_id"
	ReasonInvalidDeviceID   = "invalid_device_id"
	ReasonInvalidSiteID     = "invalid_site_id"
	ReasonInvalidTimestamp  = "invalid_timestamp"
	ReasonTimeInFuture      = "time_in_future"
	ReasonInvalidSequence   = "invalid_sequence"
	ReasonOutOfRange        = "out_of_range"
	ReasonDuplicateInBatch  = "duplicate_in_batch"
)

// Reasons lists every reason, for pre-registering metric series and docs.
var Reasons = []string{
	ReasonMalformedJSON, ReasonUnknownField, ReasonUnsupportedSchema, ReasonMissingField,
	ReasonInvalidEventID, ReasonInvalidDeviceID, ReasonInvalidSiteID, ReasonInvalidTimestamp,
	ReasonTimeInFuture, ReasonInvalidSequence, ReasonOutOfRange, ReasonDuplicateInBatch,
}

// Rejection explains why a record was refused. Detail is fixed text naming the
// offending field; it never echoes payload content, so it is safe to log and return.
type Rejection struct {
	Reason string
	Detail string
}

func (r *Rejection) Error() string { return r.Reason + ": " + r.Detail }

// Limits are the plausibility bounds applied during validation.
type Limits struct {
	TempMinC, TempMaxC float64
	VibMaxMMS          float64
	MaxFutureSkew      time.Duration
}

var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Parse decodes and validates one raw record. now is the server receive time and
// is stamped onto the result as ReceivedAt.
func Parse(raw []byte, lim Limits, now time.Time) (Event, *Rejection) {
	var w Wire
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Event{}, classifyDecodeError(err)
	}
	return Validate(w, lim, now)
}

func classifyDecodeError(err error) *Rejection {
	// encoding/json reports unknown fields only as text.
	if strings.HasPrefix(err.Error(), "json: unknown field ") {
		return &Rejection{ReasonUnknownField, "record contains a field that is not part of schema v1"}
	}
	if te, ok := err.(*json.UnmarshalTypeError); ok {
		return &Rejection{ReasonMalformedJSON, fmt.Sprintf("field %q has the wrong JSON type", te.Field)}
	}
	return &Rejection{ReasonMalformedJSON, "record is not a valid JSON object of the expected shape"}
}

// Validate checks a decoded record against schema v1 and the given limits.
func Validate(w Wire, lim Limits, now time.Time) (Event, *Rejection) {
	reject := func(reason, detail string) (Event, *Rejection) {
		return Event{}, &Rejection{Reason: reason, Detail: detail}
	}

	if w.SchemaVersion == nil {
		return reject(ReasonMissingField, "schema_version is required")
	}
	if *w.SchemaVersion != SchemaVersion {
		return reject(ReasonUnsupportedSchema, fmt.Sprintf("schema_version must be %d", SchemaVersion))
	}
	if w.EventID == nil {
		return reject(ReasonMissingField, "event_id is required")
	}
	if !idPattern.MatchString(*w.EventID) {
		return reject(ReasonInvalidEventID, "event_id must match [A-Za-z0-9][A-Za-z0-9._:-]{0,63}")
	}
	if w.DeviceID == nil {
		return reject(ReasonMissingField, "device_id is required")
	}
	if !idPattern.MatchString(*w.DeviceID) {
		return reject(ReasonInvalidDeviceID, "device_id must match [A-Za-z0-9][A-Za-z0-9._:-]{0,63}")
	}
	if w.EventTime == nil {
		return reject(ReasonMissingField, "event_time is required")
	}
	t, err := time.Parse(time.RFC3339Nano, *w.EventTime)
	if err != nil {
		return reject(ReasonInvalidTimestamp, "event_time must be an RFC 3339 timestamp")
	}
	t = t.UTC().Truncate(time.Microsecond)
	if t.Year() < 1970 || t.Year() > 9999 {
		return reject(ReasonInvalidTimestamp, "event_time year out of supported range")
	}
	if t.After(now.Add(lim.MaxFutureSkew)) {
		return reject(ReasonTimeInFuture, "event_time is further in the future than the allowed clock skew")
	}
	if w.TemperatureC == nil {
		return reject(ReasonMissingField, "temperature_c is required")
	}
	if w.VibrationMMS == nil {
		return reject(ReasonMissingField, "vibration_mm_s is required")
	}
	if !finite(*w.TemperatureC) || *w.TemperatureC < lim.TempMinC || *w.TemperatureC > lim.TempMaxC {
		return reject(ReasonOutOfRange, fmt.Sprintf("temperature_c must be finite and within [%g, %g]", lim.TempMinC, lim.TempMaxC))
	}
	if !finite(*w.VibrationMMS) || *w.VibrationMMS < 0 || *w.VibrationMMS > lim.VibMaxMMS {
		return reject(ReasonOutOfRange, fmt.Sprintf("vibration_mm_s must be finite and within [0, %g]", lim.VibMaxMMS))
	}
	if w.Sequence != nil && *w.Sequence < 0 {
		return reject(ReasonInvalidSequence, "sequence must be >= 0")
	}
	site := ""
	if w.SiteID != nil {
		if !idPattern.MatchString(*w.SiteID) {
			return reject(ReasonInvalidSiteID, "site_id must match [A-Za-z0-9][A-Za-z0-9._:-]{0,63}")
		}
		site = *w.SiteID
	}

	return Event{
		SchemaVersion: SchemaVersion,
		EventID:       *w.EventID,
		DeviceID:      *w.DeviceID,
		EventTime:     t,
		ReceivedAt:    now.UTC().Truncate(time.Microsecond),
		Sequence:      w.Sequence,
		TemperatureC:  *w.TemperatureC,
		VibrationMMS:  *w.VibrationMMS,
		SiteID:        site,
	}, nil
}

// ValidID reports whether s is an acceptable identifier (event, device or site).
func ValidID(s string) bool { return idPattern.MatchString(s) }

func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// BatchDeduper implements the in-request half of the duplicate policy: within one
// request, a repeated event_id or a repeated (device_id, sequence) is rejected.
// Duplicates across requests are resolved by the database (see store package).
type BatchDeduper struct {
	ids  map[string]struct{}
	seqs map[seqKey]struct{}
}

type seqKey struct {
	device string
	seq    int64
}

func NewBatchDeduper() *BatchDeduper {
	return &BatchDeduper{ids: map[string]struct{}{}, seqs: map[seqKey]struct{}{}}
}

// Check reports a rejection when ev repeats an earlier accepted record in this batch.
// Only records that pass are remembered.
func (d *BatchDeduper) Check(ev Event) *Rejection {
	if _, dup := d.ids[ev.EventID]; dup {
		return &Rejection{ReasonDuplicateInBatch, "event_id already appears earlier in this batch"}
	}
	if ev.Sequence != nil {
		k := seqKey{ev.DeviceID, *ev.Sequence}
		if _, dup := d.seqs[k]; dup {
			return &Rejection{ReasonDuplicateInBatch, "device_id and sequence already appear earlier in this batch"}
		}
		d.seqs[k] = struct{}{}
	}
	d.ids[ev.EventID] = struct{}{}
	return nil
}
