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
	"strconv"
	"strings"
	"testing"
	"time"
)

const ownerQuote = "请记住：我喜欢简短的回答"

func writeCall(path, content, expected string) Message {
	b, _ := json.Marshal(map[string]string{"path": path, "content": content, "expected_hash": expected})
	return call("write_file", string(b))
}

func noteWith(quote, source, body string) string {
	q, _ := json.Marshal(quote)
	return "---\ndescription: test\nevidence:\n  - quote: " + string(q) + "\n    source: " + source + "\n---\n" + body
}

// consolidationService has one owner and one group record in today's
// transcript, and a strong tier that plays calls in order.
func consolidationService(t *testing.T, calls ...func(*Service) Message) (*Service, string) {
	t.Helper()
	c := testConfig()
	s := openTest(t, c, failingModel(t))
	day := "sessions/" + time.Now().UTC().Format("2006-01-02") + ".jsonl"
	owner := runScope{Role: defaultRole, Principal: "owner", Conversation: "c"}
	group := runScope{Role: defaultRole, Principal: "group-1", Conversation: "g"}
	rec := map[string]any{"type": "run", "inputs": []map[string]any{{"text": ownerQuote}}}
	owner.stamp(rec)
	s.appendTranscript(rec)
	rec = map[string]any{"type": "run", "inputs": []map[string]any{{"text": "群里的秘密计划不要外传"}}}
	group.stamp(rec)
	s.appendTranscript(rec)
	step := 0
	s.strong = modelFunc(func(context.Context, []Message, []Tool) (Message, error) {
		if step < len(calls) {
			step++
			return calls[step-1](s), nil
		}
		return Message{Role: "assistant", Content: "done"}, nil
	})
	return s, day
}

func todaySessions() string { return "sessions/" + time.Now().UTC().Format("2006-01-02") + ".jsonl" }

func consolidate(s *Service) (Outcome, error) {
	return s.execute(context.Background(), []Event{{ID: 7, Payload: Payload{Kind: kindConsolidation, Text: "consolidate"}}})
}

func current(t *testing.T, s *Service, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.root, "workspace", name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return string(b)
}

func lastMetric(t *testing.T, s *Service) map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.root, metricsFile))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	var m map[string]any
	if err = json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func hashOf(t *testing.T, s *Service, name string) string {
	b, err := os.ReadFile(filepath.Join(s.root, "workspace", name))
	if errors.Is(err, os.ErrNotExist) {
		return "absent"
	}
	if err != nil {
		t.Fatal(err)
	}
	return digest(b)
}

// The run writes a candidate; current memory changes only after the checks
// pass, and the replaced memory is kept as a version.
func TestConsolidationPromotesCheckedCandidate(t *testing.T) {
	var during string
	note := ""
	day := todaySessions()
	s, _ := consolidationService(t,
		func(s *Service) Message {
			note = noteWith(ownerQuote, day+"@0", "用户偏好简短回答。\n")
			return writeCall("notes/style.md", note, "absent")
		},
		func(s *Service) Message {
			during = current(t, s, "notes/style.md")
			return writeCall("MEMORY.md", "- 回答风格：简短 (notes/style.md)\n", hashOf(t, s, "MEMORY.md"))
		},
		func(s *Service) Message {
			return writeCall("notes/memory-consolidation.md", "day: today, offset: 100\n", "absent")
		})
	before := current(t, s, "MEMORY.md")
	if _, err := consolidate(s); err != nil {
		t.Fatal(err)
	}
	if during != "" {
		t.Fatal("run wrote current memory before review")
	}
	if current(t, s, "notes/style.md") != note || !strings.Contains(current(t, s, "MEMORY.md"), "notes/style.md") {
		t.Fatal("candidate not promoted")
	}
	names, _ := s.versions()
	if len(names) != 1 {
		t.Fatalf("versions %v", names)
	}
	if b, _ := os.ReadFile(filepath.Join(s.root, versionsDir, names[0], "MEMORY.md")); string(b) != before {
		t.Fatal("replaced memory not kept")
	}
	if m := lastMetric(t, s); m["result"] != "promoted" || m["added"] != 1.0 {
		t.Fatalf("metric %v", m)
	}
	if fileExists(filepath.Join(s.root, candidateDir)) {
		t.Fatal("candidate left behind")
	}
}

