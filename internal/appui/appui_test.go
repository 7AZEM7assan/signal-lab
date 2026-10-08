package appui

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestServesPanelWithStrictCSP(t *testing.T) {
	h := Handler()
	for path, want := range map[string]string{"/": "<title>Signal Lab</title>", "/app.js": "use strict", "/app.css": ":root"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("%s: status %d, body missing %q", path, rec.Code, want)
		}
		csp := rec.Header().Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Fatalf("%s: unexpected CSP %q", path, csp)
		}
	}
}

func TestPanelHasNoInlineScriptsOrStyles(t *testing.T) {
	// The strict CSP blocks inline code, so the page must not rely on it.
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	html := rec.Body.String()
	if strings.Contains(html, "<style") || strings.Contains(html, " style=") || strings.Contains(html, " onclick=") {
		t.Fatal("index.html contains inline style or event handlers, which the CSP blocks")
	}
	for _, tag := range strings.Split(html, "<script")[1:] {
		if !strings.HasPrefix(strings.TrimSpace(tag), "src=") {
			t.Fatalf("inline <script> found: %.60s", tag)
		}
	}
}

func TestPanelIconMatchesTheAppIcon(t *testing.T) {
	// The panel's icon is a copy of the desktop app's icon source; they must not drift apart.
	panel, err := files.ReadFile("ui/icon.svg")
	if err != nil {
		t.Fatal(err)
	}
	app, err := os.ReadFile("../../desktop/build/icon.svg")
	if err != nil {
		t.Skipf("desktop sources not present: %v", err)
	}
	if !bytes.Equal(panel, app) {
		t.Fatal("internal/appui/ui/icon.svg differs from desktop/build/icon.svg; copy the app icon over")
	}
}

func TestPanelReferencesOnlyFilesItShips(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, ref := range []string{"app.css", "app.js", "icon.svg"} {
		if !strings.Contains(rec.Body.String(), ref) {
			t.Errorf("index.html does not reference %s", ref)
		}
		r := httptest.NewRecorder()
		Handler().ServeHTTP(r, httptest.NewRequest(http.MethodGet, "/"+ref, nil))
		if r.Code != 200 {
			t.Errorf("%s is referenced but not served (status %d)", ref, r.Code)
		}
	}
}
