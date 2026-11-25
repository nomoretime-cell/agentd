package main

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// Workbench pages are addressed by URL fragment (/#knowledge), so Go serves
// index.html only at "/" and never treats other paths as page routes.
func TestWorkbenchStaticEntry(t *testing.T) {
	old := workbenchFiles
	t.Cleanup(func() { workbenchFiles = old })
	workbenchFiles = fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	api := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(299) })
	h := withWorkbench(api)
	for _, c := range []struct {
		path string
		want int
	}{
		{"/", 200},
		{"/assets/app.js", 200},
		{"/assets/missing.js", 404},
		{"/knowledge", 404},
		{"/healthz", 299},
		{"/v1/status", 299},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", c.path, nil))
		if rec.Code != c.want {
			t.Errorf("GET %s = %d, want %d", c.path, rec.Code, c.want)
		}
	}
}

// The page's inline theme script runs before first paint; the policy must
// allow exactly that script, and hashed assets are cached for good.
func TestWorkbenchHeaders(t *testing.T) {
	old := workbenchFiles
	t.Cleanup(func() { workbenchFiles = old })
	script := "document.documentElement.classList.remove(\"dark\")"
	workbenchFiles = fstest.MapFS{
		"index.html":    {Data: []byte("<head><script>" + script + "</script><script type=\"module\" src=\"/assets/a.js\"></script></head>")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
	h := withWorkbench(http.NotFoundHandler())
	sum := sha256.Sum256([]byte(script))
	want := "script-src 'self' 'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "';"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, want) {
		t.Fatalf("CSP %q lacks %q", csp, want)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/assets/app.js", nil))
	if rec.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("asset headers %v", rec.Header())
	}
}
