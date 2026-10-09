package appctl

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

func (a *app) putRaw(path, contentType string, body []byte) (int, []byte) {
	a.t.Helper()
	req, _ := http.NewRequest("PUT", a.base+path, bytes.NewReader(body))
	req.Header.Set(TokenHeader, testToken)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestReplayToAServiceOfYourOwnNeverSendsTheAppToken(t *testing.T) {
	a := newApp(t, "")
	var mu sync.Mutex
	var headers []http.Header
	var paths []string
	own := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers, paths = append(headers, r.Header.Clone()), append(paths, r.URL.RequestURI())
		mu.Unlock()
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer own.Close()

	body := replayBody(map[string]any{"target_url": own.URL + "/hooks/SEKRETPATH?key=SEKRETQUERY", "target_headers": map[string]string{"X-Api-Key": "SEKRETHEADER"}, "payload_format": "array"})
	var snap map[string]any
	if c := a.json("POST", "/app/api/replay/start", body, &snap); c != 202 {
		t.Fatalf("start: %d %v", c, snap)
	}
	done := a.waitReplay()
	if done["state"] != "done" || done["external"] != true || done["accepted"].(float64) != 240 {
		t.Fatalf("run: %v", done)
	}
	mu.Lock()
	gotHeaders, gotPaths := append([]http.Header(nil), headers...), append([]string(nil), paths...) // copies: the second run below keeps adding
	mu.Unlock()
	if len(gotHeaders) == 0 {
		t.Fatal("the service of your own received nothing")
	}
	for i, h := range gotHeaders {
		if h.Get("X-Signallab-Token") != "" {
			t.Fatal("the app's access token was sent to a service of the user's own")
		}
		if h.Get("X-Api-Key") != "SEKRETHEADER" || !strings.HasPrefix(h.Get("User-Agent"), "SignalLab/") || gotPaths[i] != "/hooks/SEKRETPATH?key=SEKRETQUERY" {
			t.Fatalf("request %d: headers %v path %q", i, h, gotPaths[i])
		}
	}
	// Nothing was stored in the app, and no response or state mentions the secrets.
	if e, _ := a.stats(); e != 0 {
		t.Fatalf("a replay to your own service must not store anything locally, stored %v", e)
	}
	for _, path := range []string{"/app/api/replay", "/app/api/state"} {
		_, raw := a.do("GET", path, nil)
		for _, secret := range []string{"SEKRETPATH", "SEKRETQUERY", "SEKRETHEADER"} {
			if strings.Contains(string(raw), secret) {
				t.Fatalf("%s reveals %s: %s", path, secret, raw)
			}
		}
	}
	_, started := a.do("POST", "/app/api/replay/start", body)
	for _, secret := range []string{"SEKRETPATH", "SEKRETQUERY", "SEKRETHEADER"} {
		if strings.Contains(string(started), secret) {
			t.Fatalf("the start response reveals %s: %s", secret, started)
		}
	}
	a.waitReplay()
}

func TestReplayToAnotherComputerNeedsConfirmationAndAPace(t *testing.T) {
	a := newApp(t, "")
	url := "https://example.invalid/hooks/SEKRETPATH?key=SEKRETQUERY"
	for name, extra := range map[string]map[string]any{
		"not confirmed": {"target_url": url, "rate_per_s": 100},
		"unpaced":       {"target_url": url, "target_confirmed": true, "rate_per_s": 0},
		"too fast":      {"target_url": url, "target_confirmed": true, "rate_per_s": 100000},
		"credentials":   {"target_url": "https://user:pw@example.invalid/x", "target_confirmed": true, "rate_per_s": 100},
		"headers alone": {"target_headers": map[string]string{"Authorization": "x"}},
	} {
		var out map[string]any
		code := a.json("POST", "/app/api/replay/start", replayBody(extra), &out)
		raw, _ := json.Marshal(out)
		if code != 400 {
			t.Errorf("%s: status %d, want 400: %s", name, code, raw)
		}
		for _, secret := range []string{"SEKRETPATH", "SEKRETQUERY", "user:pw"} {
			if strings.Contains(string(raw), secret) {
				t.Errorf("%s: the error reveals %s: %s", name, secret, raw)
			}
		}
	}
	var st map[string]any
	a.json("GET", "/app/api/replay", nil, &st)
	if st["state"] != "idle" {
		t.Fatalf("a refused request must not start a run: %v", st)
	}
}

func TestImportedFileIsReplayedIntoTheBuiltInServiceAndCanBeRemoved(t *testing.T) {
	a := newApp(t, "")
	csv := "Timestamp,Machine,Temp,Vibration,Operator\n"
	for i := 0; i < 30; i++ {
		csv += "2025-01-15T08:00:" + pad2(i) + "Z,press-0" + string(rune('1'+i%3)) + ",60.5,2.5,ann\n"
	}
	csv += "2025-01-15T08:01:00Z,press-01,999,2.5,ann\n" // out of range: reported at import, rejected by the service
	code, raw := a.putRaw("/app/api/replay/dataset?name=../../my%20plant.csv", "text/csv", []byte(csv))
	var imp struct {
		Dataset struct {
			Name     string         `json:"name"`
			Rows     int            `json:"rows"`
			Devices  int            `json:"devices"`
			Problems map[string]int `json:"problems"`
			Ignored  []string       `json:"ignored"`
		} `json:"dataset"`
	}
	if err := json.Unmarshal(raw, &imp); err != nil || code != 200 {
		t.Fatalf("import: %d %s", code, raw)
	}
	d := imp.Dataset
	if d.Name != "my plant.csv" || d.Rows != 31 || d.Devices != 3 || d.Problems["out_of_range"] != 1 || len(d.Ignored) != 1 {
		t.Fatalf("import summary: %+v", d)
	}
	var st struct {
		Dataset *struct{ Rows int } `json:"dataset"`
	}
	a.json("GET", "/app/api/state", nil, &st)
	if st.Dataset == nil || st.Dataset.Rows != 31 {
		t.Fatalf("state should describe the imported file: %+v", st)
	}

	var snap map[string]any
	body := replayBody(map[string]any{"use_dataset": true, "rebase_time": true})
	if c := a.json("POST", "/app/api/replay/start", body, &snap); c != 202 {
		t.Fatalf("start: %d %v", c, snap)
	}
	done := a.waitReplay()
	a.settle()
	if done["state"] != "done" || done["planned"].(float64) != 31 || done["source"] != "file my plant.csv" || done["rejected"].(float64) != 1 {
		t.Fatalf("run: %v", done)
	}
	if e, _ := a.stats(); e != 30 {
		t.Fatalf("30 valid rows should be stored (the out-of-range one is rejected), got %v", e)
	}

	// Removing the file: a replay that asks for it is refused with a clear reason.
	if c, _ := a.do("DELETE", "/app/api/replay/dataset", nil); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	var out map[string]any
	if c := a.json("POST", "/app/api/replay/start", body, &out); c != 400 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "no file has been imported") {
		t.Fatalf("replay without a file: %d %v", c, out)
	}
}

