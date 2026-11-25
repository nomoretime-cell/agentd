package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWebChatBatchesHistoryAndOwnership(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.DebounceMS = 100
	model := modelFunc(func(ctx context.Context, m []Message, _ []Tool) (Message, error) {
		var parts []string
		for _, v := range m[1:] {
			parts = append(parts, v.Content)
		}
		return Message{Role: "assistant", Content: strings.Join(parts, " / ")}, nil
	})
	s := openTest(t, cfg, model)
	ctx := t.Context()
	a, b := "web-"+randomID(), "web-"+randomID()
	if err := s.saveSession([]Message{{Role: "user", Content: "LOCAL-ONLY"}}); err != nil {
		t.Fatal(err)
	}
	appendMsg := func(chat, key, body string) int64 {
		t.Helper()
		id, new, err := s.Append(ctx, key, Payload{Kind: "message", WebChat: chat, Text: body})
		if err != nil || !new {
			t.Fatal("append", err)
		}
		return id
	}
	first := appendMsg(a, "shared-key", "A-ONE")
	second := appendMsg(a, "second", "A-TWO")
	appendMsg(b, "shared-key", "B-PRIVATE")
	appendMsg(a, "third", "A-THREE")
	if id, added, e := s.Append(ctx, "shared-key", Payload{Kind: "message", WebChat: a, Text: "A-ONE"}); e != nil || added || id != first {
		t.Fatal("replay", e)
	}
	if _, _, e := s.Append(ctx, "shared-key", Payload{Kind: "message", WebChat: a, Text: "different"}); !errors.Is(e, ErrConflict) {
		t.Fatal("different body accepted")
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 4 })
	ap, err := s.webChatEvents(ctx, a, 1<<62, 30)
	if err != nil || len(ap.Items) != 3 {
		t.Fatal("thread A", err)
	}
	x, y, z := ap.Items[0], ap.Items[1], ap.Items[2]
	if z.ID != first || z.Reply != "" || y.ID != second || !strings.Contains(y.Reply, "A-ONE") || !strings.Contains(y.Reply, "A-TWO") {
		t.Fatal("batched reply out of order")
	}
	if !strings.Contains(x.Reply, "A-ONE") || !strings.Contains(x.Reply, "A-THREE") || strings.Contains(x.Reply, "B-PRIVATE") || strings.Contains(x.Reply, "LOCAL-ONLY") {
		t.Fatal("history not scoped")
	}
	bp, err := s.webChatEvents(ctx, b, 1<<62, 30)
	if err != nil || len(bp.Items) != 1 || strings.Contains(bp.Items[0].Reply, "A-") {
		t.Fatal("thread B", err)
	}
	m, err := s.messageDetail(ctx, y.ReplyID)
	if err != nil || m.Run != y.Run || m.Conversation != a || m.Event != first {
		t.Fatal("trace did not link batch", err)
	}
	r, err := s.historicalRun(ctx, y.Run)
	if err != nil || r.Channel != "web" || r.Principal != "owner" || len(r.Inputs) != 2 {
		t.Fatal("scope", err)
	}
	if local := s.loadSession(); len(local) != 1 || local[0].Content != "LOCAL-ONLY" {
		t.Fatal("web transcript leaked to CLI")
	}
	if count(t, s, "SELECT count(*) FROM outbox") != 0 {
		t.Fatal("private replies entered outbox")
	}
}

