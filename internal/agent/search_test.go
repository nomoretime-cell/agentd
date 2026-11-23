package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTranscript(t *testing.T, s *Service, day string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.root, "data/sessions", day+".jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func rec(principal, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "run", "principal": principal, "role": "default", "conversation": "c", "inputs": []map[string]string{{"text": text}}})
	return string(b)
}

func runSearch(t *testing.T, s *Service, ctx context.Context, args string) (searchResult, error) {
	t.Helper()
	res, _, err := s.tool(ctx, "q", ToolCall{Function: FunctionCall{Name: "search", Arguments: args}}, "", &Outcome{})
	var out searchResult
	if err == nil {
		if e := json.Unmarshal([]byte(res), &out); e != nil {
			t.Fatal(e, res)
		}
		if len(res) > s.cfg.Limits.MaxOutputBytes {
			t.Fatalf("result of %d bytes exceeds the output limit", len(res))
		}
	}
	return out, err
}

// One call finds a record from any day, newest day first, and read_file at
// the returned offset starts at that record.
func TestSearchFindsEarlierWork(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	writeTranscript(t, s, "2026-11-01", rec("owner", "filler"), rec("owner", "请帮我查 Go 官方网站，链接是 https://go.dev"))
	writeTranscript(t, s, "2026-11-20", rec("owner", "今天的 GO 链接是 https://go.dev/doc"))
	if err := os.WriteFile(filepath.Join(s.root, "workspace/notes/go.md"), []byte("# Go\n常用链接：go.dev\n"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := runSearch(t, s, context.Background(), `{"query":"go 链接"}`)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, h := range res.Hits {
		paths = append(paths, h.Path)
	}
	if strings.Join(paths, ",") != "notes/go.md,sessions/2026-11-20.jsonl,sessions/2026-11-01.jsonl" {
		t.Fatalf("hits %v", paths)
	}
	old := res.Hits[2]
	read := ToolCall{Function: FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q,"offset":%d}`, old.Path, old.Offset)}}
	page, _, err := s.tool(context.Background(), "r", read, "", &Outcome{})
	if err != nil {
		t.Fatal(err)
	}
	var p struct{ Content string }
	_ = json.Unmarshal([]byte(page), &p)
	if !strings.HasPrefix(p.Content, `{"conversation"`) || !strings.Contains(strings.SplitN(p.Content, "\n", 2)[0], "go.dev") {
		t.Fatalf("offset does not start at the hit: %q", p.Content)
	}
	if res, _ = runSearch(t, s, context.Background(), `{"query":"go.dev","path":"sessions/2026-11-01.jsonl"}`); len(res.Hits) != 1 {
		t.Fatalf("path filter: %v", res.Hits)
	}
}

func TestSearchFollowsScopeRules(t *testing.T) {
	c := testConfig()
	c.WeCom.GroupOwnerTools = true
	s := openTest(t, c, finalModel())
	writeTranscript(t, s, "2026-11-20", rec("owner", "MARK owner"), rec("group-1", "MARK group"), rec("local", "MARK local"), `{"type":"run","text":"MARK legacy"}`)
	if err := os.WriteFile(filepath.Join(s.root, "workspace/notes/n.md"), []byte("MARK note\n"), 0600); err != nil {
		t.Fatal(err)
	}
	shared := context.WithValue(context.Background(), sharedTurnKey{}, true)
	if _, err := runSearch(t, s, shared, `{"query":"MARK","path":"sessions/"}`); !errors.Is(err, ErrDenied) {
		t.Fatalf("shared turn searched transcripts: %v", err)
	}
	res, err := runSearch(t, s, shared, `{"query":"MARK"}`)
	if err != nil || len(res.Hits) != 1 || res.Hits[0].Path != "notes/n.md" || res.Skipped == "" {
		t.Fatalf("shared turn: %v %+v", err, res)
	}
	critical := context.WithValue(context.Background(), criticalKey{}, true)
	res, err = runSearch(t, s, critical, `{"query":"MARK","path":"sessions"}`)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, h := range res.Hits {
		got = append(got, h.Snippet)
	}
	if joined := strings.Join(got, "|"); len(got) != 2 || !strings.Contains(joined, "MARK owner") || !strings.Contains(joined, "MARK local") {
		t.Fatalf("critical run saw %v", got)
	}
	for _, bad := range []string{`{"query":"x","path":"../data"}`, `{"query":"x","path":"skills/"}`, `{"query":""}`} {
		if _, err = runSearch(t, s, context.Background(), bad); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	if err = os.Symlink(filepath.Join(s.root, "data/api-token"), filepath.Join(s.root, "workspace/notes/leak")); err != nil {
		t.Fatal(err)
	}
	if res, _ = runSearch(t, s, context.Background(), `{"query":"`+s.Token()[:8]+`"}`); len(res.Hits) != 0 {
		t.Fatal("search followed a symlink out of notes/")
	}
}

// Results stay within limits, secrets stay masked, and searching leaves no
// trace that later searches would find.
func TestSearchLimitsAndRecords(t *testing.T) {
	c := testConfig()
	c.Limits.MaxOutputBytes = 2048
	s := openTest(t, c, finalModel())
	var lines []string
	for i := range 50 {
		lines = append(lines, rec("owner", fmt.Sprintf("needle %d %s %s", i, strings.Repeat("x", 300), s.Token())))
	}
	writeTranscript(t, s, "2026-11-20", lines...)
	res, err := runSearch(t, s, context.Background(), `{"query":"NEEDLE"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Hits) == 0 || len(res.Hits) > searchMaxHits {
		t.Fatalf("hits %d truncated %v", len(res.Hits), res.Truncated)
	}
	if res, _ = runSearch(t, s, context.Background(), `{"query":"`+s.Token()+`"}`); len(res.Hits) != 0 {
		t.Fatal("secret is searchable")
	}
	b, _ := os.ReadFile(filepath.Join(s.root, "data/sessions", "2026-11-20.jsonl"))
	if strings.Count(string(b), "needle") != 50 {
		t.Fatal("search was recorded in the transcript")
	}
	day := todaySessions()
	if raw, _, _ := s.transcriptRead(day, 0, 1<<20); strings.Contains(string(raw), `"search"`) {
		t.Fatal("search call recorded")
	}
}
