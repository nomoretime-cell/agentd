package main

import (
	"crypto/sha256"
	"encoding/base64"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
)

// A Go-only build serves the existing API. Release builds attach the Vite
// bundle at compile time, so deployment still needs just one executable.
var workbenchFiles fs.FS

// inlineScript matches classic scripts without a src attribute.
var inlineScript = regexp.MustCompile(`(?s)<script>(.*?)</script>`)

// pageCSP allows exactly the inline scripts the page ships, by hash, so the
// policy cannot drift from index.html.
func pageCSP(index []byte) string {
	scripts := []string{"'self'"}
	for _, m := range inlineScript.FindAllSubmatch(index, -1) {
		sum := sha256.Sum256(m[1])
		scripts = append(scripts, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return "default-src 'self'; script-src " + strings.Join(scripts, " ") + "; style-src 'self' 'unsafe-inline'; img-src 'self' data:; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"
}

func withWorkbench(api http.Handler) http.Handler {
	if workbenchFiles == nil {
		return api
	}
	index, err := fs.ReadFile(workbenchFiles, "index.html")
	if err != nil {
		panic(err)
	}
	csp := pageCSP(index)
	mux := http.NewServeMux()
	files := http.FileServerFS(workbenchFiles)
	// Asset names carry a content hash, so they never change once published.
	mux.HandleFunc("GET /assets/{file}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		files.ServeHTTP(w, r)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", csp)
		http.ServeFileFS(w, r, workbenchFiles, "index.html")
	})
	mux.Handle("/v1/", api)
	mux.Handle("/healthz", api)
	return mux
}
