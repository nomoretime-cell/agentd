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
	"time"
)

// Everything before the clock stays byte-identical between runs so a model
// server can reuse its prompt prefix cache.
func TestSystemPromptPrefixIsStable(t *testing.T) {
	c := testConfig()
	for _, name := range []string{"zeta", "alpha", "mid", "beta", "omega"} {
		c.Actions[name] = Operation{Mode: "ask", URL: "https://example.invalid/" + name, Method: "POST"}
	}
	s := openTest(t, c, finalModel())
	prefix := func() string {
		p := s.systemPrompt()
		i := strings.Index(p, "\nCurrent time: ")
		if i < 0 {
			t.Fatal("clock missing from prompt")
		}
		return p[:i]
	}
	first := prefix()
	for range 20 {
		if prefix() != first {
			t.Fatal("prompt prefix changed between runs")
		}
	}
	if strings.Index(first, "alpha:") > strings.Index(first, "zeta:") {
		t.Fatal("named operations are not listed in a stable order")
	}
}

func TestMemoryIndexCap(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	old, err := s.workspaceRead("MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	over := strings.Repeat("- topic: notes/topic.md\n", memoryIndexMaxLines+1)
	if err = s.workspaceWrite("MEMORY.md", over, digest(old)); err == nil || !strings.Contains(err.Error(), "Merge entries") {
		t.Fatalf("over-limit index accepted: %v", err)
	}
	full := strings.Repeat("- topic: notes/topic.md\n", memoryIndexMaxLines)
	if err = s.workspaceWrite("MEMORY.md", full, digest(old)); err != nil {
		t.Fatal(err)
	}
	// Notes hold the details and are not capped by the index limit.
	if err = s.workspaceWrite("notes/long.md", over, "absent"); err != nil {
		t.Fatal(err)
	}
}

// Every run leaves a raw record the model can read back but never edit, and
// group turns cannot read the owner's transcripts.
func TestTranscriptRecordsRunsAndToolCalls(t *testing.T) {
	c := testConfig()
	c.WeCom.GroupOwnerTools = true // group turns get owner tools, yet still not transcripts
	s := openTest(t, c, notifyModel())
	s.Start(context.Background())
	if _, _, err := s.Append(context.Background(), "t1", Payload{Kind: "message", Text: "hello " + s.Token()}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 1 })
	name := "sessions/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	waitFor(t, time.Second, func() bool {
		b, _, _ := s.transcriptRead(name, 0, 1<<20)
		return strings.Contains(string(b), `"type":"run"`)
	})
	raw, _, err := s.transcriptRead(name, 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), s.Token()) {
		t.Fatal("transcript contains a host secret")
	}
	var tool, run map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec map[string]any
		if err = json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatal(err)
		}
		switch rec["type"] {
		case "tool":
			tool = rec
		case "run":
			run = rec
		}
	}
	if tool == nil || tool["tool"] != "send_message" || !strings.Contains(tool["result"].(string), "staged") {
		t.Fatalf("tool call not recorded: %v", tool)
	}
	if run == nil || run["status"] != "done" || run["answer"] != "Complete." || run["notifications"] == nil {
		t.Fatalf("run not recorded: %v", run)
	}

	read := ToolCall{Function: FunctionCall{Name: "read_file", Arguments: `{"path":"` + name + `"}`}}
	if _, _, err = s.tool(context.Background(), "r1", read, "", &Outcome{}); err != nil {
		t.Fatal(err)
	}
	team := context.WithValue(context.WithValue(context.Background(), teamTurnKey{}, true), sharedTurnKey{}, true)
	if _, _, err = s.tool(team, "r2", read, "", &Outcome{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("group turn read a transcript: %v", err)
	}
	write := ToolCall{Function: FunctionCall{Name: "write_file", Arguments: `{"path":"` + name + `","content":"x","expected_hash":"absent"}`}}
	if _, _, err = s.tool(context.Background(), "w1", write, "", &Outcome{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("transcript writable: %v", err)
	}
}

// An index edited directly on disk is cut to the same cap when it loads.
func TestMemoryIndexClippedAtAssembly(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	var b strings.Builder
	for i := 1; i <= memoryIndexMaxLines+50; i++ {
		fmt.Fprintf(&b, "- entry-%d\n", i)
	}
	if err := os.WriteFile(filepath.Join(s.root, "workspace/MEMORY.md"), []byte(b.String()), 0600); err != nil {
		t.Fatal(err)
	}
	p := s.systemPrompt()
	if !strings.Contains(p, fmt.Sprintf("- entry-%d\n", memoryIndexMaxLines)) || strings.Contains(p, fmt.Sprintf("- entry-%d\n", memoryIndexMaxLines+1)) {
		t.Fatal("index not clipped to its line cap")
	}
}

// A DingTalk group reply is visible to the whole group, so a run answering
// there cannot read the owner's transcripts; the owner's direct chat can.
func TestTranscriptHiddenFromDingTalkGroup(t *testing.T) {
	c := dtConfig()
	c.DingTalk.AllowAllGroups = true
	day := "sessions/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	var toolResult string
	m := modelFunc(func(_ context.Context, msgs []Message, _ []Tool) (Message, error) {
		if last := msgs[len(msgs)-1]; last.Role == "tool" {
			toolResult = last.Content
			return Message{Role: "assistant", Content: "done"}, nil
		}
		return call("read_file", `{"path":"`+day+`"}`), nil
	})
	s := openTest(t, c, m)
	s.appendTranscript(map[string]any{"type": "run", "status": "done", "answer": "PRIVATE_MARKER"})
	for i, kind := range []string{"2", "1"} {
		route := &IMRoute{ConversationID: "conv" + kind, ConversationType: kind, Sender: "owner", Mentioned: true, Webhook: "https://oapi.dingtalk.com/robot/sendBySession?session=x", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
		raw, _ := json.Marshal(Payload{Kind: "dingtalk", Text: "read my transcript", imRoutes: imRoutes{DingTalk: route}})
		if _, err := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at) VALUES(?,?,0)", "dt"+kind, string(raw)); err != nil {
			t.Fatal(err)
		}
		batch, err := s.claim(context.Background())
		if err != nil || len(batch) != 1 {
			t.Fatal(batch, err)
		}
		out, err := s.execute(context.Background(), batch)
		if err == nil {
			err = s.commitBatch(context.Background(), batch, out)
		}
		if err != nil {
			t.Fatal(err)
		}
		if group := i == 0; group == strings.Contains(toolResult, "PRIVATE_MARKER") {
			t.Fatalf("conversation type %s: transcript visible=%v", kind, !group)
		}
	}
}

