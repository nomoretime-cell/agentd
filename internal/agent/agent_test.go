package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type modelFunc func(context.Context, []Message, []Tool) (Message, error)

func (f modelFunc) Complete(c context.Context, m []Message, t []Tool) (Message, error) {
	return f(c, m, t)
}
func finalModel() Model {
	return modelFunc(func(context.Context, []Message, []Tool) (Message, error) {
		return Message{Role: "assistant", Content: "Complete."}, nil
	})
}
func notifyModel() Model {
	return modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		if m[len(m)-1].Role == "tool" {
			return Message{Role: "assistant", Content: "Complete."}, nil
		}
		return call("send_message", `{"content":"test notification"}`), nil
	})
}
func call(name, args string) Message {
	return Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "test-call", Type: "function", Function: FunctionCall{Name: name, Arguments: args}}}}
}
func testConfig() Config {
	c := DefaultConfig()
	c.Model.Name = "test"
	c.Limits.DebounceMS = 20
	c.Limits.PollMS = 10
	c.Limits.RetryBaseMS = 20
	c.Limits.RunSeconds = 5
	c.Limits.ToolSeconds = 2
	return c
}
func openTest(t *testing.T, c Config, m Model) *Service {
	t.Helper()
	s, e := Open(t.TempDir(), c, m)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := s.Close(); e != nil {
			t.Error(e)
		}
	})
	return s
}
func waitFor(t *testing.T, d time.Duration, fn func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not reached before deadline")
}
func count(t *testing.T, s *Service, q string, args ...any) int {
	t.Helper()
	var n int
	if e := s.read.QueryRow(q, args...).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func post(t *testing.T, server *httptest.Server, s *Service, path string, body any, code int) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", server.URL+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", "application/json")
	res, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	if res.StatusCode != code {
		t.Fatalf("%s: HTTP %d, want %d: %s", path, res.StatusCode, code, raw)
	}
	var out map[string]any
	if e = json.Unmarshal(raw, &out); e != nil {
		t.Fatal(e)
	}
	return out
}

