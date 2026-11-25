package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestActivityMigrationAndConfirmation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = old.Exec(schema + `INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES
 ('ai-news:old','local','old',1,'sent'),('ai-news:2:part:1','local','one',1,'pending'),
 ('ai-news:2:part:2','local','two',1,'unknown'),('ai-news:test:3','local','test',1,'unknown');`)
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	ctx := t.Context()
	if err = s.deliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{3, 4} {
		if err = s.ResolveDelivery(ctx, id, "sent"); err != nil {
			t.Fatal(err)
		}
	}
	if err = s.ResolveDelivery(ctx, 3, "sent"); err != ErrConflict {
		t.Fatal("duplicate confirmation accepted")
	}
	now := time.Now().UTC()
	day := now.Format(time.DateOnly)
	activity, err := s.dailyActivity(ctx, 7, now)
	if err != nil || activity.UndatedNews != 1 || activity.Days[6].News != 2 || activity.Days[5].Coverage != "none" {
		t.Fatalf("daily overview %+v, %v", activity, err)
	}
	page, err := s.activityDetails(ctx, "news", url.Values{"date": {day}})
	if err != nil || len(page.Items) != 2 || page.Items[0].Status != "adapter" || page.Items[1].Status != "owner" {
		t.Fatalf("news detail %+v, %v", page, err)
	}
	since := s.metricsSince
	s.Close()
	s, err = Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	if !s.metricsSince.Equal(since) || count(t, s, "SELECT count(*) FROM outbox WHERE sent_at IS NOT NULL") != 3 {
		t.Fatal("coverage or receipts lost on restart")
	}
}

