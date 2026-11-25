package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestDreamModelBindingAndReceipts(t *testing.T) {
	var calls [2]atomic.Int32
	cfg := testConfig()
	cfg.DreamModels = map[string]ModelConfig{}
	for i, name := range []string{"openai", "gemini"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct{ Model string }
			if json.NewDecoder(r.Body).Decode(&req) != nil || req.Model != name {
				t.Error("wrong model")
			}
			calls[i].Add(1)
			writeJSON(w, 200, map[string]any{"choices": []map[string]any{{"message": Message{Role: "assistant", Content: "no changes"}}}, "usage": map[string]int{"total_tokens": 7}})
		}))
		t.Cleanup(server.Close)
		cfg.DreamModels[name] = ModelConfig{Name: name, Endpoint: server.URL, TimeoutSeconds: 2}
	}
	s := openTest(t, cfg, nil)
	for i, name := range []string{"openai", "gemini"} {
		id := stableID("test-" + name)
		version := modelVersion(cfg.DreamModels[name])
		ev, e := s.queueDream(t.Context(), id, name, version)
		if e != nil {
			t.Fatal(e)
		}
		same, e := s.queueDream(t.Context(), id, name, version)
		if e != nil || same != ev {
			t.Fatal("retry identity", e, same)
		}
		if i == 0 {
			if _, e = s.queueDream(t.Context(), stableID("other"), name, version); !errors.Is(e, ErrConflict) {
				t.Fatal("queued twice", e)
			}
			s.Start(context.Background())
		}
		waitFor(t, 4*time.Second, func() bool {
			rs, e := s.dreamRuns(t.Context())
			return e == nil && len(rs) > 0 && rs[0].Event == ev && rs[0].Run != "" && rs[0].State == "done"
		})
		rs, _ := s.dreamRuns(t.Context())
		r, e := s.historicalRun(t.Context(), rs[0].Run)
		if e != nil || len(r.Calls) != 1 || r.Calls[0].Model != name || calls[i].Load() != 1 {
			t.Fatal("model receipt", e, calls[i].Load())
		}
	}
	if _, e := s.executeCritical(t.Context(), Event{Payload: Payload{Kind: kindConsolidation, DreamProfile: "gemini", DreamVersion: "changed"}}); e == nil {
		t.Fatal("stale profile executed")
	}
}
func TestOwnerMemoryRevisionAndRecovery(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	d, e := s.memoryDocument("MEMORY.md", "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.editDocument(d.Path, "first revision", d.Version, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = s.editDocument(d.Path, "overwrite", d.Version, ""); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	edits, e := s.revisionList()
	if e != nil || len(edits) != 1 || edits[0].Previous != d.Version {
		t.Fatal(e)
	}
	if _, e = s.editDocument(d.Path, d.Body, edits[0].Version, ""); e != nil {
		t.Fatal(e)
	}
	if _, e = s.editDocument("notes/../IDENTITY.md", "x", "absent", ""); !errors.Is(e, ErrDenied) {
		t.Fatal(e)
	}
	// A committed file replacement survives a process exit before renaming its audit record.
	edits, e = s.revisionList()
	if e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(s.root, editDir, edits[0].ID+".json")
	if e = os.Rename(file, filepath.Join(s.root, pendingEdit)); e != nil {
		t.Fatal(e)
	}
	if e = s.recoverMemoryEdit(); e != nil || !fileExists(file) {
		t.Fatal("lost committed version", e)
	}
}
func TestOwnerSkillReviewConflicts(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	p := "skills/memory-consolidation/SKILL.md"
	d, e := s.memoryDocument(p, "")
	if e != nil {
		t.Fatal(e)
	}
	draft, e := s.editDocument(p, d.Body+"\nReviewed instructions\n", d.Version, d.Base)
	if e != nil {
		t.Fatal(e)
	}
	live, _ := s.workspaceRead(p)
	if string(live) != d.Body {
		t.Fatal("published before approval")
	}
	initial, e := s.draftView(draftInfo{Name: "memory-consolidation"})
	if e != nil {
		t.Fatal(e)
	}
	edit, e := s.memoryDocument(draft, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.editDocument(draft, edit.Body+"\nSecond edit\n", edit.Version, ""); e != nil {
		t.Fatal(e)
	}
	if e = s.reviewMemory("memory-consolidation", "approve", initial.Token, ""); !errors.Is(e, ErrConflict) {
		t.Fatal("stale review", e)
	}
	latest, e := s.draftView(draftInfo{Name: "memory-consolidation"})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.reviewMemory("memory-consolidation", "approve", latest.Token, ""); e != nil {
		t.Fatal(e)
	}
	restored, _ := s.memoryDocument(p, "builtin")
	active, _ := s.memoryDocument(p, "")
	if restored.Body != d.Body || !strings.Contains(active.Body, "Second edit") {
		t.Fatal("version history")
	}
	if _, e = s.editDocument(p, restored.Body, active.Version, active.Base); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.root, "workspace", p), []byte(d.Body+"\nchanged externally"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.DecideDraft("memory-consolidation", "approve", ""); !errors.Is(e, ErrConflict) {
		t.Fatal("superseded skill published", e)
	}
}

func TestSkillPublicationRecovery(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	name := "memory-consolidation"
	p := "skills/" + name + "/SKILL.md"
	live, _ := s.memoryDocument(p, "")
	draft, e := s.editDocument(p, live.Body+"\nDurable revision", live.Version, live.Base)
	if e != nil {
		t.Fatal(e)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	entry, _ := json.Marshal(map[string]string{"ts": "2026-01-01T00:00:00Z", "draft": name, "decision": "approve"})
	marker, _ := json.Marshal(skillPublish{Name: name, Stamp: stamp, Entry: entry})
	// Simulate exit after the draft moved but before its receipt was committed.
	if e = os.Rename(s.draftDir(name), filepath.Join(s.root, "workspace/skills", name)); e != nil {
		t.Fatal(e)
	}
	if e = atomicWrite(filepath.Join(s.root, publishingSkill), marker, 0600); e != nil {
		t.Fatal(e)
	}
	for range 2 {
		if e = s.finishSkillPublish(); e != nil {
			t.Fatal(e)
		}
	}
	rec, e := s.recentMemoryLog(gateLog)
	if e != nil || len(rec) != 1 || rec[0]["draft"] != name {
		t.Fatal("approval receipt", e)
	}
	if fileExists(filepath.Join(s.root, "workspace", draft)) {
		t.Fatal("draft still active")
	}
}

// A revision record left behind when its write was later replaced (by a
// promotion, rollback or server edit) must not block writes or startup.
func TestOrphanedRevisionRecordDoesNotBlock(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, testConfig(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	raw, _ := json.Marshal(memoryRevision{ID: "20260101T000000.000000000Z-abcdef01", Path: "MEMORY.md", Previous: "gone", Version: "also-gone"})
	if err = os.WriteFile(filepath.Join(root, pendingEdit), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(root, testConfig(), nil)
	if err != nil {
		t.Fatal("orphaned record stopped startup:", err)
	}
	defer s.Close()
	if fileExists(filepath.Join(root, pendingEdit)) {
		t.Fatal("orphaned record left pending")
	}
	orphans, _ := filepath.Glob(filepath.Join(root, "data/memory/edit-pending.*.orphan.json"))
	if len(orphans) != 1 {
		t.Fatal("orphaned record not kept for inspection")
	}
	cur, err := s.workspaceRead("MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.editDocument("MEMORY.md", "after", digest(cur), ""); err != nil {
		t.Fatal("write after orphaned record:", err)
	}
}

// Owner edits wait for a running consolidation; any edit would make its
// candidate stale.
func TestOwnerEditsWaitForConsolidation(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	cur, err := s.workspaceRead("MEMORY.md")
	if err != nil {
		t.Fatal(err)
	}
	s.consolidating.Store(true)
	defer s.consolidating.Store(false)
	if _, err = s.editDocument("MEMORY.md", "edited", digest(cur), ""); !errors.Is(err, ErrConflict) {
		t.Fatal("note edit during consolidation:", err)
	}
}

// A skill with a pending draft is revised through the draft; a masked
// version cannot be restored.
func TestSkillDraftAndMaskedViews(t *testing.T) {
	cfg := testConfig()
	cfg.Model.APIKey = "sk-view-mask-0123456789"
	s := openTest(t, cfg, nil)
	const skill = "skills/memory-consolidation/SKILL.md"
	if v, err := s.memoryDocument(skill, ""); err != nil || !v.Writable || v.Draft != "" {
		t.Fatalf("skill without draft: %+v %v", v, err)
	}
	draft := filepath.Join(s.root, "workspace", draftsPrefix, "memory-consolidation")
	if err := os.MkdirAll(draft, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(draft, "SKILL.md"), []byte("---\nname: memory-consolidation\ndescription: d\n---\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if v, err := s.memoryDocument(skill, ""); err != nil || v.Writable || v.Draft != draftsPrefix+"memory-consolidation/SKILL.md" {
		t.Fatalf("skill with draft: %+v %v", v, err)
	}
	if err := s.workspaceWrite("notes/key.md", "key "+cfg.Model.APIKey, "absent"); err != nil {
		t.Fatal(err)
	}
	if v, err := s.memoryDocument("notes/key.md", ""); err != nil || !v.Masked || v.Writable {
		t.Fatalf("masked note: %+v %v", v, err)
	}
}
