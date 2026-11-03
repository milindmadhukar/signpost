package main

import "net/http"

// The dashboard installs as an app. Both files sit at the root, outside
// signpost's own auth check, because they hold nothing secret; Authelia
// still guards them at Traefik, and the browser sends its session cookie
// for both. The service worker has to live at /sw.js: a worker only
// controls the paths under its own URL, so /static/sw.js could not reach
// /links.

func (a *App) pwaRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /manifest.webmanifest", serveEmbedded("web/manifest.webmanifest", "application/manifest+json"))
	// Built from web/ts/sw.ts by esbuild (Makefile, Dockerfile). no-cache, so
	// a new build's worker is picked up on the next visit.
	mux.HandleFunc("GET /sw.js", serveEmbedded("web/static/sw.js", "text/javascript; charset=utf-8"))
}

func serveEmbedded(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := webFS.ReadFile(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(b)
	}
}
