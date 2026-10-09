package lab

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"signallab/internal/event"
)

// Dataset is a file of readings the user imported, to replay instead of generated data.
// Its records use the Signal Lab field names; column names are matched leniently (see aliases).
type Dataset struct {
	Records []Record
	Summary DatasetSummary
}

// DatasetSummary describes an imported file: what was found and what the Signal Lab schema
// would make of it. It is safe to show: it holds counts, column names and times only.
type DatasetSummary struct {
	Name       string            `json:"name"`
	Format     string            `json:"format"` // csv, ndjson or json
	Rows       int               `json:"rows"`
	Devices    int               `json:"devices"`
	Columns    []string          `json:"columns"`           // as written in the file
	Mapped     map[string]string `json:"mapped,omitempty"`  // file column -> Signal Lab field, when the names differ
	Ignored    []string          `json:"ignored,omitempty"` // columns that are not part of the schema and are not sent
	FirstTime  string            `json:"first_time,omitempty"`
	LastTime   string            `json:"last_time,omitempty"`
	Problems   map[string]int    `json:"problems,omitempty"` // rows the Signal Lab schema would reject, by reason
	Derived    []string          `json:"derived,omitempty"`  // fields filled in by the importer (for example event_id)
	ImportedAt time.Time         `json:"imported_at"`
}

// Hard limits for an imported file.
const (
	// MaxDatasetBytes and MaxDatasetRows keep an import within a sensible amount of memory: every
	// row becomes a small map while the file is parsed and planned.
	MaxDatasetBytes = 32 << 20
	MaxDatasetRows  = 200_000
	maxLineBytes    = 1 << 20
)

// Fields of the Signal Lab event schema that a file may provide.
var schemaFields = []string{"schema_version", "event_id", "device_id", "event_time", "sequence", "temperature_c", "vibration_mm_s", "site_id"}

// Column names people commonly use, mapped to the schema field. Matching ignores case and treats
// spaces and dashes as underscores. Values are sent exactly as they are in the file: the app does
// not convert units.
var aliases = map[string]string{
	"timestamp": "event_time", "time": "event_time", "ts": "event_time", "datetime": "event_time",
	"date_time": "event_time", "event_timestamp": "event_time", "time_utc": "event_time", "measured_at": "event_time",
	"device": "device_id", "device_name": "device_id", "machine": "device_id", "machine_id": "device_id",
	"sensor": "device_id", "sensor_id": "device_id", "asset": "device_id", "asset_id": "device_id",
	"temperature": "temperature_c", "temp": "temperature_c", "temp_c": "temperature_c",
	"temperature_celsius": "temperature_c", "temperature_degc": "temperature_c",
	"vibration": "vibration_mm_s", "vib": "vibration_mm_s", "vibration_mms": "vibration_mm_s",
	"vibration_mm_per_s": "vibration_mm_s", "vibration_rms": "vibration_mm_s",
	"id": "event_id", "eventid": "event_id", "reading_id": "event_id", "message_id": "event_id",
	"seq": "sequence", "sequence_number": "sequence", "counter": "sequence",
	"site": "site_id", "plant": "site_id", "plant_id": "site_id", "facility": "site_id",
	"version": "schema_version",
}

func normalizeColumn(s string) string {
	s = strings.ToLower(strings.TrimSpace(strings.TrimPrefix(s, "\ufeff")))
	return strings.NewReplacer(" ", "_", "-", "_").Replace(s)
}

// columnMap decides, for each column of a file, which schema field it feeds (or "" to ignore it).
// A column with the exact schema name always beats an alias for the same field.
type columnMap struct {
	field   []string          // per column, "" = ignored
	mapped  map[string]string // original name -> field, for aliases only
	ignored []string
}

func mapColumns(names []string) columnMap {
	cm := columnMap{field: make([]string, len(names)), mapped: map[string]string{}}
	exact := map[string]bool{}
	for _, n := range names {
		c := normalizeColumn(n)
		for _, f := range schemaFields {
			if c == f {
				exact[f] = true
			}
		}
	}
	taken := map[string]bool{}
	for i, n := range names {
		c := normalizeColumn(n)
		f := ""
		for _, sf := range schemaFields {
			if c == sf {
				f = sf
			}
		}
		if f == "" {
			if a, ok := aliases[c]; ok && !exact[a] {
				f = a
				cm.mapped[strings.TrimSpace(n)] = a
			}
		}
		if f != "" && taken[f] {
			f = "" // a second column for the same field: the first one wins
		}
		if f == "" {
			cm.ignored = append(cm.ignored, strings.TrimSpace(n))
			continue
		}
		taken[f] = true
		cm.field[i] = f
	}
	if len(cm.mapped) == 0 {
		cm.mapped = nil
	}
	return cm
}

