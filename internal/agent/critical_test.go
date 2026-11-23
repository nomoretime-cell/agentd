package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func failingModel(t *testing.T) Model {
	return modelFunc(func(context.Context, []Message, []Tool) (Message, error) {
		t.Error("default model used for a critical action")
		return Message{}, errors.New("wrong tier")
	})
}

func fireConsolidation(t *testing.T, s *Service) {
	t.Helper()
	if err := s.UpsertTimer(context.Background(), Timer{ID: consolidationTimerID, FireAt: time.Now().Add(-time.Second).UnixMilli(), Prompt: "consolidate", Active: true}); err != nil {
		t.Fatal(err)
	}
}

// Without the strong tier a critical action does not run at all, and it does
// not dead-letter the Inbox: later events still run.
func TestCriticalActionNeverFallsBack(t *testing.T) {
	var calls int
	m := modelFunc(func(_ context.Context, msgs []Message, _ []Tool) (Message, error) {
		calls++
		if strings.Contains(msgs[len(msgs)-1].Content, "consolidate") {
			t.Error("default model ran the critical action")
		}
		return Message{Role: "assistant", Content: "ok"}, nil
	})
	s := openTest(t, testConfig(), m)
	s.Start(context.Background())
	fireConsolidation(t, s)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 1 })
	if n := count(t, s, "SELECT count(*) FROM inbox WHERE status='dead' OR retries>0"); n != 0 {
		t.Fatal("critical action was retried or dead-lettered")
	}
	if _, _, err := s.Append(context.Background(), "after", Payload{Kind: "message", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 2 })
	if calls != 1 {
		t.Fatalf("default model calls = %d", calls)
	}
	day := "sessions/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	raw, _, _ := s.transcriptRead(day, 0, 1<<20)
	if !strings.Contains(string(raw), `"status":"skipped"`) || !strings.Contains(string(raw), "strong_model is not enabled") {
		t.Fatalf("skip not recorded: %s", raw)
	}
	if _, _, err := s.Append(context.Background(), "spoof", Payload{Kind: kindConsolidation, Text: "x"}); err == nil {
		t.Fatal("API raised a critical action")
	}
}

// The strong tier runs the action with file tools only; a failure is recorded
// and not retried.
func TestCriticalActionUsesStrongTierWithFileToolsOnly(t *testing.T) {
	s := openTest(t, testConfig(), failingModel(t))
	var offered []string
	var denied error
	steps := 0
	s.strong = modelFunc(func(_ context.Context, msgs []Message, tools []Tool) (Message, error) {
		steps++
		switch steps {
		case 1:
			for _, tool := range tools {
				offered = append(offered, tool.Function.Name)
			}
			if strings.Contains(msgs[0].Content, "Previously sent messages") {
				t.Error("critical run sees messages sent to other conversations")
			}
			return call("send_message", `{"content":"leak"}`), nil
		case 2:
			if c := msgs[len(msgs)-1].Content; !strings.Contains(c, "denied") && !strings.Contains(c, "ERROR") {
				denied = errors.New(c)
			}
			return Message{}, errors.New("strong service down")
		}
		return Message{Role: "assistant", Content: "done"}, nil
	})
	s.Start(context.Background())
	fireConsolidation(t, s)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 1 })
	if strings.Join(offered, ",") != "read_file,write_file,edit_file" {
		t.Fatalf("critical run offered %v", offered)
	}
	if denied != nil {
		t.Fatalf("send_message not denied: %v", denied)
	}
	if n := count(t, s, "SELECT count(*) FROM outbox"); n != 0 {
		t.Fatal("critical run queued a message")
	}
	if n := count(t, s, "SELECT retries FROM inbox"); n != 0 || steps != 2 {
		t.Fatalf("failed critical run retried (retries=%d steps=%d)", n, steps)
	}
}

