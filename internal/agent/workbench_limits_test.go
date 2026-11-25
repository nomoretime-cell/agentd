package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunEvidenceRetention(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	now := time.Now()
	old := now.AddDate(0, 0, -runEvidenceDays-1).UnixMilli()
	recent := now.AddDate(0, 0, -1).UnixMilli()
	add := func(key, status string, last int64) {
		t.Helper()
		index := fmt.Sprintf(`{"_first":%d,"_last":%d}`, last, last)
		if _, e := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at,status,run_index,run_data) VALUES(?,?,0,?,?,?)", key, `{"kind":"message","text":"x"}`, status, index, `{"a":{"sources":[]}}`); e != nil {
			t.Fatal(e)
		}
	}
	add("old-done", "done", old)
	add("recent-done", "done", recent)
	add("old-dead", "dead", old)
	kept := func() []string {
		t.Helper()
		rows, e := s.db.Query("SELECT dedup_key FROM inbox WHERE run_data IS NOT NULL ORDER BY dedup_key")
		if e != nil {
			t.Fatal(e)
		}
		defer rows.Close()
		var keys []string
		for rows.Next() {
			var k string
			if e = rows.Scan(&k); e != nil {
				t.Fatal(e)
			}
			keys = append(keys, k)
		}
		return keys
	}
	s.pruneRunEvidence(now)
	if got := strings.Join(kept(), ","); got != "old-dead,recent-done" {
		t.Fatalf("evidence kept for %s", got)
	}
	if count(t, s, "SELECT count(*) FROM inbox WHERE dedup_key='old-done' AND run_index IS NOT NULL") != 1 {
		t.Fatal("run index removed with its evidence")
	}
	// Pruning runs at most hourly.
	add("old-done-2", "done", old)
	s.pruneRunEvidence(now.Add(time.Minute))
	if len(kept()) != 3 {
		t.Fatal("pruned again within the hour")
	}
	s.pruneRunEvidence(now.Add(time.Hour))
	if len(kept()) != 2 {
		t.Fatal("hourly pruning skipped")
	}
}

func TestMemoryRevisionPruning(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	dir := filepath.Join(s.root, editDir)
	bodies := filepath.Join(s.root, editBodies)
	for _, d := range []string{dir, bodies} {
		if e := os.MkdirAll(d, 0700); e != nil {
			t.Fatal(e)
		}
	}
	write := func(n int) {
		t.Helper()
		r := memoryRevision{ID: fmt.Sprintf("20260101T000000.%09dZ-abcdef01", n), Previous: fmt.Sprintf("p%d", n), Version: fmt.Sprintf("v%d", n)}
		raw, _ := json.Marshal(r)
		if e := os.WriteFile(filepath.Join(dir, r.ID+".json"), raw, 0600); e != nil {
			t.Fatal(e)
		}
		for _, h := range []string{r.Previous, r.Version} {
			if e := os.WriteFile(filepath.Join(bodies, h), []byte(h), 0600); e != nil {
				t.Fatal(e)
			}
		}
	}
	limit := keepEdits + keepEdits/10
	for i := range limit {
		write(i)
	}
	if e := s.pruneEdits(); e != nil {
		t.Fatal(e)
	}
	if n, _ := os.ReadDir(dir); len(n) != limit {
		t.Fatalf("pruned below the slack: %d", len(n))
	}
	write(limit)
	if e := s.pruneEdits(); e != nil {
		t.Fatal(e)
	}
	names, _ := os.ReadDir(dir)
	left, _ := os.ReadDir(bodies)
	if len(names) != keepEdits || len(left) != 2*keepEdits {
		t.Fatalf("kept %d revisions and %d bodies", len(names), len(left))
	}
	if !strings.HasPrefix(names[0].Name(), fmt.Sprintf("20260101T000000.%09dZ", limit+1-keepEdits)) || fileExists(filepath.Join(bodies, "v0")) {
		t.Fatal("pruned the wrong revisions")
	}
}