func TestWebChatApprovalAfterRestart(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); fmt.Fprint(w, "local fixture receipt") }))
	defer remote.Close()
	root := t.TempDir()
	cfg := testConfig()
	cfg.Actions["ticket"] = Operation{Mode: "ask", URL: remote.URL, Method: "POST"}
	model := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		last := m[len(m)-1]
		if strings.Contains(last.Content, "Host receipt:") {
			return Message{Role: "assistant", Content: "approved in A"}, nil
		}
		if strings.Contains(last.Content, "need ticket") {
			return call("bash", `{"action":"ticket","arguments":{}}`), nil
		}
		if strings.Contains(last.Content, "notify separately") {
			return call("send_message", `{"content":"explicit notification"}`), nil
		}
		return Message{Role: "assistant", Content: "ordinary answer"}, nil
	})
	ctx := t.Context()
	a, b := "web-"+randomID(), "web-"+randomID()
	s, err := Open(root, cfg, model)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if s != nil {
			s.Close()
		}
	}()
	if _, _, err = s.Append(ctx, "ticket", Payload{Kind: "message", WebChat: a, Text: "need ticket"}); err != nil {
		t.Fatal(err)
	}
	s.Start(ctx)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM actions WHERE status='pending_approval'") == 1 })
	p, err := s.webChatEvents(ctx, a, 1<<62, 30)
	if err != nil || len(p.Items) != 1 || p.Items[0].ActionStatus != "pending_approval" {
		t.Fatal("approval missing", err)
	}
	action := p.Items[0].Action
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, cfg, model)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Append(ctx, "other", Payload{Kind: "message", WebChat: b, Text: "other conversation"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.Decide(ctx, action, "approve"); err != nil {
			t.Fatal(err)
		}
	}
	s.Start(ctx)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 3 })
	p, err = s.webChatEvents(ctx, a, 1<<62, 30)
	if err != nil || len(p.Items) != 2 || p.Items[0].Reply != "approved in A" || p.Items[0].Action != action || p.Items[0].ActionStatus != "succeeded" {
		t.Fatal("continuation misplaced", err)
	}
	other, err := s.webChatEvents(ctx, b, 1<<62, 30)
	if err != nil || len(other.Items) != 1 || other.Items[0].Reply != "ordinary answer" {
		t.Fatal("foreign continuation", err)
	}
	if calls.Load() != 1 || count(t, s, "SELECT count(*) FROM outbox") != 0 {
		t.Fatal("duplicate execution or implicit external delivery")
	}
	if _, _, err = s.Append(ctx, "explicit", Payload{Kind: "message", WebChat: a, Text: "notify separately"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM outbox WHERE content='explicit notification'") == 1
	})
}

func TestWebChatOpenCodeSessionKeys(t *testing.T) {
	cfg := testConfig()
	cfg.OpenCodeBinary = filepath.Join(t.TempDir(), "opencode")
	script := `#!/bin/sh
while [ "$1" != "--session" ]; do shift; done
sid=$2
cat >/dev/null
printf '{"type":"text","sessionID":"%s","part":{"text":"%s"}}\n' "$sid" "$sid"
printf '{"type":"step_finish","sessionID":"%s","part":{"id":"finish","reason":"stop"}}\n' "$sid"
`
	if err := os.WriteFile(cfg.OpenCodeBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	s := openTest(t, cfg, nil)
	ctx := t.Context()
	a, b := "web-"+randomID(), "web-"+randomID()
	for i, chat := range []string{a, b, a} {
		_, _, err := s.Append(ctx, fmt.Sprint(i), Payload{Kind: "message", WebChat: chat, Text: "hello"})
		if err != nil {
			t.Fatal(err)
		}
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 3 })
	for _, chat := range []string{a, b} {
		p, err := s.webChatEvents(ctx, chat, 1<<62, 30)
		if err != nil || len(p.Items) == 0 {
			t.Fatal(err)
		}
		for _, e := range p.Items {
			if e.Reply != "ses_"+stableID(chat) {
				t.Fatal("wrong external session")
			}
		}
	}
}