func TestActivityAcrossMidnightAndRepeatedReceipts(t *testing.T) {
	cfg := testConfig()
	cfg.Timezone = "Asia/Shanghai"
	s := openTest(t, cfg, nil)
	ctx := t.Context()
	loc, _ := time.LoadLocation(cfg.Timezone)
	midnight := time.Date(2026, 7, 2, 0, 0, 0, 0, loc)
	s.metricsSince = midnight.Add(-4 * time.Hour)
	id, _, err := s.Append(ctx, "event", Payload{Kind: "message", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	scoped := context.WithValue(ctx, callScopeKey{}, callScope{id, "attempt", "message"})
	c := s.startCall(scoped, "native", "example")
	c.At = midnight.Add(-time.Second).UnixMilli()
	sum := int64(10)
	c.Total = &sum
	for range 2 {
		if err = s.storeCall(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	c.ID = randomID()
	c.At = midnight.UnixMilli()
	c.Total = nil
	if err = s.storeCall(ctx, c); err != nil {
		t.Fatal(err)
	}
	c.ID = randomID()
	sum2 := int64(15)
	c.Total = &sum2
	if err = s.storeCall(ctx, c); err != nil {
		t.Fatal(err)
	}
	for i, key := range []string{"ai-news:1:part:1", "ai-news:1:part:2"} {
		_, err = s.db.Exec("INSERT INTO outbox(dedup_key,target,content,status,deliver_at,sent_at,confirmed_by) VALUES(?,'local','body','sent',0,?,'adapter')", key, midnight.Add(time.Duration(i-1)*time.Second).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	a, err := s.dailyActivity(ctx, 7, midnight)
	if err != nil {
		t.Fatal(err)
	}
	prev, today := a.Days[5], a.Days[6]
	if prev.Tokens != 10 || prev.Calls != 1 || prev.News != 1 || today.Tokens != 15 || today.Calls != 2 || today.Reported != 1 || today.News != 1 {
		t.Fatalf("%+v %+v", prev, today)
	}
	rows, err := s.activityDetails(ctx, "tokens", url.Values{"date": {today.Date}})
	if err != nil || len(rows.Items) != today.Calls {
		t.Fatalf("%+v %v", rows, err)
	}
}
func TestCommittedRepliesAndToolSteps(t *testing.T) {
	s := openTest(t, testConfig(), notifyModel())
	ctx := t.Context()
	id, _, err := s.Append(ctx, "original", Payload{Kind: "message", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE reply_at IS NOT NULL") == 1 })
	list, err := s.dailyActivity(ctx, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	day := list.Days[6]
	if day.Replies != 1 || day.Calls != 2 || day.Reported != 0 {
		t.Fatalf("%+v", day)
	}
	p, err := s.activityDetails(ctx, "replies", url.Values{"date": {day.Date}})
	if err != nil || len(p.Items) != 1 || p.Items[0].Event != id || p.Items[0].Body != "Complete." || p.Items[0].Attempt == "" {
		t.Fatalf("%+v %v", p, err)
	}
	listID, inserted, err := s.Append(ctx, "original", Payload{Kind: "message", Text: "hello"})
	if err != nil || inserted || listID != id {
		t.Fatal("event replay")
	}
	if count(t, s, "SELECT count(*) FROM inbox WHERE reply_at IS NOT NULL") != 1 {
		t.Fatal("duplicate reply")
	}
}

func TestOpenCodeStepUsageReceipts(t *testing.T) {
	cfg := testConfig()
	cfg.OpenCodeBinary = filepath.Join(t.TempDir(), "executor")
	// Simulate the CLI stream, including a repeated receipt and a cached step
	// whose total is omitted.
	script := `#!/bin/sh
while [ "$1" != "--session" ]; do shift; done
sid=$2
cat >/dev/null
cat <<FRAMES
{"type":"step_start","sessionID":"$sid","part":{"id":"s1"}}
{"type":"tool_use","sessionID":"$sid","part":{"id":"tool-1","tool":"lookup","state":{"status":"completed","input":{"query":"sample"},"output":"fixture-result"}}}
{"type":"step_finish","sessionID":"$sid","part":{"id":"f1","reason":"tool-calls","tokens":{"input":2,"output":3,"total":12}}}
{"type":"step_finish","sessionID":"$sid","part":{"id":"f1","reason":"tool-calls","tokens":{"total":12}}}
{"type":"step_start","sessionID":"$sid","part":{"id":"s2"}}
{"type":"text","sessionID":"$sid","part":{"text":"ok"}}
{"type":"step_finish","sessionID":"$sid","part":{"id":"f2","reason":"stop","tokens":{"input":2,"output":1,"reasoning":3,"cache":{"read":4,"write":5}}}}
FRAMES
`
	if err := os.WriteFile(cfg.OpenCodeBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s := openTest(t, cfg, nil)
	if _, _, err := s.Append(t.Context(), "oc", Payload{Kind: "message", Text: "test"}); err != nil {
		t.Fatal(err)
	}
	s.Start(t.Context())
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 1 })
	a, err := s.dailyActivity(t.Context(), 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if d := a.Days[6]; d.Calls != 2 || d.Reported != 2 || d.Tokens != 27 || d.Replies != 1 {
		t.Fatalf("unexpected activity: %+v", d)
	}
	page, err := s.runHistory(t.Context(), url.Values{})
	if err != nil || len(page.Items) != 1 {
		t.Fatal("OpenCode index", err)
	}
	rec, err := s.historicalRun(t.Context(), page.Items[0].ID)
	if err != nil || rec.Evidence == nil {
		t.Fatal("OpenCode context", err)
	}
	var stdin, reported bool
	for _, src := range rec.Evidence.Sources {
		stdin = stdin || src.Name == "stdin" && strings.Contains(src.Text, "test")
		reported = reported || src.Kind == "executor" && strings.Contains(src.Text, "fixture-result") && strings.Contains(src.Text, "sample")
	}
	if !stdin || !reported {
		t.Fatal("executor stdin or reported tool receipt missing")
	}

}

func TestUsageFormatDoesNotChangeValidAnswer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"received"}}],"usage":{"total_tokens":"unavailable"}}`)
	}))
	defer server.Close()
	m := NewHTTPModel(ModelConfig{Endpoint: server.URL, Name: "test", TimeoutSeconds: 2})
	answer, err := m.Complete(t.Context(), nil, nil)
	if err != nil || answer.Content != "received" || answer.usage.Total != nil {
		t.Fatalf("invalid optional telemetry changed completion: %v", err)
	}
}

// The call ledger only measures. When it cannot be written the run still
// completes, consumes no retry and does not pause the service.
func TestCallLedgerFailureKeepsRunning(t *testing.T) {
	var calls atomic.Int32
	s := openTest(t, testConfig(), modelFunc(func(context.Context, []Message, []Tool) (Message, error) {
		calls.Add(1)
		return Message{Role: "assistant", Content: "Complete."}, nil
	}))
	if _, err := s.db.Exec(`CREATE TRIGGER fail_ledger BEFORE UPDATE OF model_calls ON inbox BEGIN SELECT RAISE(ABORT,'unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	s.Start(context.Background())
	id, _, err := s.Append(context.Background(), "ledger", Payload{Kind: "message", Text: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM inbox WHERE id=? AND status='done'", id) == 1
	})
	if calls.Load() != 1 || count(t, s, "SELECT retries FROM inbox WHERE id=?", id) != 0 || fileExists(filepath.Join(s.root, "data/paused.lock")) {
		t.Fatalf("calls=%d retries=%d", calls.Load(), count(t, s, "SELECT retries FROM inbox WHERE id=?", id))
	}
}

// A database from before the workbench carries inbox rows in every state.
// Opening it migrates in place, recovers interrupted rows without inventing
// run history, and later runs are recorded once.
func TestSchemaV1InboxRowsMigrate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "data"), 0700); err != nil {
		t.Fatal(err)
	}
	old, err := sql.Open("sqlite", filepath.Join(root, "data", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	// Rows use the stored formats: external dedup keys and Payload bytes.
	msg := func(text string) string { b, _ := json.Marshal(Payload{Kind: "message", Text: text}); return string(b) }
	_, err = old.Exec(schema+`INSERT INTO inbox(id,dedup_key,payload,status,retries,available_at) VALUES
 (1,'external:a',?,'running',0,1),(2,'external:b',?,'running',2,1),(3,'external:c',?,'dead',3,1),(4,'external:d',?,'done',0,1),(5,'external:e',?,'pending',0,1);
 INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('reply:4','local','old reply',1,'sent');`,
		msg("a"), msg("b"), msg("c"), msg("d"), msg("e"))
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := t.Context()
	var version int
	if err = s.read.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 4 {
		t.Fatalf("schema version %d, %v", version, err)
	}
	if count(t, s, "SELECT count(*) FROM inbox WHERE run_index IS NOT NULL OR model_calls IS NOT NULL OR run_data IS NOT NULL") != 0 {
		t.Fatal("migration invented run history")
	}
	for id, status := range map[int]string{1: "pending", 2: "dead", 3: "dead", 4: "done", 5: "pending"} {
		if count(t, s, "SELECT count(*) FROM inbox WHERE id=? AND status=?", id, status) != 1 {
			t.Errorf("row %d not %s after recovery", id, status)
		}
	}
	if _, err = s.dailyActivity(ctx, 7, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.runHistory(ctx, url.Values{}); err != nil {
		t.Fatal(err)
	}
	// Replaying an old event with the same bytes finds it rather than conflicting.
	if id, inserted, err := s.Append(ctx, "e", Payload{Kind: "message", Text: "e"}); err != nil || inserted || id != 5 {
		t.Fatalf("replay of old event: %d %v %v", id, inserted, err)
	}
	// Dead heads block the queue until the owner resolves them.
	if err = s.ResolveEvent(ctx, 2, "skip"); err != nil {
		t.Fatal(err)
	}
	if err = s.ResolveEvent(ctx, 3, "retry"); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM inbox WHERE id IN (1,3,5) AND status='done'") == 3
	})
	if count(t, s, "SELECT count(*) FROM inbox i, json_each(i.run_index) j WHERE i.id=3 AND j.key NOT LIKE '\\_%' ESCAPE '\\'") != 1 ||
		count(t, s, "SELECT count(*) FROM inbox WHERE id=3 AND reply_at IS NOT NULL AND model_calls IS NOT NULL") != 1 {
		t.Fatal("retried old event not recorded once")
	}
}