// Transcripts grow all day; any page can be read however large the file is.
func TestTranscriptPagesPastWorkspaceLimit(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	line := `{"type":"run","answer":"` + strings.Repeat("长", 300) + `"}` + "\n"
	big := strings.Repeat(line, (1<<20)/len(line)+50)
	if err := os.WriteFile(filepath.Join(s.root, "data/sessions/2026-01-01.jsonl"), []byte(big), 0600); err != nil {
		t.Fatal(err)
	}
	var page struct {
		Content    string
		NextOffset int `json:"next_offset"`
		Size       int
	}
	read := func(offset int) {
		res, _, err := s.tool(context.Background(), "r", ToolCall{Function: FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":"sessions/2026-01-01.jsonl","offset":%d}`, offset)}}, "", &Outcome{})
		if err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(res), &page); err != nil {
			t.Fatal(err)
		}
	}
	read(0)
	if page.Size != len(big) || page.Content == "" || !strings.HasSuffix(page.Content, "\n") {
		t.Fatalf("first page: size=%d len=%d", page.Size, len(page.Content))
	}
	read(len(big) - 2*len(line))
	if page.NextOffset != len(big) || page.Content != line+line {
		t.Fatal("last page incomplete")
	}
}

// Reading a transcript, even today's, does not grow it.
func TestTranscriptReadIsNotRecorded(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	day := "sessions/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	s.appendTranscript(map[string]any{"type": "run", "status": "done"})
	_, before, _ := s.transcriptRead(day, 0, 1)
	for range 3 {
		if _, _, err := s.tool(context.Background(), "r", ToolCall{Function: FunctionCall{Name: "read_file", Arguments: `{"path":"` + day + `"}`}}, "", &Outcome{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, after, _ := s.transcriptRead(day, 0, 1); after != before {
		t.Fatalf("transcript grew from %d to %d bytes by being read", before, after)
	}
}

// Blank lines count toward the index cap.
func TestMemoryIndexCountsBlankLines(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	old, _ := s.workspaceRead("MEMORY.md")
	if err := s.workspaceWrite("MEMORY.md", "x"+strings.Repeat("\n", memoryIndexMaxLines+50), digest(old)); err == nil {
		t.Fatal("index padded with blank lines accepted")
	}
}

// Tools that OpenCode runs itself, such as web search, reach the transcript
// with what OpenCode reported; host tools are recorded once, by the host.
func TestOpenCodeToolsReachTranscript(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "opencode")
	script := `#!/bin/sh
while [ "$1" != "--session" ]; do shift; done
sid=$2
cat >/dev/null
echo '{"type":"step_start","sessionID":"'$sid'"}'
echo '{"type":"tool_use","sessionID":"'$sid'","part":{"tool":"websearch","state":{"status":"running","input":{"query":"go"}}}}'
echo '{"type":"tool_use","sessionID":"'$sid'","part":{"tool":"websearch","state":{"status":"completed","title":"Go official site","input":{"query":"go"},"output":"https://go.dev/ SOURCE_MARKER"}}}'
echo '{"type":"tool_use","sessionID":"'$sid'","part":{"tool":"read","state":{"status":"error","input":{"filePath":"/missing"},"error":"ERROR_MARKER file not found"}}}'
echo '{"type":"tool_use","sessionID":"'$sid'","part":{"tool":"agentd_read_file","state":{"status":"completed","output":"HOST_TOOL"}}}'
echo '{"type":"text","sessionID":"'$sid'","part":{"text":"done"}}'
echo '{"type":"step_finish","sessionID":"'$sid'","part":{"reason":"stop"}}'
`
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c := testConfig()
	c.OpenCodeBinary = bin
	s := openTest(t, c, nil)
	if _, _, err := s.Append(context.Background(), "oc", Payload{Kind: "message", Text: "search"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Duration(c.Limits.DebounceMS) * time.Millisecond * 2)
	batch, err := s.claim(context.Background())
	if err != nil || len(batch) != 1 {
		t.Fatal(batch, err)
	}
	if _, err = s.execute(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	raw, _, err := s.transcriptRead("sessions/"+time.Now().UTC().Format("2006-01-02")+".jsonl", 0, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Count(text, `"executor":"opencode"`) != 2 || !strings.Contains(text, "SOURCE_MARKER") || !strings.Contains(text, `"error":"ERROR_MARKER file not found"`) || !strings.Contains(text, `\"query\":\"go\"`) || strings.Contains(text, "HOST_TOOL") {
		t.Fatalf("opencode tools not recorded as expected:\n%s", text)
	}
}

// Escaping can multiply a page's size when it becomes JSON. Whatever the
// content, each page must still arrive as complete JSON after the native
// loop's cut, and the pages must add up to the whole file.
func TestReadPagesSurviveEscaping(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	// A record longer than a page is cut by bytes, so a page of backslashes doubles when encoded.
	record := `{"type":"tool","args":"` + strings.Repeat(`\\`, 6000) + strings.Repeat(`\"<&>`, 200) + `"}` + "\n"
	transcript := strings.Repeat(record, 6)
	if err := os.WriteFile(filepath.Join(s.root, "data/sessions/2026-01-02.jsonl"), []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	note := strings.Repeat(`\"`, 3000) + strings.Repeat(`<a href="x">\path\to & more</a>`+"\n", 600)
	if err := s.workspaceWrite("notes/escaped.md", note[:s.cfg.Limits.MaxOutputBytes-1], "absent"); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"sessions/2026-01-02.jsonl": transcript, "notes/escaped.md": note[:s.cfg.Limits.MaxOutputBytes-1]} {
		var got strings.Builder
		for offset, pages := 0, 0; offset < len(want); pages++ {
			if pages > 100 {
				t.Fatalf("%s: paging does not advance", path)
			}
			res, _, err := s.tool(context.Background(), "r", ToolCall{Function: FunctionCall{Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q,"offset":%d}`, path, offset)}}, "", &Outcome{})
			if err != nil {
				t.Fatal(err)
			}
			var page struct {
				Content    string
				NextOffset int `json:"next_offset"`
			}
			if err = json.Unmarshal([]byte(s.bounded(res)), &page); err != nil || page.NextOffset <= offset {
				t.Fatalf("%s at %d: page unusable after the loop's cut (%v)", path, offset, err)
			}
			got.WriteString(page.Content)
			offset = page.NextOffset
		}
		if got.String() != want {
			t.Fatalf("%s: pages do not reassemble the file", path)
		}
	}
}
