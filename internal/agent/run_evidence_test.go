package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestEvidenceFollowsCommittedReplyAcrossDocumentChange(t *testing.T) {
	const path = "notes/handover.md"
	model := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		last := m[len(m)-1]
		if last.Role != "tool" {
			return call("search", `{"query":"receipt","path":"notes/"}`), nil
		}
		if strings.Contains(last.Content, `"hits"`) {
			return call("read_file", `{"path":"notes/handover.md","offset":2}`), nil
		}
		return Message{Role: "assistant", Content: "根据 notes/handover.md 完成。"}, nil
	})
	cfg := testConfig()
	cfg.Limits.DebounceMS = 100
	s := openTest(t, cfg, model)
	ctx := t.Context()
	original := "a receipt " + s.Token()
	if err := s.workspaceWrite(path, original, "absent"); err != nil {
		t.Fatal(err)
	}
	var first int64
	for i, text := range []string{"请检查", "并核对回执"} {
		id, _, err := s.Append(ctx, fmt.Sprint(i), Payload{Kind: "message", Text: text})
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = id
		}
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 2 })
	if err := s.workspaceWrite(path, "replacement", digest([]byte(original))); err != nil {
		t.Fatal(err)
	}
	m, err := s.messageDetail(ctx, fmt.Sprintf("local:%d", first))
	if err != nil || m.Run == "" || m.Event != first || m.Status != "committed" {
		t.Fatalf("reply association: %v, %s", err, m.Run)
	}
	r, err := s.historicalRun(ctx, m.Run)
	if err != nil || len(r.ReplyIDs) != 1 || len(r.Inputs) != 2 || r.Evidence == nil {
		t.Fatalf("run association: %v", err)
	}
	seen := map[string]bool{}
	for _, src := range r.Evidence.Sources {
		seen[src.Kind] = true
		if src.Kind == "read" && (src.Name != path || src.Version != digest([]byte(original)) || src.Offset != 2 || src.End != len(original) || !strings.HasPrefix(src.Text, "receipt ")) {
			t.Fatal("read did not preserve old masked version and range")
		}
	}
	for _, kind := range []string{"read", "hit", "tool_args", "request", "document", "catalog"} {
		if !seen[kind] {
			t.Errorf("missing %s", kind)
		}
	}
	raw, _ := json.Marshal(r.Evidence)
	if strings.Contains(string(raw), s.Token()) {
		t.Fatal("unmasked evidence")
	}
	pg, err := s.conversations(ctx, url.Values{"role": {r.Role}, "principal": {r.Principal}, "channel": {"local"}})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Count != 1 || !pg.Items[0].Active {
		t.Fatal("conversation aggregation or scope filter", err)
	}
	for _, path := range []string{"/v1/conversations", "/v1/deliveries", "/v1/replies/" + m.ID, "/v1/runs/" + m.Run} {
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
}

func TestEvidenceCommitFailureAndRestart(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig()
	ctx := t.Context()
	s, err := Open(root, cfg, notifyModel())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	// Simulate unavailable evidence storage after the initial running entry exists.
	_, err = s.db.Exec(`CREATE TRIGGER fail_evidence BEFORE UPDATE OF run_data ON inbox WHEN OLD.run_index IS NOT NULL BEGIN SELECT RAISE(ABORT,'unavailable');END;`)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := s.Append(ctx, "once", Payload{Kind: "message", Text: "notify"})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { _, err := os.Stat(filepath.Join(root, "data/paused.lock")); return err == nil })
	if count(t, s, "SELECT count(*) FROM inbox WHERE replies IS NOT NULL") != 0 || count(t, s, "SELECT count(*) FROM outbox") != 0 {
		t.Fatal("partial reply or notification escaped failed transaction")
	}
	pg, err := s.runHistory(ctx, url.Values{})
	if err != nil || len(pg.Items) != 1 || pg.Items[0].Status != "running" {
		t.Fatal("opening entry unavailable", err)
	}
	old := pg.Items[0].ID
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, cfg, notifyModel())
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := s.historicalRun(ctx, old)
	if err != nil || interrupted.Status != "interrupted" || interrupted.Evidence != nil {
		t.Fatal("crash did not retain interrupted attempt", err)
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_evidence"); err != nil {
		t.Fatal(err)
	}
	if err = s.SetPaused(false); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 1 })
	reply, err := s.messageDetail(ctx, fmt.Sprintf("local:%d", id))
	if err != nil || reply.Run == old || reply.Run == "" {
		t.Fatal("retry lost its new stable id", err)
	}
	list, err := s.noticeHistory(ctx, url.Values{})
	if err != nil || len(list.Items) != 1 {
		t.Fatal("duplicate or missing delivery", err)
	}
	notice, err := s.messageDetail(ctx, list.Items[0].ID)
	if err != nil || notice.Run != reply.Run || notice.Kind != "message" || notice.Body != "test notification" {
		t.Fatal("incorrect outbox provenance", err)
	}
}