var requiredFields = []string{"event_time", "device_id", "temperature_c", "vibration_mm_s"}

func (cm columnMap) check(format string) error {
	have := map[string]bool{}
	for _, f := range cm.field {
		if f != "" {
			have[f] = true
		}
	}
	var missing []string
	for _, f := range requiredFields {
		if !have[f] {
			missing = append(missing, f)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("the file has no column for %s (it needs event_time, device_id, temperature_c and vibration_mm_s; common alternatives such as timestamp, device, temp and vibration are accepted)", strings.Join(missing, ", "))
	}
	return nil
}

// ParseDataset reads a CSV, NDJSON or JSON-array file. The format is detected from the first
// character: [ is a JSON array, { is NDJSON, anything else is CSV. lim and now are used only to
// count the rows the Signal Lab schema would reject.
func ParseDataset(name string, data []byte, lim event.Limits, now time.Time) (*Dataset, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	trimmed := bytes.TrimLeft(data, " \t\r\n")
	if len(trimmed) == 0 {
		return nil, errors.New("the file is empty")
	}
	var (
		format  string
		cols    []string
		rows    []map[string]any
		columns columnMap
		err     error
	)
	switch trimmed[0] {
	case '[':
		format = "json"
		rows, cols, err = readJSONArray(trimmed)
	case '{':
		format = "ndjson"
		rows, cols, err = readNDJSON(trimmed)
	default:
		format = "csv"
		rows, cols, columns, err = readCSV(data)
	}
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, errors.New("the file has a header but no rows")
	}
	if len(rows) > MaxDatasetRows {
		return nil, fmt.Errorf("the file has more than %d rows; split it and import part of it", MaxDatasetRows)
	}
	if format != "csv" {
		columns = mapColumns(cols)
	}
	if err := columns.check(format); err != nil {
		return nil, err
	}

	d := &Dataset{Summary: DatasetSummary{Name: name, Format: format, Rows: len(rows), Columns: cols, Mapped: columns.mapped, Ignored: columns.ignored, ImportedAt: now.UTC()}}
	haveID := false
	for _, f := range columns.field {
		if f == "event_id" {
			haveID = true
		}
	}
	if !haveID {
		d.Summary.Derived = append(d.Summary.Derived, "event_id (device id and row number)")
	}
	hasVersion := false
	for _, f := range columns.field {
		if f == "schema_version" {
			hasVersion = true
		}
	}
	if !hasVersion {
		d.Summary.Derived = append(d.Summary.Derived, "schema_version (1)")
	}

	devices := map[string]bool{}
	var first, last time.Time
	d.Records = make([]Record, 0, len(rows))
	for i, row := range rows {
		rec := Record{}
		for j, f := range columns.field {
			if f == "" {
				continue
			}
			if v, ok := row[cols[j]]; ok && v != nil {
				if s, isStr := v.(string); isStr && strings.TrimSpace(s) == "" {
					continue // an empty cell is a missing value
				}
				rec[f] = v
			}
		}
		if _, ok := rec["schema_version"]; !ok {
			rec["schema_version"] = 1
		}
		if _, ok := rec["event_id"]; !ok && !haveID {
			dev, _ := rec["device_id"].(string)
			rec["event_id"] = fmt.Sprintf("%s-%d", dev, i+1)
		}
		// The planner needs an event_time string to exist even when the file left it empty.
		if _, ok := rec["event_time"].(string); !ok {
			if v, present := rec["event_time"]; present {
				rec["event_time"] = fmt.Sprint(v)
			} else {
				rec["event_time"] = ""
			}
		}
		if dev, ok := rec["device_id"].(string); ok {
			devices[dev] = true
		}
		if ts, err := time.Parse(time.RFC3339, rec["event_time"].(string)); err == nil {
			if first.IsZero() || ts.Before(first) {
				first = ts
			}
			if ts.After(last) {
				last = ts
			}
		}
		d.Records = append(d.Records, rec)
	}
	d.Summary.Devices = len(devices)
	if !first.IsZero() {
		d.Summary.FirstTime, d.Summary.LastTime = FormatTime(first), FormatTime(last)
	}
	problems := map[string]int{}
	for _, rec := range d.Records {
		raw, err := json.Marshal(rec)
		if err != nil {
			problems[event.ReasonMalformedJSON]++
			continue
		}
		if _, rej := event.Parse(raw, lim, now); rej != nil {
			problems[rej.Reason]++
		}
	}
	if len(problems) > 0 {
		d.Summary.Problems = problems
	}
	return d, nil
}