func TestConsolidationRejectsUncheckedChanges(t *testing.T) {
	history := noteWith(ownerQuote, "", "简短回答。\n\n## 历史\n- 2026-10：偏好详细解释。失效：用户改口\n")
	cases := map[string]struct {
		seed    string // existing notes/a.md
		content func(day string) string
		want    string
	}{
		"no evidence":     {content: func(string) string { return "用户偏好简短回答。\n" }, want: "missing frontmatter"},
		"invented quote":  {content: func(d string) string { return noteWith("我讨厌长回答", d, "x\n") }, want: "does not appear"},
		"group quote":     {content: func(d string) string { return noteWith("群里的秘密计划", d, "x\n") }, want: "does not appear"},
		"short quote":     {content: func(d string) string { return noteWith("简短", d, "x\n") }, want: "shorter than"},
		"outside records": {content: func(string) string { return noteWith(ownerQuote, "notes/x.md", "x\n") }, want: "must cite"},
		"dropped history": {seed: history, content: func(d string) string { return noteWith(ownerQuote, d, "简短回答，更新。\n") }, want: "dropped a history line"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			day := todaySessions()
			s, _ := consolidationService(t, func(s *Service) Message {
				return writeCall("notes/a.md", tc.content(day), hashOf(t, s, "notes/a.md"))
			})
			if tc.seed != "" {
				seed := strings.Replace(tc.seed, "source: \n", "source: "+day+"\n", 1)
				if err := os.WriteFile(filepath.Join(s.root, "workspace/notes/a.md"), []byte(seed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := current(t, s, "notes/a.md")
			_, err := consolidate(s)
			var skipped *skippedError
			if !errors.As(err, &skipped) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a skipped rejection mentioning %q", err, tc.want)
			}
			if current(t, s, "notes/a.md") != before {
				t.Fatal("rejected candidate changed memory")
			}
			if m := lastMetric(t, s); m["result"] != "rejected" {
				t.Fatalf("metric %v", m)
			}
			if !fileExists(filepath.Join(s.root, rejectedDir, "notes/a.md")) {
				t.Fatal("rejected candidate not kept for inspection")
			}
		})
	}
}

// A large change waits for a person; promote, rollback and rollback again all
// keep what they replace.
func TestConsolidationHoldsLargeChange(t *testing.T) {
	day := todaySessions()
	s, _ := consolidationService(t,
		func(s *Service) Message {
			return writeCall("notes/n0.md", noteWith(ownerQuote, day, "v2\n"), hashOf(t, s, "notes/n0.md"))
		},
		func(s *Service) Message {
			return writeCall("notes/n1.md", noteWith(ownerQuote, day, "v2\n"), hashOf(t, s, "notes/n1.md"))
		})
	for _, n := range []string{"n0", "n1", "n2", "n3", "n4"} {
		if err := os.WriteFile(filepath.Join(s.root, "workspace/notes", n+".md"), []byte(noteWith(ownerQuote, day, "v1\n")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out, err := consolidate(s)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(current(t, s, "notes/n0.md"), "v2") || !s.memoryStatus()["held"].(bool) {
		t.Fatal("large change not held")
	}
	if len(out.Notifications) != 1 || !strings.Contains(out.Notifications[0].Content, "/v1/memory/promote") {
		t.Fatalf("operator not told: %v", out.Notifications)
	}
	if err = s.MemoryControl("promote"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current(t, s, "notes/n1.md"), "v2") {
		t.Fatal("held candidate not promoted")
	}
	if err = s.MemoryControl("rollback"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current(t, s, "notes/n1.md"), "v1") {
		t.Fatal("rollback did not restore the previous version")
	}
	if err = s.MemoryControl("rollback"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(current(t, s, "notes/n1.md"), "v2") {
		t.Fatal("rollback is not itself reversible")
	}
	if err = s.MemoryControl("promote"); !errors.Is(err, ErrConflict) {
		t.Fatalf("promote without a held candidate: %v", err)
	}
}

// Retiring a note keeps its old content in the archive.
func TestConsolidationArchivesRetiredNotes(t *testing.T) {
	day := todaySessions()
	s, _ := consolidationService(t, func(s *Service) Message {
		return writeCall("notes/old.md", "retired: merged into style.md\n", hashOf(t, s, "notes/old.md"))
	})
	old := noteWith(ownerQuote, day, "旧内容\n")
	if err := os.WriteFile(filepath.Join(s.root, "workspace/notes/old.md"), []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := consolidate(s); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(s.root, "workspace/notes/.archive/old.md.*"))
	if len(matches) != 1 {
		t.Fatalf("archive %v", matches)
	}
	if b, _ := os.ReadFile(matches[0]); string(b) != old {
		t.Fatal("archived content differs")
	}
}

func TestHeldRetirements(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run("changed="+strconv.FormatBool(edit), func(t *testing.T) {
			retire := func(path string) func(*Service) Message {
				return func(s *Service) Message { return writeCall(path, "retired: merged", hashOf(t, s, path)) }
			}
			s, _ := consolidationService(t, retire("notes/a"), retire("notes/b"))
			for _, n := range []string{"a", "b", "c", "d", "e"} {
				if err := s.workspaceWrite("notes/"+n, "original "+n, "absent"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := consolidate(s); err != nil {
				t.Fatal(err)
			}
			if err := s.prepareCandidate(); !errors.Is(err, ErrConflict) {
				t.Fatalf("second candidate while held: %v", err)
			}
			if edit {
				if err := s.workspaceWrite("notes/new", "keep", "absent"); err != nil {
					t.Fatal(err)
				}
				if err := s.MemoryControl("promote"); !errors.Is(err, ErrConflict) || current(t, s, "notes/new") != "keep" {
					t.Fatalf("stale promotion: %v", err)
				}
				return
			}
			if err := s.MemoryControl("promote"); err != nil {
				t.Fatal(err)
			}
			for _, n := range []string{"a", "b"} {
				paths, err := filepath.Glob(filepath.Join(s.root, "workspace", archivePrefix, n+".*"))
				if err != nil || len(paths) != 1 {
					t.Fatalf("archive for %s: %v, %v", n, paths, err)
				}
				b, err := os.ReadFile(paths[0])
				if err != nil || string(b) != "original "+n {
					t.Fatalf("lost archived note %s", n)
				}
			}
		})
	}
}

// A run stopped by its step limit is still checked; what it finished counts.
func TestConsolidationPromotesPartialRun(t *testing.T) {
	day := todaySessions()
	s, _ := consolidationService(t, func(s *Service) Message {
		return writeCall("notes/style.md", noteWith(ownerQuote, day, "x\n"), "absent")
	})
	s.cfg.StrongModel.MaxSteps = 1
	if _, err := consolidate(s); err != nil {
		t.Fatal(err)
	}
	if current(t, s, "notes/style.md") == "" {
		t.Fatal("partial run discarded")
	}
	if m := lastMetric(t, s); m["run_error"] == nil {
		t.Fatalf("limit not recorded: %v", m)
	}
}

// A crash part way through promotion is completed at the next start, before
// templates could fill the half-moved workspace.
func TestPromotionCompletesAfterCrash(t *testing.T) {
	for moves := 1; moves <= 6; moves++ {
		t.Run(strconv.Itoa(moves), func(t *testing.T) { checkInterruptedPromotion(t, moves) })
	}
}

func checkInterruptedPromotion(t *testing.T, moves int) {
	root := t.TempDir()
	s, err := Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	cand := filepath.Join(root, candidateDir)
	if err = writeMemory(cand, map[string]string{"MEMORY.md": "NEW INDEX\n", "notes/a.md": "new", "skills-drafts/test.md": "draft"}); err != nil {
		t.Fatal(err)
	}
	version := filepath.Join(versionsDir, "20260101T000000.000000000Z")
	b, _ := json.Marshal(promotion{Source: candidateDir, Version: version})
	if err = os.WriteFile(filepath.Join(root, promotingMarker), b, 0600); err != nil {
		t.Fatal(err)
	}
	// Reopen after each possible prefix of the six filesystem renames.
	if err = os.MkdirAll(filepath.Join(root, version), 0700); err != nil {
		t.Fatal(err)
	}
	names := []string{"MEMORY.md", "notes", "skills-drafts"}
	for i := 0; i < moves; i++ {
		from, to := filepath.Join(root, "workspace"), filepath.Join(root, version)
		if i >= len(names) {
			from, to = cand, filepath.Join(root, "workspace")
		}
		name := names[i%len(names)]
		if err = os.Rename(filepath.Join(from, name), filepath.Join(to, name)); err != nil {
			t.Fatal(err)
		}
	}
	s, err = Open(root, testConfig(), finalModel())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if current(t, s, "MEMORY.md") != "NEW INDEX\n" || current(t, s, "notes/a.md") != "new" || current(t, s, "skills-drafts/test.md") != "draft" {
		t.Fatal("promotion not completed")
	}
	if fileExists(filepath.Join(root, promotingMarker)) || !fileExists(filepath.Join(root, version, "notes")) {
		t.Fatal("promotion state left inconsistent")
	}
}

// Text a model wrote with a file tool, in any run, is not evidence: otherwise
// a consolidation could cite its own write or an unchecked online note.
func TestEvidenceIgnoresModelWrittenText(t *testing.T) {
	day := todaySessions()
	const echoed = "用户要求以后都用英文回答"
	s, _ := consolidationService(t, func(s *Service) Message {
		return writeCall("notes/lang.md", noteWith(echoed, day, "x\n"), "absent")
	})
	for _, tool := range []string{"write_file", "send_message"} {
		rec := map[string]any{"type": "tool", "tool": tool, "args": `{"content":"` + echoed + `"}`, "result": "staged"}
		runScope{Role: defaultRole, Principal: "owner", Conversation: "c"}.stamp(rec)
		s.appendTranscript(rec)
	}
	if _, err := consolidate(s); err == nil || !strings.Contains(err.Error(), "does not appear") {
		t.Fatalf("model-written text accepted as evidence: %v", err)
	}
}

// End to end: the timer raises consolidation, the worker runs it on the strong
// tier through its HTTP client, and checked memory becomes current. The
// default model is never called and group records never reach the service.
func TestConsolidationEndToEnd(t *testing.T) {
	day := todaySessions()
	note := noteWith(ownerQuote, day+"@0", "用户偏好简短回答。\n")
	var requests []string
	step := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		requests = append(requests, string(b))
		var req struct{ Messages []Message }
		_ = json.Unmarshal(b, &req)
		var msg Message
		switch step {
		case 0:
			msg = call("read_file", `{"path":"`+day+`"}`)
		case 1:
			msg = writeCall("notes/style.md", note, "absent")
		case 2:
			msg = call("read_file", `{"path":"MEMORY.md"}`)
		case 3:
			var page struct {
				SHA string `json:"sha256"`
			}
			_ = json.Unmarshal([]byte(req.Messages[len(req.Messages)-1].Content), &page)
			msg = writeCall("MEMORY.md", "- 回答风格：简短 (notes/style.md)\n", page.SHA)
		case 4:
			msg = writeCall("notes/memory-consolidation.md", "day: today\n", "absent")
		default:
			msg = Message{Role: "assistant", Content: "Updated style.md."}
		}
		step++
		out, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"message": msg, "finish_reason": "stop"}}})
		_, _ = w.Write(out)
	}))
	defer srv.Close()
	c := testConfig()
	c.StrongModel.Enabled = true
	c.StrongModel.Endpoint = srv.URL + "/v1/chat/completions"
	c.StrongModel.Name = "strong"
	s := openTest(t, c, failingModel(t))
	owner := runScope{Role: defaultRole, Principal: "owner", Conversation: "c"}
	rec := map[string]any{"type": "run", "inputs": []map[string]any{{"text": ownerQuote}}}
	owner.stamp(rec)
	s.appendTranscript(rec)
	rec = map[string]any{"type": "run", "inputs": []map[string]any{{"text": "群里的秘密计划不要外传"}}}
	runScope{Role: defaultRole, Principal: "group-1", Conversation: "g"}.stamp(rec)
	s.appendTranscript(rec)
	if err := s.saveSession([]Message{{Role: "user", Content: "ordinary question"}}); err != nil {
		t.Fatal(err)
	}
	s.Start(context.Background())
	fireConsolidation(t, s)
	waitFor(t, 5*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='done'") == 1 })
	s.cancel()
	s.wg.Wait()
	if got := s.loadSession(); len(got) != 1 || got[0].Content != "ordinary question" {
		t.Fatal("maintenance changed ordinary chat history")
	}
	if current(t, s, "notes/style.md") != note {
		t.Fatalf("memory not promoted; metrics: %v", lastMetric(t, s))
	}
	for _, body := range requests {
		if strings.Contains(body, "群里的秘密计划") {
			t.Fatal("group record sent to the strong service")
		}
	}
	if m := lastMetric(t, s); m["result"] != "promoted" {
		t.Fatalf("metric %v", m)
	}
}