func TestWebChatCursorSkipsOtherScopesAndNewArrivals(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxHistoryMessages = 0
	s := openTest(t, cfg, finalModel())
	ctx := t.Context()
	a, b := "web-"+randomID(), "web-"+randomID()
	for i := range 36 {
		for _, chat := range []string{a, b, ""} {
			key := fmt.Sprintf("key-%d", i)
			_, _, err := s.Append(ctx, key, Payload{Kind: "message", Text: key, WebChat: chat})
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	first, err := s.webChatEvents(ctx, a, 1<<62, 30)
	if err != nil || len(first.Items) != 30 || first.Next == "" || first.Items[0].Text != "key-35" || first.Items[0].State != "pending" {
		t.Fatal("first page", err)
	}
	if _, _, err = s.Append(ctx, "new", Payload{Kind: "message", Text: "arrived", WebChat: a}); err != nil {
		t.Fatal(err)
	}
	before, err := chatCursor(first.Next)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := s.webChatEvents(ctx, a, before, 30)
	if err != nil || len(rest.Items) != 6 || rest.Items[0].Text != "key-5" || rest.Next != "" {
		t.Fatal("cursor lost position", err)
	}
	threads, err := s.webChats(ctx, 1<<62)
	if err != nil || len(threads.Items) != 2 || threads.Items[0].ID != b || threads.Items[1].ID != a {
		t.Fatal("private thread list", err)
	}
	m, err := s.webChatHistory(ctx, a, 1<<62)
	if err != nil || len(m) != 0 {
		t.Fatal("zero history", err)
	}
}

// A batch that died together but was resolved separately must not leave a
// skipped input pointing at the other input's reply.
func TestWebChatSplitBatchKeepsRepliesApart(t *testing.T) {
	var calls atomic.Int32
	model := modelFunc(func(context.Context, []Message, []Tool) (Message, error) {
		if calls.Add(1) <= 3 {
			return Message{}, errors.New("model unavailable")
		}
		return Message{Role: "assistant", Content: "the reply"}, nil
	})
	cfg := testConfig()
	cfg.Limits.DebounceMS = 200
	s := openTest(t, cfg, model)
	ctx := t.Context()
	chat := "web-" + randomID()
	var ids []int64
	for i, text := range []string{"first", "second"} {
		id, _, err := s.Append(ctx, fmt.Sprint("k", i), Payload{Kind: "message", WebChat: chat, Text: text})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	s.Start(ctx)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='dead'") == 2 })
	if err := s.ResolveEvent(ctx, ids[1], "skip"); err != nil {
		t.Fatal(err)
	}
	if err := s.ResolveEvent(ctx, ids[0], "retry"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 4*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM inbox WHERE id=? AND status='done'", ids[0]) == 1
	})
	p, err := s.webChatEvents(ctx, chat, 1<<62, 10)
	if err != nil {
		t.Fatal(err)
	}
	replies := map[int64]string{}
	for _, e := range p.Items {
		replies[e.ID] = e.ReplyID
	}
	if replies[ids[0]] == "" || replies[ids[1]] != "" {
		t.Fatalf("reply links %v", replies)
	}
	history, err := s.webChatHistory(ctx, chat, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	answers := 0
	for _, m := range history {
		if m.Role == "assistant" {
			answers++
		}
	}
	if answers != 1 {
		t.Fatalf("history has %d answers", answers)
	}
}

// Trimming history keeps a reply with its question.
func TestWebChatHistoryKeepsTurnsWhole(t *testing.T) {
	cfg := testConfig()
	cfg.Limits.MaxHistoryMessages = 3
	s := openTest(t, cfg, finalModel())
	ctx := t.Context()
	chat := "web-" + randomID()
	s.Start(ctx)
	for i := range 2 {
		id, _, err := s.Append(ctx, fmt.Sprint("t", i), Payload{Kind: "message", WebChat: chat, Text: fmt.Sprint("question ", i)})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE id=? AND status='done'", id) == 1 })
	}
	history, err := s.webChatHistory(ctx, chat, 1<<62)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 2 || history[0].Role != "user" || history[1].Role != "assistant" {
		t.Fatalf("history %+v", history)
	}
}
