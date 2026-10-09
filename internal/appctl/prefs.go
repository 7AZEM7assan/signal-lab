package appctl

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// Prefs are the panel's small interface preferences. They live in the data folder (ui-prefs.json)
// because the panel's address changes on every launch (the engine picks a free port), and a browser
// keeps what a page stores per address, so the panel cannot remember them itself.
type Prefs struct {
	Theme       string `json:"theme"`        // "auto" (follow the system), "light" or "dark"
	WelcomeSeen bool   `json:"welcome_seen"` // the first-run welcome was dismissed
}

// DefaultPrefs follow the system appearance and show the welcome.
func DefaultPrefs() Prefs { return Prefs{Theme: "auto"} }

func validTheme(t string) bool { return t == "auto" || t == "light" || t == "dark" }

var prefsMu sync.Mutex

func prefsPath(dir string) string { return filepath.Join(dir, "ui-prefs.json") }

// LoadPrefs reads ui-prefs.json. A missing or unreadable file gives the defaults: losing a preference is
// never worth failing for.
func LoadPrefs(dir string) Prefs {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	p := DefaultPrefs()
	raw, err := os.ReadFile(prefsPath(dir))
	if err != nil {
		return p
	}
	var got Prefs
	if json.Unmarshal(raw, &got) != nil || !validTheme(got.Theme) {
		return p
	}
	return got
}

// SavePrefs writes ui-prefs.json atomically (a temporary file, then a rename).
func SavePrefs(dir string, p Prefs) error {
	prefsMu.Lock()
	defer prefsMu.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "ui-prefs-*.tmp")
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
	return os.Rename(tmp.Name(), prefsPath(dir))
}