func TestSchemaAndSingleInstance(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	n := 0
	for _, table := range []string{"inbox", "timers", "actions", "outbox"} {
		rows, e := s.read.Query("PRAGMA table_info(" + table + ")")
		if e != nil {
			t.Fatal(e)
		}
		for rows.Next() {
			n++
		}
		rows.Close()
	}
	if n != 37 {
		t.Fatalf("schema has %d columns", n)
	}
	if other, e := Open(s.root, testConfig(), finalModel()); e == nil {
		other.Close()
		t.Fatal("second instance accepted")
	}
	if _, e := s.read.Exec("DELETE FROM inbox"); e == nil {
		t.Fatal("reader accepted write")
	}
}
func TestInboxDedupFixedWindowAndFIFO(t *testing.T) {
	c := testConfig()
	c.Limits.DebounceMS = 1000
	s := openTest(t, c, finalModel())
	ctx := context.Background()
	var wg sync.WaitGroup
	var added atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, new, e := s.Append(ctx, "same", Payload{Kind: "message", Text: "hello"})
			if e != nil {
				t.Error(e)
			}
			if new {
				added.Add(1)
			}
		}()
	}
	wg.Wait()
	if added.Load() != 1 {
		t.Fatal("dedup failed")
	}
	id, _, e := s.Append(ctx, "second", Payload{Kind: "message", Text: "next"})
	if e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(DISTINCT available_at) FROM inbox") != 1 {
		t.Fatal("fixed window moved")
	}
	if _, _, e = s.Append(ctx, "same", Payload{Kind: "message", Text: "changed"}); !errors.Is(e, ErrConflict) {
		t.Fatal("payload conflict accepted")
	}
	s.db.Exec("UPDATE inbox SET available_at=0 WHERE id=?", id)
	s.mu.Lock()
	batch, e := s.claim(ctx)
	s.mu.Unlock()
	if e != nil || len(batch) != 0 {
		t.Fatal("later event bypassed backoff head", e)
	}
	s.db.Exec("UPDATE inbox SET status='dead' WHERE id=1")
	s.mu.Lock()
	batch, e = s.claim(ctx)
	s.mu.Unlock()
	if e != nil || len(batch) != 0 {
		t.Fatal("dead head bypassed")
	}
	if e = s.ResolveEvent(ctx, 1, "skip"); e != nil {
		t.Fatal(e)
	}
	s.mu.Lock()
	batch, e = s.claim(ctx)
	s.mu.Unlock()
	if e != nil || len(batch) != 1 || batch[0].ID != id {
		t.Fatal("explicit skip did not unblock", e)
	}
}
func TestTimerAtomicFireAndCron(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	ctx := context.Background()
	past := time.Now().Add(-time.Second).UnixMilli()
	for _, tm := range []Timer{{ID: "once", FireAt: past, Prompt: "one", Active: true}, {ID: "cron", Cron: "*/5 * * * *", FireAt: past, Prompt: "periodic", Active: true}} {
		if e := s.UpsertTimer(ctx, tm); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.fireTimers(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.fireTimers(ctx); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM inbox") != 2 {
		t.Fatal("timer generated duplicates")
	}
	if count(t, s, "SELECT count(*) FROM timers WHERE id='once' AND active=0") != 1 {
		t.Fatal("one-shot not disabled")
	}
	if count(t, s, "SELECT count(*) FROM timers WHERE id='cron' AND active=1 AND fire_at>?", time.Now().UnixMilli()) != 1 {
		t.Fatal("cron not advanced")
	}
	if _, e := s.schedule("not cron"); e == nil {
		t.Fatal("bad cron accepted")
	}
}
func TestHTTPInboxToDelivery(t *testing.T) {
	s := openTest(t, testConfig(), notifyModel())
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	s.Start(context.Background())
	res, e := server.Client().Get(server.URL + "/v1/status")
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("missing auth accepted")
	}
	payload := map[string]any{"key": "e2e-1", "text": "Notify me"}
	post(t, server, s, "/v1/events", payload, 200)
	post(t, server, s, "/v1/events", payload, 200)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'") == 1 })
	if count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") != 1 {
		t.Fatal("input not completed")
	}
	files, _ := os.ReadDir(filepath.Join(s.root, "data/deliveries"))
	if len(files) != 1 {
		t.Fatal("notification not delivered once")
	}
	post(t, server, s, "/v1/events", map[string]any{"key": "e2e-1", "text": "different"}, 409)
	post(t, server, s, "/v1/events", map[string]any{"key": "x", "text": "x", "unknown": true}, 400)
}
func TestApprovalEndToEndAndReplay(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret" || r.Header.Get("Idempotency-Key") == "" {
			t.Error("missing host credentials or stable key")
		}
		calls.Add(1)
		writeJSON(w, 200, map[string]string{"receipt": "ticket-1"})
	}))
	defer remote.Close()
	c := testConfig()
	c.Actions["ticket"] = Operation{Mode: "ask", URL: remote.URL, Method: "POST", BearerToken: "test-secret"}
	s := openTest(t, c, finalModel())
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	s.Start(context.Background())
	post(t, server, s, "/v1/action-requests", map[string]any{"key": "ask-1", "name": "ticket", "arguments": map[string]string{"title": "frozen"}, "text": "Create ticket then notify"}, 200)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM actions WHERE status='pending_approval'") == 1 })
	if calls.Load() != 0 {
		t.Fatal("action executed before approval")
	}
	var id string
	s.read.QueryRow("SELECT id FROM actions").Scan(&id)
	post(t, server, s, "/v1/actions/"+id+"/decision", map[string]string{"decision": "approve"}, 200)
	waitFor(t, 3*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM actions WHERE status='succeeded'") == 1 && count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 2
	})
	a, e := s.executeAction(context.Background(), id)
	if e != nil || a.Status != "succeeded" {
		t.Fatal(a, e)
	}
	if calls.Load() != 1 {
		t.Fatalf("remote action executed %d times", calls.Load())
	}
	if count(t, s, "SELECT count(*) FROM outbox WHERE dedup_key=? AND target=? AND content LIKE ?", "action-result:"+id, c.DefaultTarget, "%ticket-1%") != 1 {
		t.Fatal("missing local approval receipt after silent model completion")
	}
	if strings.Contains(a.Args, "test-secret") {
		t.Fatal("credential in action ledger")
	}
}
func TestActionUnknownAndIntentConflict(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, e := w.(http.Hijacker).Hijack()
		if e == nil {
			conn.Close()
		}
	}))
	defer remote.Close()
	c := testConfig()
	c.Actions["write"] = Operation{Mode: "allow", Method: "POST", URL: remote.URL}
	s := openTest(t, c, finalModel())
	text, _, e := s.namedOperation(context.Background(), "ev:1:1", "write", json.RawMessage(`{"x":1}`), "")
	if e != nil || !strings.Contains(text, "unknown") {
		t.Fatal(text, e)
	}
	_, _, e = s.namedOperation(context.Background(), "ev:1:1", "write", json.RawMessage(`{"x":1}`), "")
	if e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 {
		t.Fatal("unknown action resent")
	}
	if _, _, e = s.namedOperation(context.Background(), "ev:1:1", "write", json.RawMessage(`{"x":2}`), ""); !errors.Is(e, ErrConflict) {
		t.Fatal("changed replay accepted")
	}
	if _, _, e = s.namedOperation(context.Background(), "ev:2:1", "write", json.RawMessage(`{"x":1}`), ""); !errors.Is(e, ErrConflict) {
		t.Fatal("unknown bypassed under new event")
	}
	var id string
	s.read.QueryRow("SELECT id FROM actions").Scan(&id)
	if e = s.ReconcileAction(context.Background(), id, "succeeded", "operator verified remote ID 123"); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM inbox WHERE json_extract(payload,'$.kind')='approval'") != 1 {
		t.Fatal("reconciliation did not wake worker")
	}
}
func TestApprovalExpiryAndReject(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	ctx := context.Background()
	for _, id := range []string{"expired", "rejected"} {
		expires := time.Now().Add(time.Hour).UnixMilli()
		if id == "expired" {
			expires = 1
		}
		_, e := s.db.Exec("INSERT INTO actions VALUES(?,?,?,?,'','pending_approval',?,NULL)", id, id, "test", `{}`, expires)
		if e != nil {
			t.Fatal(e)
		}
	}
	if e := s.Decide(ctx, "rejected", "reject"); e != nil {
		t.Fatal(e)
	}
	if e := s.expireActions(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.expireActions(ctx); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM actions WHERE status='rejected'") != 2 || count(t, s, "SELECT count(*) FROM inbox") != 2 {
		t.Fatal("expiry/rejection not idempotent")
	}
}
func TestOutboxUnknownAndRateRetry(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}))
	defer remote.Close()
	c := testConfig()
	c.Targets["hook"] = Target{Kind: "webhook", URL: remote.URL}
	s := openTest(t, c, finalModel())
	ctx := context.Background()
	if e := transaction(ctx, s.db, func(tx *sql.Tx) error {
		return s.enqueueTx(ctx, tx, Notification{Key: "x", Target: "hook", Content: "hello"})
	}); e != nil {
		t.Fatal(e)
	}
	if e := s.deliverOne(ctx); e != nil {
		t.Fatal(e)
	}
	if e := s.deliverOne(ctx); e != nil {
		t.Fatal(e)
	}
	if calls.Load() != 1 || count(t, s, "SELECT count(*) FROM outbox WHERE status='unknown'") != 1 {
		t.Fatal("uncertain notification resent")
	}
	if e := s.ResolveDelivery(ctx, 1, "sent"); e != nil {
		t.Fatal(e)
	}
}
func TestAtomicOutcomeRollback(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	ctx := context.Background()
	id, _, _ := s.Append(ctx, "atomic", Payload{Kind: "message", Text: "x"})
	s.db.Exec("UPDATE inbox SET status='running' WHERE id=?", id)
	outcome := Outcome{Notifications: []Notification{{Key: "same", Target: "local", Content: "first"}, {Key: "same", Target: "local", Content: "conflict"}}}
	if e := s.commitBatch(ctx, []Event{{ID: id}}, outcome); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM outbox") != 0 || count(t, s, "SELECT count(*) FROM inbox WHERE status='running'") != 1 {
		t.Fatal("partial transaction committed")
	}
}
func TestPauseCancelNoFailureAndResume(t *testing.T) {
	entered := make(chan struct{}, 1)
	var calls atomic.Int32
	m := modelFunc(func(ctx context.Context, _ []Message, _ []Tool) (Message, error) {
		calls.Add(1)
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return Message{}, ctx.Err()
	})
	s := openTest(t, testConfig(), m)
	s.Start(context.Background())
	s.Append(context.Background(), "pause", Payload{Kind: "message", Text: "wait"})
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("run not started")
	}
	if e := s.SetPaused(true); e != nil {
		t.Fatal(e)
	}
	waitFor(t, time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM inbox WHERE status='pending' AND retries=0") == 1
	})
	time.Sleep(40 * time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("paused run restarted")
	}
	if !fileExists(filepath.Join(s.root, "data/paused.lock")) {
		t.Fatal("pause not persisted")
	}
}
func TestRecovery(t *testing.T) {
	root := t.TempDir()
	c := testConfig()
	s, e := Open(root, c, finalModel())
	if e != nil {
		t.Fatal(e)
	}
	s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at,status,retries) VALUES('a','{}',0,'running',2)")
	s.db.Exec("INSERT INTO actions VALUES('a','a','x','{}','','executing',0,NULL)")
	s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('n','local','x',0,'sending')")
	if e = s.SetPaused(true); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, c, finalModel())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	if !s.paused || count(t, s, "SELECT count(*) FROM inbox WHERE status='dead' AND retries=3") != 1 || count(t, s, "SELECT count(*) FROM actions WHERE status='unknown'") != 1 || count(t, s, "SELECT count(*) FROM outbox WHERE status='unknown'") != 1 {
		t.Fatal("unsafe recovery")
	}
}
func TestMemoryBoundariesAndSessionDamage(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	if e := s.workspaceWrite("notes/example.md", "memory", "absent"); e != nil {
		t.Fatal(e)
	}
	b, e := s.workspaceRead("notes/example.md")
	if e != nil || string(b) != "memory" {
		t.Fatal(e)
	}
	if e = s.workspaceWrite("notes/example.md", "overwrite", "wrong"); !errors.Is(e, ErrConflict) {
		t.Fatal("stale write accepted")
	}
	for _, path := range []string{"IDENTITY.md", "skills/x.md", "../data/api-token"} {
		if e = s.workspaceWrite(path, "bad", "absent"); e == nil {
			t.Fatal("protected write allowed", path)
		}
	}
	if e = os.Symlink(filepath.Join(s.root, "data/api-token"), filepath.Join(s.root, "workspace/notes/leak")); e != nil {
		t.Fatal(e)
	}
	if _, e = s.workspaceRead("notes/leak"); e == nil {
		t.Fatal("symlink escaped workspace")
	}
	os.WriteFile(filepath.Join(s.root, "data/session.jsonl"), []byte("{\"role\":\"user\",\"content\":\"valid\"}\n{broken"), 0600)
	if got := s.loadSession(); len(got) != 1 || got[0].Content != "valid" {
		t.Fatal(got)
	}
}
func TestQuietHours(t *testing.T) {
	c := testConfig()
	c.Timezone = "Asia/Shanghai"
	c.Quiet = QuietHours{Start: "23:00", End: "08:00"}
	s := openTest(t, c, finalModel())
	now, _ := time.Parse(time.RFC3339, "2026-11-17T23:30:00+08:00")
	want, _ := time.Parse(time.RFC3339, "2026-11-18T08:00:00+08:00")
	if !s.quietUntil(now).Equal(want) {
		t.Fatal(s.quietUntil(now))
	}
}
func TestSandboxIsolation(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS sandbox test; Linux requires bubblewrap")
	}
	s := openTest(t, testConfig(), finalModel())
	text, e := s.runShell(context.Background(), "sandbox-ok", "printf SANDBOX_OK; printf data > result.txt")
	if e != nil || !strings.Contains(text, "SANDBOX_OK") {
		t.Fatal(text, e)
	}
	text, e = s.runShell(context.Background(), "sandbox-secret", "cat "+filepath.Join(s.root, "data/api-token"))
	if e == nil || strings.Contains(text, s.Token()) {
		t.Fatal("sandbox read secret", text, e)
	}
	text, e = s.runShell(context.Background(), "sandbox-write", "printf bad > "+filepath.Join(s.root, "workspace/IDENTITY.md"))
	if e == nil {
		t.Fatal("sandbox wrote identity", text)
	}
}
func TestModelHTTPAndToolSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		// The tool budget is seven (AGENTS.md, 规模预算); a new tool needs a decision.
		if len(body["tools"].([]any)) != 7 {
			t.Error("expected seven tools")
		}
		fmt.Fprint(w, `{"choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"x","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"MEMORY.md\"}"}}]}}]}`)
	}))
	defer server.Close()
	m := NewHTTPModel(ModelConfig{Endpoint: server.URL, Name: "fake", MaxTokens: 128, TimeoutSeconds: 2})
	msg, e := m.Complete(context.Background(), []Message{{Role: "user", Content: "hello"}}, toolDefinitions())
	if e != nil || len(msg.ToolCalls) != 1 {
		t.Fatal(msg, e)
	}
}
