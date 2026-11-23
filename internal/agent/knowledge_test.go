package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func importAndWait(t *testing.T, s *Service, path string) []importResult {
	t.Helper()
	if err := s.StartImport(path); err != nil {
		t.Fatal(err)
	}
	var last []importResult
	waitFor(t, 5*time.Second, func() bool {
		s.knowledge.mu.Lock()
		defer s.knowledge.mu.Unlock()
		last = s.knowledge.last
		return !s.knowledge.running
	})
	return last
}

func statusOf(results []importResult, suffix string) string {
	for _, r := range results {
		if strings.HasSuffix(r.Source, suffix) {
			return r.Status + r.Error
		}
	}
	return "missing"
}

func knowledgeService(t *testing.T) (*Service, string) {
	c := testConfig()
	c.Knowledge.Converter = []string{"fake-converter"}
	s := openTest(t, c, finalModel())
	s.convert = func(_ context.Context, source string, raw []byte) ([]byte, error) {
		return []byte("# Converted " + filepath.Base(source) + "\n\n" + string(raw)), nil
	}
	src := t.TempDir()
	for name, content := range map[string]string{
		"handbook.md":       "# 员工手册\n\n报销需在 30 天内提交。\n",
		"notes/faq.txt":     "常见问题：VPN 地址是 vpn.example.com\n",
		"report.pdf":        "PDF body about quarterly revenue",
		"image.png":         "ignored",
		".hidden/secret.md": "# hidden\n",
	} {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return s, src
}

// Imported documents become read-only Markdown with an index that loads into
// every run and that search covers.
func TestKnowledgeImport(t *testing.T) {
	s, src := knowledgeService(t)
	results := importAndWait(t, s, src)
	for _, f := range []string{"handbook.md", "faq.txt", "report.pdf"} {
		if st := statusOf(results, f); st != "added" {
			t.Fatalf("%s: %s", f, st)
		}
	}
	if len(results) != 3 {
		t.Fatalf("imported %v; hidden directories and unsupported files must be skipped", results)
	}
	prompt := s.systemPrompt()
	if !strings.Contains(prompt, "- 员工手册 (knowledge/md/") || !strings.Contains(prompt, "- Converted report.pdf") {
		t.Fatalf("index not loaded:\n%s", prompt)
	}
	id := docID(filepath.Join(src, "handbook.md"))
	b, err := s.workspaceRead("knowledge/md/" + id + ".md")
	if err != nil || !strings.Contains(string(b), "30 天") {
		t.Fatal(err)
	}
	if err = s.workspaceWrite("knowledge/md/"+id+".md", "x", digest(b)); !errors.Is(err, ErrDenied) {
		t.Fatalf("knowledge writable: %v", err)
	}
	res, err := runSearch(t, s, context.Background(), `{"query":"VPN"}`)
	if err != nil || len(res.Hits) != 1 || !strings.HasPrefix(res.Hits[0].Path, "knowledge/md/") {
		t.Fatalf("search: %v %+v", err, res)
	}
	if !fileExists(filepath.Join(s.root, knowledgeRaw, docID(filepath.Join(src, "report.pdf"))+".pdf")) {
		t.Fatal("original not kept")
	}
}

// Re-importing is idempotent; a changed file replaces its Markdown and the
// old one is archived; a file gone from an imported directory is archived.
func TestKnowledgeReimport(t *testing.T) {
	s, src := knowledgeService(t)
	importAndWait(t, s, src)
	results := importAndWait(t, s, src)
	for _, r := range results {
		if r.Status != "unchanged" {
			t.Fatalf("re-import changed %v", r)
		}
	}
	if err := os.WriteFile(filepath.Join(src, "handbook.md"), []byte("# 员工手册\n\n报销需在 60 天内提交。\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(src, "notes/faq.txt")); err != nil {
		t.Fatal(err)
	}
	results = importAndWait(t, s, src)
	if statusOf(results, "handbook.md") != "updated" || statusOf(results, "faq.txt") != "archived" {
		t.Fatalf("results %v", results)
	}
	res, _ := runSearch(t, s, context.Background(), `{"query":"天内提交"}`)
	if len(res.Hits) != 1 || !strings.Contains(res.Hits[0].Snippet, "60") {
		t.Fatalf("search sees archived versions: %+v", res.Hits)
	}
	if strings.Contains(s.systemPrompt(), "faq") {
		t.Fatal("removed document still indexed")
	}
	archived, _ := filepath.Glob(filepath.Join(s.root, knowledgeDir, ".archive", "*.md"))
	if len(archived) != 2 {
		t.Fatalf("archive %v", archived)
	}
	k, _ := s.Knowledge()
	if docs := k["documents"].([]knowledgeDoc); len(docs) != 2 {
		t.Fatalf("manifest %v", docs)
	}
	if err := s.RemoveKnowledge(docID(filepath.Join(src, "report.pdf"))); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.systemPrompt(), "report.pdf") {
		t.Fatal("removed document still indexed")
	}
}

func TestKnowledgeRefusals(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if st := statusOf(importAndWait(t, s, write("a.pdf", "x")), "a.pdf"); !strings.Contains(st, "needs knowledge.converter") {
		t.Fatalf("pdf without converter: %s", st)
	}
	if st := statusOf(importAndWait(t, s, write("b.txt", "\xff\xfe")), "b.txt"); !strings.Contains(st, "UTF-8") {
		t.Fatalf("binary text: %s", st)
	}
	big := strings.Repeat("x", maxMarkdownBytes+1)
	if st := statusOf(importAndWait(t, s, write("c.md", big)), "c.md"); !strings.Contains(st, "split") {
		t.Fatalf("oversized: %s", st)
	}
	// The index has the same cap as MEMORY.md.
	many := t.TempDir()
	for i := range memoryIndexMaxLines + 5 {
		if err := os.WriteFile(filepath.Join(many, fmt.Sprintf("doc-%03d.md", i)), []byte(fmt.Sprintf("# Doc %d\n", i)), 0600); err != nil {
			t.Fatal(err)
		}
	}
	results := importAndWait(t, s, many)
	failed := slices.IndexFunc(results, func(r importResult) bool { return strings.Contains(r.Error, "INDEX.md would have") })
	if failed < 0 {
		t.Fatal("index cap not enforced")
	}
	idx, _ := s.workspaceRead("knowledge/INDEX.md")
	if lineCount(string(idx)) > memoryIndexMaxLines || len(idx) > memoryIndexMaxBytes {
		t.Fatal("index exceeds its cap")
	}
	// One import at a time.
	s.knowledge.mu.Lock()
	s.knowledge.running = true
	s.knowledge.mu.Unlock()
	if err := s.StartImport(dir); !errors.Is(err, ErrConflict) {
		t.Fatalf("concurrent import: %v", err)
	}
	// Removing during an import would be undone when the import saves.
	if err := s.RemoveKnowledge("anything"); !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "import is running") {
		t.Fatalf("remove during import: %v", err)
	}
	s.knowledge.mu.Lock()
	s.knowledge.running = false
	s.knowledge.mu.Unlock()
}

