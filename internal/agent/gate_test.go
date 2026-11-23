package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func skillDraft(name, description string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n## Procedure\n1. Do it.\n"
}

func writeDraft(t *testing.T, s *Service, name, file, content string) {
	t.Helper()
	p := filepath.Join(s.root, "workspace/skills-drafts", name, file)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Only critical runs can see or write drafts, and nobody can rewrite a
// rejection record.
func TestDraftsStayOutOfOrdinaryRuns(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	writeDraft(t, s, "weekly-report", "SKILL.md", skillDraft("weekly-report", "Write the weekly report."))
	ctx := context.Background()
	read := ToolCall{Function: FunctionCall{Name: "read_file", Arguments: `{"path":"skills-drafts/weekly-report/SKILL.md"}`}}
	if _, _, err := s.tool(ctx, "r", read, "", &Outcome{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("ordinary run read a draft: %v", err)
	}
	write := writeCall("skills-drafts/x/SKILL.md", "x", "absent").ToolCalls[0]
	if _, _, err := s.tool(ctx, "w", write, "", &Outcome{}); !errors.Is(err, ErrDenied) {
		t.Fatalf("ordinary run wrote a draft: %v", err)
	}
	critical := context.WithValue(ctx, criticalKey{}, true)
	if _, _, err := s.tool(critical, "r", read, "", &Outcome{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.tool(critical, "w", writeCall(".rejected/x/REJECTED.md", "x", "absent").ToolCalls[0], "", &Outcome{}); err == nil {
		t.Fatal("rejection record writable")
	}
	if _, _, err := s.tool(critical, "w", writeCall("skills-drafts/.rejected/x/REJECTED.md", "x", "absent").ToolCalls[0], "", &Outcome{}); err == nil {
		t.Fatal("rejection record writable")
	}
	if strings.Contains(s.systemPrompt(), "weekly-report") {
		t.Fatal("draft listed before approval")
	}
}

// A consolidation's drafts are part of its candidate: they appear only if the
// candidate passes, and the owner is told.
func TestConsolidationDraftsFollowCandidate(t *testing.T) {
	day := todaySessions()
	draft := func(s *Service) Message {
		return writeCall("skills-drafts/weekly-report/SKILL.md", skillDraft("weekly-report", "Write the weekly report."), "absent")
	}
	s, _ := consolidationService(t, draft)
	out, err := consolidate(s)
	if err != nil {
		t.Fatal(err)
	}
	if !fileExists(filepath.Join(s.root, "workspace/skills-drafts/weekly-report/SKILL.md")) {
		t.Fatal("draft not promoted with the candidate")
	}
	if len(out.Notifications) != 1 || !strings.Contains(out.Notifications[0].Content, "weekly-report") {
		t.Fatalf("owner not told about drafts: %v", out.Notifications)
	}
	if m := lastMetric(t, s); m["drafts"] != 1.0 {
		t.Fatalf("metric %v", m)
	}

	s, _ = consolidationService(t, draft, func(s *Service) Message {
		return writeCall("notes/bad.md", noteWith("编造的引文内容", day, "x\n"), "absent")
	})
	if _, err = consolidate(s); err == nil {
		t.Fatal("bad candidate accepted")
	}
	if fileExists(filepath.Join(s.root, "workspace/skills-drafts/weekly-report")) {
		t.Fatal("draft from a rejected consolidation took effect")
	}
}

func TestApproveSkillDraft(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	writeDraft(t, s, "weekly-report", "SKILL.md", skillDraft("weekly-report", "Write the weekly report. Use when asked for one."))
	writeDraft(t, s, "weekly-report", "references/template.md", "# Template\n")
	writeDraft(t, s, "misnamed", "SKILL.md", skillDraft("other", "x"))
	list, err := s.Drafts()
	if err != nil || len(list) != 2 {
		t.Fatal(list, err)
	}
	for _, d := range list {
		if (d.Name == "misnamed") != (d.Problem != "") {
			t.Fatalf("draft %s problem %q", d.Name, d.Problem)
		}
	}
	if err = s.DecideDraft("misnamed", "approve", ""); err == nil {
		t.Fatal("misnamed draft approved")
	}
	if err = s.DecideDraft("weekly-report", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.systemPrompt(), "- weekly-report: Write the weekly report. Use when asked for one.") {
		t.Fatal("approved skill not in the catalog")
	}
	if b, _ := s.workspaceRead("skills/weekly-report/references/template.md"); string(b) != "# Template\n" {
		t.Fatal("skill files not installed")
	}
	// Replacing it keeps the old version.
	writeDraft(t, s, "weekly-report", "SKILL.md", skillDraft("weekly-report", "Second version."))
	if err = s.DecideDraft("weekly-report", "approve", ""); err != nil {
		t.Fatal(err)
	}
	kept, _ := filepath.Glob(filepath.Join(s.root, gateVersions, "weekly-report", "*", "SKILL.md"))
	if len(kept) != 1 {
		t.Fatalf("replaced skill not kept: %v", kept)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, gateLog)); strings.Count(string(b), `"approve"`) != 2 {
		t.Fatalf("decisions not logged: %s", b)
	}
	if !strings.Contains(s.systemPrompt(), "- memory-consolidation:") {
		t.Fatal("built-in skill missing from the catalog")
	}
}

func TestRejectAndIdentityDrafts(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	writeDraft(t, s, "noisy", "SKILL.md", skillDraft("noisy", "x"))
	if err := s.DecideDraft("noisy", "reject", " "); err == nil {
		t.Fatal("rejection without a reason accepted")
	}
	if err := s.DecideDraft("noisy", "reject", "too specific to one ticket"); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(s.root, "workspace/skills-drafts/.rejected/noisy.*/REJECTED.md"))
	if len(matches) != 1 {
		t.Fatalf("rejection not kept: %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); !strings.Contains(string(b), "too specific") {
		t.Fatal("reason not kept")
	}
	if list, _ := s.Drafts(); len(list) != 0 {
		t.Fatalf("rejected draft still listed: %v", list)
	}
	if err := s.DecideDraft("../x", "approve", ""); err == nil {
		t.Fatal("path accepted as a draft name")
	}

	before, _ := s.workspaceRead("IDENTITY.md")
	writeDraft(t, s, identityDraft, "PROPOSAL.md", "- Reply in Chinese. Evidence: \"请用中文\" (sessions/2026-11-20.jsonl)\n")
	if err := s.DecideDraft(identityDraft, "approve", ""); err != nil {
		t.Fatal(err)
	}
	after, _ := s.workspaceRead("IDENTITY.md")
	if !strings.HasPrefix(string(after), strings.TrimRight(string(before), "\n")) || !strings.Contains(string(after), "## Approved rules") || !strings.Contains(string(after), "Reply in Chinese") {
		t.Fatalf("identity not extended: %s", after)
	}
	if kept, _ := filepath.Glob(filepath.Join(s.root, gateVersions, "IDENTITY.md", "*.md")); len(kept) != 1 {
		t.Fatal("old identity not kept")
	}
	if fileExists(s.draftDir(identityDraft)) {
		t.Fatal("approved proposal left as a draft")
	}
}

func TestSkillCatalogIsCapped(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	for i := range maxSkills + 5 {
		name := "skill-" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		p := filepath.Join(s.root, "workspace/skills", name, "SKILL.md")
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(skillDraft(name, "Does "+name+".")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	catalog := s.skillCatalog()
	if len(catalog) > maxCatalogBytes+64 || !strings.Contains(catalog, "more skills not listed") {
		t.Fatalf("catalog not capped: %d bytes", len(catalog))
	}
	if catalog != s.skillCatalog() {
		t.Fatal("catalog order not stable")
	}
	writeDraft(t, s, "one-more", "SKILL.md", skillDraft("one-more", "x"))
	if err := s.DecideDraft("one-more", "approve", ""); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("skill cap not enforced: %v", err)
	}
}
