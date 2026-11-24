package agent

import (
	"net/http/httptest"
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
