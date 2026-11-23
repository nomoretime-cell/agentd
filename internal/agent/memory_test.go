package agent

import (
	"strings"
	"testing"
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
