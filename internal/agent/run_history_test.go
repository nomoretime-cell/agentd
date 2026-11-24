package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistoryPaginationAndReceipts(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	dir := filepath.Join(s.root, "data/sessions")
	var lines bytes.Buffer
	for i := 0; i < 212; i++ {
		fmt.Fprintf(&lines, "{\"type\":\"run\",\"status\":\"done\",\"conversation\":\"local\",\"inputs\":[{\"event\":%d,\"text\":\"旧记录\"}]}\n", i)
	}
	day := "2026-10-12"
	file := filepath.Join(dir, day+".jsonl")
	if err := os.WriteFile(file, lines.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	older := map[string]any{"type": "run", "answer": "摘要 " + s.Token(), "notifications": []map[string]string{{"key": "notice", "target": "local", "content": "归档"}}}
	raw, _ := json.Marshal(older)
	if err := os.WriteFile(filepath.Join(dir, "2026-10-11.jsonl"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('notice','local','归档',0,'sent')"); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"to": {day}}
	ids := map[string]bool{}
	var oldest string
	for i := 0; i < 20; i++ {
		page, err := s.runHistory(t.Context(), q)
		if err != nil || page.Incomplete {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, r := range page.Items {
			if ids[r.ID] {
				t.Fatal("duplicate", r.ID)
			}
			ids[r.ID] = true
			oldest = r.ID
		}
		if i == 0 {
			f, err := os.OpenFile(file, os.O_APPEND|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.WriteString("{\"type\":\"run\",\"answer\":\"new arrival\"}\n")
			f.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if page.Next == "" {
			break
		}
		q.Set("cursor", page.Next)
	}
	if len(ids) != 213 || oldest != "2026-10-11.0" {
		t.Fatalf("got %d records, oldest %s", len(ids), oldest)
	}
	r, err := s.historicalRun(t.Context(), oldest)
	if err != nil || r.Answer != "摘要 [redacted]" || r.Role != "" || len(r.Notifications) != 1 || r.Notifications[0].DeliveryStatus != "sent" {
		t.Fatal("missing redaction, legacy scope or authoritative receipt", err)
	}
	q = url.Values{"q": {"旧记录"}, "to": {day}, "conversation": {"local"}, "status": {"failed"}}
	page, err := s.runHistory(t.Context(), q)
	if err != nil || len(page.Items) != 0 {
		t.Fatal("status filter")
	}
	q.Set("status", "done")
	page, err = s.runHistory(t.Context(), q)
	if err != nil || len(page.Items) != 20 || page.Items[0].Excerpt != "旧记录" {
		t.Fatal("text and conversation filters")
	}
	for _, path := range []string{"/v1/runs", "/v1/runs/" + oldest} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal(w.Code)
		}
	}
}

func TestHistoryScanBoundAndRecordBoundary(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	day := "2026-11-01"
	early := "{\"type\":\"run\",\"answer\":\"earlier\"}\n"
	data := early + strings.Repeat("x", historyWindow+300) + "\n{\"type\":\"run\",\"answer\":\"later\"}\n{\"type\":"
	if err := os.WriteFile(filepath.Join(s.root, "data/sessions", day+".jsonl"), []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	q := url.Values{"to": {day}, "from": {day}}
	var excerpts []string
	incomplete := false
	for i := 0; i < 5; i++ {
		page, err := s.runHistory(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range page.Items {
			excerpts = append(excerpts, r.Excerpt)
		}
		incomplete = incomplete || page.Incomplete
		if page.Next == "" {
			break
		}
		q.Set("cursor", page.Next)
	}
	if !incomplete || strings.Join(excerpts, ",") != "later,earlier" {
		t.Fatalf("scan: %v, incomplete %t", excerpts, incomplete)
	}
	for _, bad := range []string{"../api-token.0", day + ".1", day + ".-1"} {
		if _, err := s.historicalRun(t.Context(), bad); err == nil {
			t.Fatal("accepted invalid position")
		}
	}
}

func TestRunTimeAcrossClockRollback(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	for input, want := range map[string]string{
		"2026-11-01T05:30:00Z": "2026-11-01T01:30:00-04:00",
		"2026-11-01T06:30:00Z": "2026-11-01T01:30:00-05:00",
		"2026-11-01":           "2026-11-01", // Older transcripts may omit the time.
	} {
		if got := serverTimestamp(input, loc); got != want {
			t.Errorf("serverTimestamp(%q) = %q, want %q", input, got, want)
		}
	}
}