func TestDamagedUploadReceiptIsSkipped(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, testConfig(), nil)
	if e != nil {
		t.Fatal(e)
	}
	good := filepath.Join(root, knowledgeUploads, stableID("good"))
	bad := filepath.Join(root, knowledgeUploads, stableID("bad"))
	for _, d := range []string{good, bad} {
		if e = os.MkdirAll(d, 0700); e != nil {
			t.Fatal(e)
		}
	}
	if e = saveUpload(good, uploadBatch{ID: filepath.Base(good), Files: []uploadFile{{uploadChoice: uploadChoice{Name: "a.md", Mode: "match"}, Status: "added"}}}); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(bad, "receipt.json"), []byte("{"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, testConfig(), nil)
	if e != nil {
		t.Fatal("damaged receipt stopped the service:", e)
	}
	defer s.Close()
	req := httptest.NewRequest("GET", "/v1/knowledge/uploads", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), filepath.Base(good)) {
		t.Fatalf("list %d: %s", w.Code, w.Body.String())
	}
	if !fileExists(filepath.Join(bad, "receipt.json")) {
		t.Fatal("damaged batch removed")
	}
}

func TestUploadRetryDoesNotWaitForImport(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	id := stableID("retry")
	data := "# retry"
	choice := uploadChoice{Name: "retry.md", Mode: "match"}
	dir := filepath.Join(s.root, knowledgeUploads, id)
	if e := os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e := saveUpload(dir, uploadBatch{ID: id, Files: []uploadFile{{uploadChoice: choice, SHA: digest([]byte(data)), Status: "processing"}}}); e != nil {
		t.Fatal(e)
	}
	// The accepted batch is still importing.
	if e := s.beginImport(); e != nil {
		t.Fatal(e)
	}
	defer s.finishImport(nil)
	post := func(key, content string) int {
		body, typ := uploadPayload(t, []uploadChoice{choice}, content)
		req := httptest.NewRequest("POST", "/v1/knowledge/uploads?key="+key, bytes.NewReader(body))
		req.Host = s.cfg.Listen
		req.Header.Set("Content-Type", typ)
		req.Header.Set("Origin", "http://"+req.Host)
		req.Header.Set("X-Agentd-Workbench", "1")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	if code := post(id, data); code != 200 {
		t.Fatalf("retry of accepted batch got %d", code)
	}
	if code := post(id, "# different"); code != 409 {
		t.Fatalf("changed retry got %d", code)
	}
	if code := post(stableID("new"), data); code != 409 {
		t.Fatalf("new batch during import got %d", code)
	}
}

func TestRespondStatusClasses(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{errors.New("invalid input"), 400},
		{fmt.Errorf("read: %w", fs.ErrNotExist), 404},
		{&fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}, 500},
		{fmt.Errorf("save: %w", &os.LinkError{Op: "rename", Old: "a", New: "b", Err: fs.ErrPermission}), 500},
		{ErrConflict, 409},
	} {
		w := httptest.NewRecorder()
		respond(w, nil, tc.err)
		if w.Code != tc.want {
			t.Errorf("%v: got %d want %d", tc.err, w.Code, tc.want)
		}
	}
	s := openTest(t, testConfig(), nil)
	_, e := s.read.Exec("SELECT * FROM missing_table")
	w := httptest.NewRecorder()
	respond(w, nil, e)
	if w.Code != 500 {
		t.Fatalf("database error got %d", w.Code)
	}
}

func TestNewsNoticeKeys(t *testing.T) {
	for key, want := range map[string]string{
		"ai-news:test:42":      "42 true true",
		"ai-news:7:since:1000": "7 false true",
		"reply:9":              " false false",
	} {
		ev, test, ok := newsNotice(key)
		if got := fmt.Sprint(ev, " ", test, " ", ok); got != want {
			t.Errorf("%s: got %q want %q", key, got, want)
		}
	}
	if noticeKind("ai-news:test:42") != "news-test" || noticeKind("ai-news:7:since:1") != "news" || noticeKind("reply:9") != "reply" {
		t.Fatal("notice kinds changed")
	}
}