func TestIndexedAttemptsResumeThroughSparseResultsAndArchive(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	ctx := t.Context()
	stamp := time.Now().UTC().Add(-time.Minute)
	q := url.Values{"from": {stamp.Format(time.DateOnly)}, "to": {stamp.Format(time.DateOnly)}, "q": {"needle"}}
	newRow := func(tx *sql.Tx, i int, at time.Time) error {
		key := fmt.Sprintf("%048x", i)
		body := "other"
		if i < 5 || i == 500 {
			body = "needle"
		}
		rec := map[string]any{"type": "run", "status": "done", "at": at.UnixMilli(), "ts": at.Format(time.RFC3339Nano), "answer": body}
		raw, err := json.Marshal(map[string]any{"_first": at.UnixMilli(), "_last": at.UnixMilli(), key: rec})
		if err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO inbox(dedup_key,payload,available_at,status,run_index) VALUES(?,'{}',0,'done',?)", key, string(raw))
		return err
	}
	if err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		for i := 1; i <= 212; i++ {
			if err := newRow(tx, i, stamp); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, "data/sessions", stamp.Format(time.DateOnly)+".jsonl")
	if err := os.WriteFile(path, []byte("{\"type\":\"run\",\"answer\":\"needle archived\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for i := 0; i < 10; i++ {
		p, err := s.runHistory(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if p.Next == "" || len(p.Items) != 0 {
				t.Fatal("scan budget not observed for sparse match")
			}
			if err = transaction(ctx, s.db, func(tx *sql.Tx) error { return newRow(tx, 500, stamp.Add(time.Millisecond)) }); err != nil {
				t.Fatal(err)
			}
		}
		for _, r := range p.Items {
			if ids[r.ID] {
				t.Fatal("duplicate across pages")
			}
			ids[r.ID] = true
		}
		if p.Next == "" {
			break
		}
		q.Set("cursor", p.Next)
	}
	if len(ids) != 5 || !ids[stamp.Format(time.DateOnly)+".0"] {
		t.Fatal("lost or duplicated indexed or archived attempt", len(ids))
	}
}

// Every step resends the conversation; evidence must still keep the last
// step's tool result instead of filling its use cap with repeats.
func TestEvidenceKeepsLastStepOfLongRun(t *testing.T) {
	// 30 steps resend about a thousand messages in total, far above the cap.
	steps := 0
	model := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		if steps < 30 {
			steps++
			return call("search", fmt.Sprintf(`{"query":"step-%d","path":"notes/"}`, steps)), nil
		}
		return Message{Role: "assistant", Content: "done"}, nil
	})
	cfg := testConfig()
	cfg.Limits.MaxSteps = 32
	s := openTest(t, cfg, model)
	ctx := t.Context()
	id, _, err := s.Append(ctx, "long", Payload{Kind: "message", Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE id=? AND status='done'", id) == 1 })
	m, err := s.messageDetail(ctx, fmt.Sprintf("local:%d", id))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.historicalRun(ctx, m.Run)
	if err != nil || r.Evidence == nil {
		t.Fatal(err)
	}
	last := false
	sawDocument := false
	if len(r.Calls) != 31 {
		t.Fatalf("got %d model calls, want 31", len(r.Calls))
	}
	for index, src := range r.Evidence.Sources {
		if src.Kind == "document" && src.Name == "MEMORY.md" {
			sawDocument = true
			steps := map[string]bool{}
			for _, use := range r.Evidence.Uses {
				if use.Source == index {
					steps[use.Step] = true
				}
			}
			for _, call := range r.Calls {
				if !steps[call.ID] {
					t.Fatalf("missing document evidence for call %s", call.ID)
				}
			}
		}
		if src.Kind == "tool_args" && strings.Contains(src.Text, "step-30") {
			last = true
		}
	}
	if !sawDocument || !last || r.Evidence.Omitted != 0 {
		t.Fatalf("last step kept=%v omitted=%d uses=%d", last, r.Evidence.Omitted, len(r.Evidence.Uses))
	}
}

func TestRequestEvidenceUsesOnlySourcesActuallySent(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	c := &capture{}
	ctx := context.WithValue(t.Context(), evidenceKey{}, c)
	system := Message{Role: "system", Content: "original prompt"}
	user := Message{Role: "user", Content: "one question", historyID: "message-1"}
	c.staged(s.source("document", "MEMORY.md", "v1", "note"))
	s.submitted(ctx, "first", []Message{system, user})
	s.submitted(ctx, "second", []Message{{Role: "system", Content: "different prompt"}, user})
	s.submitted(ctx, "third", []Message{system})
	want := [][]int{{0, 1, 2}, {3, 2}, {0, 1}}
	if len(c.Requests) != len(want) || len(c.Sources) != 4 {
		t.Fatalf("unexpected requests or duplicate sources: %d requests, %d sources", len(c.Requests), len(c.Sources))
	}
	for i, step := range c.Requests {
		if !slices.Equal(step.Sources, want[i]) {
			t.Fatalf("%s sources %v, want %v", step.Step, step.Sources, want[i])
		}
	}
}

// JSON escapes "<", ">" and "&"; tool arguments must be redacted before
// encoding or a secret containing them survives in evidence.
func TestEvidenceRedactsToolArgumentsBeforeEncoding(t *testing.T) {
	const secret = "sk<test>&key-0123456789"
	model := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		if m[len(m)-1].Role != "tool" {
			return call("search", `{"query":"`+secret+`","path":"notes/"}`), nil
		}
		return Message{Role: "assistant", Content: "done"}, nil
	})
	cfg := testConfig()
	cfg.Model.APIKey = secret
	s := openTest(t, cfg, model)
	ctx := t.Context()
	id, _, err := s.Append(ctx, "escaped", Payload{Kind: "message", Text: "go"})
	if err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE id=? AND status='done'", id) == 1 })
	var data string
	if err = s.read.QueryRow("SELECT run_data FROM inbox WHERE id=?", id).Scan(&data); err != nil {
		t.Fatal(err)
	}
	escaped, _ := json.Marshal(secret)
	if strings.Contains(data, secret) || strings.Contains(data, strings.Trim(string(escaped), `"`)) || strings.Contains(data, "0123456789") {
		for _, probe := range []string{secret, strings.Trim(string(escaped), `"`), "0123456789"} {
			if i := strings.Index(data, probe); i >= 0 {
				t.Logf("found %q near: %s", probe, data[max(0, i-300):min(len(data), i+80)])
			}
		}
		t.Fatal("secret kept in evidence")
	}
}

// Once the source cap is reached, a message resent by later requests is
// counted as omitted once, not once per request.
func TestEvidenceOmittedCountsEachSourceOnce(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	c := &capture{}
	ctx := context.WithValue(t.Context(), evidenceKey{}, c)
	for i := range maxEvidenceSources {
		c.add("fill", s.source("tool", fmt.Sprint("t", i), "", "x"))
	}
	extra := Message{Role: "user", Content: "does not fit"}
	for _, step := range []string{"a", "b", "c"} {
		s.submitted(ctx, step, []Message{extra})
	}
	if c.Omitted != 1 {
		t.Fatalf("omitted %d, want 1", c.Omitted)
	}
}