// A critical run sends what it reads to an external service, so it reads only
// the owner's private chats and local runs; group, other users', unknown and
// pre-scope records stay on this machine. Paging still advances by file bytes.
func TestCriticalTranscriptReadLeavesOutNonOwnerRecords(t *testing.T) {
	c := testConfig()
	c.Limits.MaxOutputBytes = 1024
	s := openTest(t, c, finalModel())
	lines := []string{
		`{"principal":"owner","role":"default","conversation":"a","text":"OWNER_1"}`,
		`{"principal":"group-1","role":"default","conversation":"g","text":"GROUP"}`,
		`{"principal":"local","role":"default","conversation":"local","text":"LOCAL_1"}`,
		`{"type":"run","text":"LEGACY"}`,
		`{"principal":"user-2","role":"default","conversation":"u","text":"OTHER_USER"}`,
		`{"principal":"unknown","role":"default","conversation":"unknown","text":"UNKNOWN"}`,
		`{"principal":"maintenance","role":"default","conversation":"maintenance","text":"MAINTENANCE"}`,
	}
	for i := range 40 {
		lines = append(lines, `{"principal":"owner","role":"default","conversation":"a","text":"OWNER_PAGE_`+strings.Repeat("x", 40)+string(rune('A'+i%26))+`"}`)
	}
	day := time.Now().UTC().Format("2006-01-02") + ".jsonl"
	file := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(s.root, "data/sessions", day), []byte(file), 0600); err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), criticalKey{}, true)
	var seen strings.Builder
	omitted := 0.0
	offset := 0
	for pages := 0; offset < len(file); pages++ {
		if pages > 100 {
			t.Fatal("paging made no progress")
		}
		read := ToolCall{Function: FunctionCall{Name: "read_file", Arguments: `{"path":"sessions/` + day + `","offset":` + itoa(offset) + `}`}}
		res, _, err := s.tool(ctx, "r", read, "", &Outcome{})
		if err != nil {
			t.Fatal(err)
		}
		if len(res) > c.Limits.MaxOutputBytes {
			t.Fatalf("page of %d bytes exceeds the output limit", len(res))
		}
		var page struct {
			Content string  `json:"content"`
			Next    int     `json:"next_offset"`
			Omitted float64 `json:"omitted_records"`
		}
		if err = json.Unmarshal([]byte(res), &page); err != nil {
			t.Fatal(err)
		}
		if page.Next <= offset {
			t.Fatalf("no progress at %d", offset)
		}
		seen.WriteString(page.Content)
		omitted += page.Omitted
		offset = page.Next
	}
	got := seen.String()
	for _, hidden := range []string{"GROUP", "LEGACY", "OTHER_USER", "UNKNOWN", "MAINTENANCE"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("%s record reached a critical run", hidden)
		}
	}
	if !strings.Contains(got, "OWNER_1") || !strings.Contains(got, "LOCAL_1") || strings.Count(got, "OWNER_PAGE_") != 40 {
		t.Fatalf("owner and local records missing: %q", got)
	}
	if omitted != 5 {
		t.Fatalf("omitted %v records", omitted)
	}
	// An offset inside a record skips that record instead of guessing its scope.
	read := ToolCall{Function: FunctionCall{Name: "read_file", Arguments: `{"path":"sessions/` + day + `","offset":5}`}}
	res, _, err := s.tool(ctx, "r", read, "", &Outcome{})
	if err != nil || strings.Contains(res, "OWNER_1") {
		t.Fatalf("mid-record offset: %v %s", err, res)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Hosted APIs reject the local server's template options; the strong tier
// sends only standard fields, and its key never reaches records.
func TestStrongModelSendsStandardRequest(t *testing.T) {
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		auth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()
	c := testConfig()
	c.StrongModel.Enabled = true
	c.StrongModel.Endpoint = srv.URL + "/v1/chat/completions"
	c.StrongModel.Name = "strong"
	c.StrongModel.APIKey = "sk-strong-secret-value"
	s := openTest(t, c, finalModel())
	if _, err := s.strong.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, toolDefinitions()); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"chat_template_kwargs", "temperature", "max_tokens", "parallel_tool_calls"} {
		if _, ok := body[k]; ok {
			t.Fatalf("standard request carries %s", k)
		}
	}
	if body["model"] != "strong" || body["tools"] == nil || auth != "Bearer sk-strong-secret-value" {
		t.Fatalf("request %v auth %q", body, auth)
	}
	if strings.Contains(s.redact("key sk-strong-secret-value"), "sk-strong") {
		t.Fatal("strong API key not redacted")
	}
}

func TestStrongModelConfigValidation(t *testing.T) {
	c := testConfig()
	c.StrongModel.Enabled = true
	c.StrongModel.Name = "m"
	for endpoint, ok := range map[string]bool{
		"https://api.example.com/v1/chat/completions": true,
		"http://127.0.0.1:9000/v1/chat/completions":   true,
		"http://api.example.com/v1/chat/completions":  false,
	} {
		c.StrongModel.Endpoint = endpoint
		if err := c.Validate(); (err == nil) != ok {
			t.Fatalf("%s: %v", endpoint, err)
		}
	}
	c.StrongModel.Endpoint = "https://api.example.com/v1/chat/completions"
	c.StrongModel.Name = ""
	if c.Validate() == nil {
		t.Fatal("strong tier without a model name accepted")
	}
}
