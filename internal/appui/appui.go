// Package appui embeds the control panel served by the desktop app (and by `signallab app`).
// It is plain HTML, CSS and JavaScript with no build step and no third-party code.
package appui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui
var files embed.FS

// Handler serves the panel with a strict content security policy: only the app's own scripts and
// styles run, and the page may only talk to its own origin.
func Handler() http.Handler {
	sub, err := fs.Sub(files, "ui")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	fileServer := http.FileServerFS(sub)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})
}
