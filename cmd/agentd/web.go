package main

import (
	"io/fs"
	"net/http"
)

// A Go-only build serves the existing API. Release builds attach the Vite
// bundle at compile time, so deployment still needs just one executable.
var workbenchFiles fs.FS

func withWorkbench(api http.Handler) http.Handler {
	if workbenchFiles == nil {
		return api
	}
	mux := http.NewServeMux()
	files := http.FileServerFS(workbenchFiles)
	mux.Handle("GET /assets/{file}", files)
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		http.ServeFileFS(w, r, workbenchFiles, "index.html")
	})
	mux.Handle("/v1/", api)
	mux.Handle("/healthz", api)
	return mux
}
