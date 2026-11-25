package agent

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkbenchCountsOutsideRecentWindow(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	_, err := s.db.Exec(`INSERT INTO inbox(dedup_key,payload,available_at,status) VALUES ('old','{"kind":"message","text":"past"}',0,'dead')`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.db.Exec(`WITH RECURSIVE n(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM n WHERE x<210)
		INSERT INTO inbox(dedup_key,payload,available_at,status) SELECT x,'{"kind":"message"}',0,'done' FROM n`)
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.workItems(t.Context(), "inbox", "0")
	if err != nil || page.Counts["inbox"] != 1 || len(page.Items) != 1 || page.Items[0].Detail != "past" {
		t.Fatalf("%+v, %v", page, err)
	}
}

func TestWorkbenchOrigin(t *testing.T) {
	cfg := testConfig()
	cfg.WebOrigin = "https://workbench.example"
	s := openTest(t, cfg, nil)
	for _, tc := range []struct {
		origin string
		auth   bool
		want   int
	}{
		{"", true, 200}, {"http://" + cfg.Listen, true, 200}, {cfg.WebOrigin, true, 200}, {cfg.WebOrigin, false, 401},
		{"https://workbench.example.invalid", true, 403}, {"null", true, 403},
	} {
		req := httptest.NewRequest("GET", "/v1/workbench?kind=actions", nil)
		req.Header.Set("Origin", tc.origin)
		if tc.auth {
			req.Header.Set("Authorization", "Bearer "+s.Token())
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Fatalf("origin %q: got %d want %d", tc.origin, rec.Code, tc.want)
		}
	}
}

func TestDirectWorkbenchAccess(t *testing.T) {
	cfg := testConfig()
	cfg.WebOrigin = "http://192.0.2.10"
	s := openTest(t, cfg, nil)
	handler := s.Handler()
	for _, tc := range []struct {
		method, path, host, origin, site string
		want                             int
	}{
		{"GET", "/v1/status", cfg.Listen, "", "same-origin", 200},
		{"GET", "/v1/status", "192.0.2.10", "", "", 200},
		{"GET", "/v1/status", "other.invalid", "", "", 403},
		{"GET", "/v1/status", cfg.Listen, "", "cross-site", 403},
		{"POST", "/v1/chats", cfg.Listen, cfg.WebOrigin, "same-origin", 200},
		{"POST", "/v1/chats", cfg.Listen, "", "", 403},
		{"POST", "/v1/chats", cfg.Listen, "http://192.0.2.11", "", 403},
		{"POST", "/v1/control", cfg.Listen, cfg.WebOrigin, "", 404},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Host = tc.host
		req.Header.Set("Origin", tc.origin)
		req.Header.Set("Sec-Fetch-Site", tc.site)
		req.Header.Set("X-Agentd-Workbench", "1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s %s host %q origin %q site %q: got %d want %d", tc.method, tc.path, tc.host, tc.origin, tc.site, rec.Code, tc.want)
		}
	}
	// Credentials added by an authenticating reverse proxy keep the workbench
	// path and its route limits; they never open the management API.
	for _, tc := range []struct {
		method, path string
		workbench    bool
		want         int
	}{
		{"GET", "/v1/status", true, 200},
		{"POST", "/v1/control", true, 404},
		{"GET", "/v1/status", false, 401},
	} {
		req := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		req.Host = cfg.Listen
		req.Header.Set("Origin", cfg.WebOrigin)
		req.Header.Set("Authorization", "Basic b3duZXI6c2VjcmV0")
		if tc.workbench {
			req.Header.Set("X-Agentd-Workbench", "1")
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("proxy credentials %s %s workbench=%v: got %d want %d", tc.method, tc.path, tc.workbench, rec.Code, tc.want)
		}
	}
}