// Consolidation may cite knowledge as evidence.
func TestKnowledgeAsEvidence(t *testing.T) {
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "policy.md"), []byte("# 报销制度\n报销需在 30 天内提交。\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := docID(filepath.Join(src, "policy.md"))
	s, _ := consolidationService(t, func(s *Service) Message {
		return writeCall("notes/expense.md", noteWith("报销需在 30 天内提交", "knowledge/md/"+id+".md", "报销期限 30 天。\n"), "absent")
	})
	importAndWait(t, s, src)
	if _, err := consolidate(s); err != nil {
		t.Fatal(err)
	}
	if current(t, s, "notes/expense.md") == "" {
		t.Fatal("knowledge evidence rejected")
	}
}

// The converter runs in the same sandbox as bash, seeing only what it needs.
func TestConverterSandboxArguments(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("argument check written for bubblewrap")
	}
	cmd, err := sandboxed(context.Background(), []string{"/usr/bin/conv", "/w/input.pdf"}, []string{"/opt/conv"}, "/w")
	if err != nil {
		t.Skip(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, want := range []string{"--unshare-all", "--ro-bind /opt/conv /opt/conv", "--bind /w /w", "/usr/bin/conv /w/input.pdf"} {
		if !strings.Contains(args, want) {
			t.Fatalf("missing %q in %s", want, args)
		}
	}
	if strings.Contains(args, os.Getenv("HOME")+" ") {
		t.Fatal("home directory exposed")
	}
}
