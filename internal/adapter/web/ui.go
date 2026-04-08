package web

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distFS embed.FS

// registerPlaygroundRoutes adds the env-config.js and SPA catch-all routes to mux.
// Must be called after all API routes are registered.
func registerPlaygroundRoutes(mux *http.ServeMux) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		log.Printf("[Web] Playground: failed to open dist FS: %v", err)
		return
	}

	// Verify that the assets are actually present (not just the .gitkeep placeholder)
	if _, err := sub.Open("index.html"); err != nil {
		log.Printf("[Web] Playground: index.html not found in dist — playground UI not available (build with Docker to include assets)")
		return
	}

	// env-config.js — served dynamically so API_URL is always same-origin ("")
	mux.HandleFunc("GET /env-config.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprint(w, `window.__ENV__ = { API_URL: "" };`)
	})

	// Catch-all SPA handler — serves static assets directly, falls back to index.html
	mux.Handle("/", spaHandler(sub))

	log.Printf("[Web] Playground UI enabled at /")
}

// spaHandler returns an http.Handler that serves files from dist and falls back
// to index.html for any path that does not match a real file (client-side routing).
func spaHandler(dist fs.FS) http.Handler {
	fileServer := http.FileServer(http.FS(dist))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := path.Clean(strings.TrimPrefix(r.URL.Path, "/"))
		if name == "." {
			name = "index.html"
		}
		if _, err := dist.Open(name); err != nil {
			// File not found — rewrite to root so the SPA handles the route
			r = r.Clone(r.Context())
			r.URL.Path = "/"
		}
		fileServer.ServeHTTP(w, r)
	})
}
