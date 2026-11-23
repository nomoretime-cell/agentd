package agent

import (
	"context"
	"encoding/json"
	"errors"
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
	waitFor(t, time.Second, func() bool { b, _ := s.workspaceRead(name); return strings.Contains(string(b), `"type":"run"`) })
	raw, err := s.workspaceRead(name)
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
	team := context.WithValue(context.Background(), teamTurnKey{}, true)
	if _, _, err = s.tool(team, "r2", read, "", &Outcome{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("group turn read a transcript: %v", err)
	}
	write := ToolCall{Function: FunctionCall{Name: "write_file", Arguments: `{"path":"` + name + `","content":"x","expected_hash":"absent"}`}}
	if _, _, err = s.tool(context.Background(), "w1", write, "", &Outcome{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("transcript writable: %v", err)
	}
}
