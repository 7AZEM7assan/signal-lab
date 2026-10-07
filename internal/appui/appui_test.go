package appui

import (
	"net/http"
	"net/http/httptest"
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