func readJSONArray(data []byte) ([]map[string]any, []string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if _, err := dec.Token(); err != nil {
		return nil, nil, fmt.Errorf("the file is not a valid JSON array: %v", err)
	}
	var rows []map[string]any
	cols := newColumnSet()
	for dec.More() {
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			return nil, nil, fmt.Errorf("item %d is not a JSON object: %v", len(rows)+1, err)
		}
		cols.add(row)
		rows = append(rows, row)
		if len(rows) > MaxDatasetRows {
			break
		}
	}
	return rows, cols.list, nil
}

func readNDJSON(data []byte) ([]map[string]any, []string, error) {
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), maxLineBytes)
	var rows []map[string]any
	cols := newColumnSet()
	for line := 1; sc.Scan(); line++ {
		text := bytes.TrimSpace(sc.Bytes())
		if len(text) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(text))
		dec.UseNumber()
		var row map[string]any
		if err := dec.Decode(&row); err != nil || row == nil {
			return nil, nil, fmt.Errorf("line %d is not a JSON object", line)
		}
		cols.add(row)
		rows = append(rows, row)
		if len(rows) > MaxDatasetRows {
			break
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, fmt.Errorf("could not read the file: %v (a single line may be longer than 1 MiB)", err)
	}
	return rows, cols.list, nil
}

// columnSet collects the keys of JSON objects in first-seen order.
type columnSet struct {
	seen map[string]bool
	list []string
}

func newColumnSet() *columnSet { return &columnSet{seen: map[string]bool{}} }

func (c *columnSet) add(row map[string]any) {
	keys := make([]string, 0, len(row))
	for k := range row {
		if !c.seen[k] {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		c.seen[k] = true
		c.list = append(c.list, k)
	}
}

func readCSV(data []byte) ([]map[string]any, []string, columnMap, error) {
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, nil, columnMap{}, fmt.Errorf("could not read the header row: %v", err)
	}
	names := make([]string, len(header))
	copy(names, header)
	for i := range names {
		names[i] = strings.TrimSpace(strings.TrimPrefix(names[i], "\ufeff"))
	}
	cm := mapColumns(names)
	if err := cm.check("csv"); err != nil {
		return nil, nil, cm, err
	}
	var rows []map[string]any
	for line := 2; ; line++ {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, cm, fmt.Errorf("line %d: %v", line, err)
		}
		if len(rec) == 1 && strings.TrimSpace(rec[0]) == "" {
			continue // blank line
		}
		row := make(map[string]any, len(names))
		for i, n := range names {
			if i >= len(rec) || cm.field[i] == "" {
				continue
			}
			row[n] = csvValue(cm.field[i], strings.TrimSpace(rec[i]))
		}
		rows = append(rows, row)
		if len(rows) > MaxDatasetRows {
			break
		}
	}
	return rows, names, cm, nil
}

// csvValue types a cell the way the schema expects. A value that does not parse stays a string,
// so the service under test sees (and can reject) exactly what the file contained.
func csvValue(field, cell string) any {
	if cell == "" {
		return ""
	}
	switch field {
	case "temperature_c", "vibration_mm_s":
		if f, err := strconv.ParseFloat(cell, 64); err == nil {
			return json.Number(strconv.FormatFloat(f, 'f', -1, 64))
		}
	case "sequence", "schema_version":
		if n, err := strconv.ParseInt(cell, 10, 64); err == nil {
			return json.Number(strconv.FormatInt(n, 10))
		}
	}
	return cell
}

// Rebased returns the records with every parseable event_time shifted so the latest one is now,
// which keeps an old recording inside what most services accept as a recent timestamp. Records
// whose time cannot be parsed are left alone. The dataset itself is not changed.
func (d *Dataset) Rebased(now time.Time) []Record {
	var last time.Time
	for _, r := range d.Records {
		if ts, err := time.Parse(time.RFC3339, r["event_time"].(string)); err == nil && ts.After(last) {
			last = ts
		}
	}
	if last.IsZero() {
		return d.Records
	}
	delta := now.UTC().Sub(last)
	out := make([]Record, len(d.Records))
	for i, r := range d.Records {
		ts, err := time.Parse(time.RFC3339, r["event_time"].(string))
		if err != nil {
			out[i] = r
			continue
		}
		c := make(Record, len(r))
		for k, v := range r {
			c[k] = v
		}
		c["event_time"] = FormatTime(ts.Add(delta))
		out[i] = c
	}
	return out
}
