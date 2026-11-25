package main

import (
	"net/http"
	"net/http/httptest"
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