func TestBadFilesAreRefusedWithAReason(t *testing.T) {
	a := newApp(t, "")
	for name, tc := range map[string]struct{ body, want string }{
		"empty":    {"", "empty"},
		"not json": {"{\"a\":1}\nnope\n", "line 2"},
	} {
		code, raw := a.putRaw("/app/api/replay/dataset", "text/plain", []byte(tc.body))
		if code != 400 || !strings.Contains(string(raw), tc.want) || !strings.Contains(string(raw), "invalid_dataset") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
	var st struct {
		Dataset *struct{} `json:"dataset"`
	}
	a.json("GET", "/app/api/state", nil, &st)
	if st.Dataset != nil {
		t.Fatal("a refused file must not replace or create the imported file")
	}
	// Importing needs the token like everything else.
	req, _ := http.NewRequest("PUT", a.base+"/app/api/replay/dataset", strings.NewReader("x"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("without a token: %d", resp.StatusCode)
	}
}

func pad2(n int) string {
	if n < 10 {
		return "0" + string(rune('0'+n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

// A file whose columns are not recognised is not "invalid": the reply lists its columns so the panel can ask
// which one is which, and importing again with that choice works.
func TestFileWithOtherColumnNamesCanBeMapped(t *testing.T) {
	a := newApp(t, "")
	csv := "Zeit,Maschine,Grad,Schwingung\n2025-01-15T08:00:00Z,press-01,61.5,2.1\n2025-01-15T08:00:02Z,press-01,62,2.2\n"
	code, raw := a.putRaw("/app/api/replay/dataset?name=werk.csv", "text/csv", []byte(csv))
	var need struct {
		Error struct {
			Code    string   `json:"code"`
			Missing []string `json:"missing"`
			Columns []string `json:"columns"`
			Fields  []string `json:"fields"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &need); err != nil || code != 400 || need.Error.Code != "needs_mapping" {
		t.Fatalf("unrecognised columns: %d %s", code, raw)
	}
	if len(need.Error.Missing) != 4 || len(need.Error.Columns) != 4 || len(need.Error.Fields) == 0 {
		t.Fatalf("the reply must list the missing fields, the file's columns and the schema fields: %s", raw)
	}
	var st struct {
		Dataset *struct{} `json:"dataset"`
	}
	a.json("GET", "/app/api/state", nil, &st)
	if st.Dataset != nil {
		t.Fatal("a file that needs mapping must not be imported yet")
	}

	choice := `{"Zeit":"event_time","Maschine":"device_id","Grad":"temperature_c","Schwingung":"vibration_mm_s"}`
	code, raw = a.putRaw("/app/api/replay/dataset?name=werk.csv&map="+url.QueryEscape(choice), "text/csv", []byte(csv))
	var ok struct {
		Dataset struct {
			Rows   int               `json:"rows"`
			Mapped map[string]string `json:"mapped"`
			Fields map[string]string `json:"fields"`
		} `json:"dataset"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil || code != 200 || ok.Dataset.Rows != 2 {
		t.Fatalf("with the user's choice: %d %s", code, raw)
	}
	if ok.Dataset.Fields["temperature_c"] != "Grad" || ok.Dataset.Mapped["Zeit"] != "event_time" {
		t.Fatalf("the summary must say which column feeds which field: %s", raw)
	}
	for name, bad := range map[string]string{
		"unknown column": `{"Nope":"event_time"}`,
		"unknown field":  `{"Zeit":"colour"}`,
		"two for one":    `{"Zeit":"event_time","Maschine":"event_time"}`,
		"not an object":  `[1]`,
	} {
		code, raw = a.putRaw("/app/api/replay/dataset?map="+url.QueryEscape(bad), "text/csv", []byte(csv))
		if code != 400 || !strings.Contains(string(raw), "invalid_dataset") {
			t.Errorf("%s: %d %s", name, code, raw)
		}
	}
}

// The panel's address changes on every launch, so its preferences are kept by the engine in the data folder.
func TestInterfacePreferencesPersistInTheDataFolder(t *testing.T) {
	a := newApp(t, "")
	var out struct {
		Prefs Prefs `json:"prefs"`
	}
	if c := a.json("GET", "/app/api/prefs", nil, &out); c != 200 || out.Prefs.Theme != "auto" || out.Prefs.WelcomeSeen {
		t.Fatalf("defaults: %d %+v", c, out)
	}
	if c := a.json("PUT", "/app/api/prefs", map[string]any{"theme": "dark"}, &out); c != 200 || out.Prefs.Theme != "dark" {
		t.Fatalf("set theme: %d %+v", c, out)
	}
	if c := a.json("PUT", "/app/api/prefs", map[string]any{"welcome_seen": true}, &out); c != 200 || out.Prefs.Theme != "dark" || !out.Prefs.WelcomeSeen {
		t.Fatalf("a change must keep the other value: %d %+v", c, out)
	}
	if got := LoadPrefs(a.dir); got.Theme != "dark" || !got.WelcomeSeen {
		t.Fatalf("not on disk: %+v", got)
	}
	for name, body := range map[string]any{"bad theme": map[string]any{"theme": "purple"}, "unknown field": map[string]any{"colour": "red"}} {
		if c := a.json("PUT", "/app/api/prefs", body, nil); c != 400 {
			t.Errorf("%s: %d", name, c)
		}
	}
}
