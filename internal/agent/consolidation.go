package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"
)

// Consolidation never edits memory in place. Its run reads and writes a
// candidate copy of MEMORY.md and notes/; afterwards the host checks the
// candidate and only then promotes it, keeping the version it replaces.
// The checks are plain string and file comparisons, so they hold however
// capable the model becomes.
const (
	candidateDir    = "data/memory/candidate"
	heldDir         = "data/memory/held"
	rejectedDir     = "data/memory/rejected"
	versionsDir     = "data/memory/versions"
	promotingMarker = "data/memory/promoting.json"
	metricsFile     = "data/memory/consolidation.jsonl"

	keepVersions   = 10
	progressNote   = "notes/memory-consolidation.md"
	archivePrefix  = "notes/.archive/"
	minQuoteRunes  = 4
	holdShare      = 0.3 // changing more than this share of memory needs a person
	holdMinEntries = 5   // below this many entries any share is allowed
	transcriptScan = 64 << 20
)

type candidateKey struct{}

// memoryHome is where this run's memory paths resolve.
func (s *Service) memoryHome(ctx context.Context) string {
	if v, _ := ctx.Value(candidateKey{}).(bool); v {
		return filepath.Join(s.root, candidateDir)
	}
	return filepath.Join(s.root, "workspace")
}

// memoryTrees are the directories a candidate copies besides MEMORY.md.
var memoryTrees = []string{"notes", "skills-drafts"}

// readMemory loads MEMORY.md and every file under memoryTrees in dir, keyed
// by model path.
func readMemory(dir string) (map[string]string, error) {
	files := map[string]string{}
	if b, e := os.ReadFile(filepath.Join(dir, "MEMORY.md")); e == nil {
		files["MEMORY.md"] = string(b)
	} else if !errors.Is(e, os.ErrNotExist) {
		return nil, e
	}
	for _, tree := range memoryTrees {
		if e := readTree(dir, tree, files); e != nil {
			return nil, e
		}
	}
	return files, nil
}

func readTree(dir, tree string, files map[string]string) error {
	notes := filepath.Join(dir, tree)
	return filepath.WalkDir(notes, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return errors.New("memory cannot contain symlinks")
		}
		if !d.Type().IsRegular() || strings.HasPrefix(d.Name(), ".agentd-") {
			return nil
		}
		rel, err := filepath.Rel(notes, p)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		files[tree+"/"+filepath.ToSlash(rel)] = string(b)
		return err
	})
}

func writeMemory(dir string, files map[string]string) error {
	for _, tree := range memoryTrees {
		if e := os.MkdirAll(filepath.Join(dir, tree), 0700); e != nil {
			return e
		}
	}
	for name, content := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if e := atomicWrite(p, []byte(content), 0600); e != nil {
			return e
		}
	}
	return nil
}

// prepareCandidate starts a consolidation from a copy of current memory.
func (s *Service) prepareCandidate() error {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	dir := filepath.Join(s.root, candidateDir)
	if e := os.RemoveAll(dir); e != nil {
		return e
	}
	cur, e := readMemory(filepath.Join(s.root, "workspace"))
	if e != nil {
		return e
	}
	return writeMemory(dir, cur)
}

// review is the host's verdict on a candidate.
type review struct {
	Added, Changed, Retired []string
	Drafts                  []string // skill drafts added or changed, by name
	Failures                []string
	Hold                    string
}

func (r review) changed() bool { return len(r.Added)+len(r.Changed)+len(r.Retired)+len(r.Drafts) > 0 }

// entryPath reports whether a model path is a memory entry the checks cover:
// a note other than the progress note and the archive. Drafts are reviewed by
// a person at the gate instead.
func entryPath(p string) bool {
	return strings.HasPrefix(p, "notes/") && p != progressNote && !strings.HasPrefix(p, archivePrefix)
}

// frozen reports whether a path is a record that no run may change.
func frozen(p string) bool {
	return strings.HasPrefix(p, archivePrefix) || strings.HasPrefix(p, rejectedDrafts)
}

func (s *Service) reviewCandidate(cur, cand map[string]string) review {
	var r review
	fail := func(format string, args ...any) { r.Failures = append(r.Failures, fmt.Sprintf(format, args...)) }
	idx := cand["MEMORY.md"]
	if n := lineCount(idx); n > memoryIndexMaxLines || len(idx) > memoryIndexMaxBytes {
		fail("MEMORY.md has %d lines and %d bytes; the limit is %d lines and %d bytes", n, len(idx), memoryIndexMaxLines, memoryIndexMaxBytes)
	}
	for _, p := range slices.Sorted(mapKeys(cur)) {
		if _, ok := cand[p]; !ok {
			fail("%s was deleted; retire it instead", p)
		} else if frozen(p) && cand[p] != cur[p] {
			fail("%s is a record and cannot change", p)
		}
	}
	for p := range cand {
		if _, ok := cur[p]; !ok && frozen(p) {
			fail("%s is a record and cannot be added", p)
		}
	}
	for _, p := range slices.Sorted(mapKeys(cand)) {
		if strings.HasPrefix(p, draftsPrefix) && cand[p] != cur[p] {
			if name := draftName(p); !slices.Contains(r.Drafts, name) {
				r.Drafts = append(r.Drafts, name)
			}
		}
	}
	transcripts := map[string]string{}
	entries := 0
	for _, p := range slices.Sorted(mapKeys(cur)) {
		if entryPath(p) && !retired(cur[p]) {
			entries++
		}
	}
	for _, p := range slices.Sorted(mapKeys(cand)) {
		v := cand[p]
		old, existed := cur[p]
		if !entryPath(p) || (existed && old == v) {
			continue
		}
		if retired(v) {
			if existed && !retired(old) {
				r.Retired = append(r.Retired, p)
			}
			continue
		}
		if existed {
			r.Changed = append(r.Changed, p)
			kept := historyLines(v)
			for _, line := range historyLines(old) {
				if !slices.Contains(kept, line) {
					fail("%s dropped a history line: %q", p, line)
				}
			}
		} else {
			r.Added = append(r.Added, p)
		}
		evidence, err := parseEvidence(v)
		if err != nil {
			fail("%s: %v", p, err)
			continue
		}
		if len(evidence) == 0 {
			fail("%s has no evidence", p)
		}
		for _, ev := range evidence {
			if utf8.RuneCountInString(ev.Quote) < minQuoteRunes {
				fail("%s: quote %q is shorter than %d characters", p, ev.Quote, minQuoteRunes)
			} else if ok, err := s.quoteFound(ev.Source, ev.Quote, transcripts); err != nil {
				fail("%s: source %q: %v", p, ev.Source, err)
			} else if !ok {
				fail("%s: quote %q does not appear in %s", p, ev.Quote, ev.Source)
			}
		}
	}
	if touched := len(r.Changed) + len(r.Retired); entries >= holdMinEntries && float64(touched) > holdShare*float64(entries) {
		r.Hold = fmt.Sprintf("changes %d of %d memory entries", touched, entries)
	}
	oldIndex := strings.Split(strings.TrimSpace(cur["MEMORY.md"]), "\n")
	removed := 0
	for _, line := range oldIndex {
		if line != "" && !strings.Contains(idx, line) {
			removed++
		}
	}
	if len(oldIndex) >= 2*holdMinEntries && float64(removed) > holdShare*float64(len(oldIndex)) {
		r.Hold = strings.TrimPrefix(r.Hold+fmt.Sprintf("; removes %d of %d MEMORY.md lines", removed, len(oldIndex)), "; ")
	}
	return r
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func retired(v string) bool { return strings.HasPrefix(strings.TrimSpace(v), "retired:") }

// historyLines returns the nonblank lines of an entry's "## 历史" section.
// History only grows: facts that turned out wrong are kept there with why.
func historyLines(v string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(v, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "## ") {
			in = t == "## 历史" || strings.EqualFold(t, "## History")
			continue
		}
		if in && t != "" {
			out = append(out, t)
		}
	}
	return out
}

type evidence struct {
	Quote  string `yaml:"quote"`
	Source string `yaml:"source"`
}

func parseEvidence(v string) ([]evidence, error) {
	rest, ok := strings.CutPrefix(v, "---\n")
	if !ok {
		return nil, errors.New("missing frontmatter with evidence")
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return nil, errors.New("frontmatter is not closed with ---")
	}
	var meta struct {
		Evidence []evidence `yaml:"evidence"`
	}
	if e := yaml.Unmarshal([]byte(front), &meta); e != nil {
		return nil, fmt.Errorf("frontmatter: %w", e)
	}
	return meta.Evidence, nil
}

// quoteFound reports whether quote appears word for word in what a cited
// transcript observed: the inputs and answers of runs and the results of
// tools, in records this run was allowed to read.
// Tool arguments are what a model chose to write, so they prove nothing.
// cache holds each transcript's searchable text.
func (s *Service) quoteFound(source, quote string, cache map[string]string) (bool, error) {
	name, _, _ := strings.Cut(source, "@")
	if !strings.HasPrefix(name, "sessions/") {
		return false, errors.New("evidence must cite sessions/YYYY-MM-DD.jsonl")
	}
	text, ok := cache[name]
	if !ok {
		f, size, err := s.openTranscript(name, 0)
		if err != nil {
			return false, err
		}
		raw, err := io.ReadAll(io.LimitReader(f, transcriptScan))
		f.Close()
		if err != nil {
			return false, err
		}
		if size > transcriptScan {
			s.log.Warn("evidence check scans only the start of a large transcript", "transcript", name)
		}
		var b strings.Builder
		for _, line := range bytes.Split(raw, []byte("\n")) {
			if !egressVisible(line) {
				continue
			}
			var rec map[string]any
			if json.Unmarshal(line, &rec) != nil {
				continue
			}
			for _, v := range observed(rec) {
				walkStrings(v, func(v string) { b.WriteString(v); b.WriteByte(0) })
			}
		}
		text = b.String()
		cache[name] = text
	}
	return strings.Contains(text, quote), nil
}

func observed(rec map[string]any) []any {
	switch rec["type"] {
	case "run":
		var out []any
		if inputs, ok := rec["inputs"].([]any); ok {
			for _, in := range inputs {
				if m, ok := in.(map[string]any); ok {
					out = append(out, m["text"])
				}
			}
		}
		return append(out, rec["answer"])
	case "tool":
		return []any{rec["result"]}
	}
	return nil
}

func walkStrings(v any, fn func(string)) {
	switch t := v.(type) {
	case string:
		fn(t)
	case []any:
		for _, x := range t {
			walkStrings(x, fn)
		}
	case map[string]any:
		for _, x := range t {
			walkStrings(x, fn)
		}
	}
}

// finishConsolidation checks the candidate a consolidation run left and
// promotes, holds or rejects it. A run that stopped at its step or context
// limit is still checked: the skill records progress as it goes.
func (s *Service) finishConsolidation(ev Event, out Outcome, runErr error) (Outcome, error) {
	metric := map[string]any{"event": ev.ID, "model": s.cfg.StrongModel.Name}
	if runErr != nil {
		metric["run_error"] = s.bounded(runErr.Error())
	}
	partial := errors.Is(runErr, errMaxSteps) || errors.Is(runErr, errContextBudget)
	if runErr != nil && !partial {
		metric["result"] = "failed"
		s.recordConsolidation(metric)
		return out, errors.Join(runErr, s.setAside(rejectedDir))
	}
	cur, e := readMemory(filepath.Join(s.root, "workspace"))
	if e != nil {
		return out, e
	}
	cand, e := readMemory(filepath.Join(s.root, candidateDir))
	if e != nil {
		return out, e
	}
	r := s.reviewCandidate(cur, cand)
	idx := cand["MEMORY.md"]
	metric["index_lines"], metric["index_bytes"] = lineCount(idx), len(idx)
	metric["added"], metric["changed"], metric["retired"], metric["drafts"] = len(r.Added), len(r.Changed), len(r.Retired), len(r.Drafts)
	switch {
	case len(r.Failures) > 0:
		metric["result"], metric["failures"] = "rejected", r.Failures
		s.recordConsolidation(metric)
		return out, errors.Join(fmt.Errorf("consolidation rejected: %s", strings.Join(r.Failures, "; ")), s.setAside(rejectedDir))
	case cand[progressNote] == cur[progressNote] && !r.changed() && cand["MEMORY.md"] == cur["MEMORY.md"]:
		metric["result"] = "unchanged"
		s.recordConsolidation(metric)
		return out, s.setAside("")
	case r.Hold != "":
		metric["result"], metric["hold"] = "held", r.Hold
		s.recordConsolidation(metric)
		if e = s.setAside(heldDir); e != nil {
			return out, e
		}
		out.Notifications = append(out.Notifications, Notification{Key: fmt.Sprintf("memory-held:%d", ev.ID), Target: s.cfg.DefaultTarget, Content: "Memory consolidation is held for review: it " + r.Hold + ". Promote it with POST /v1/memory/promote or drop it with POST /v1/memory/discard.", DeliverAt: time.Now().UnixMilli()})
		return out, nil
	}
	for _, p := range r.Retired {
		cand[archivePrefix+strings.TrimPrefix(p, "notes/")+"."+time.Now().UTC().Format("20060102T150405Z")] = cur[p]
	}
	if e = writeMemory(filepath.Join(s.root, candidateDir), cand); e != nil {
		return out, e
	}
	if e = s.promote(candidateDir); e != nil {
		return out, e
	}
	metric["result"] = "promoted"
	s.recordConsolidation(metric)
	if len(r.Drafts) > 0 {
		out.Notifications = append(out.Notifications, s.draftsNotice(ev.ID, r.Drafts))
	}
	return out, nil
}

// setAside moves the candidate to dir for inspection, replacing what was
// there, or removes it when dir is empty.
func (s *Service) setAside(dir string) error {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	src := filepath.Join(s.root, candidateDir)
	if dir == "" {
		return os.RemoveAll(src)
	}
	dst := filepath.Join(s.root, dir)
	if e := os.RemoveAll(dst); e != nil {
		return e
	}
	return os.Rename(src, dst)
}

func (s *Service) recordConsolidation(metric map[string]any) {
	metric["ts"] = time.Now().UTC().Format(time.RFC3339)
	b, e := json.Marshal(metric)
	if e != nil {
		s.logErr("consolidation metrics", e)
		return
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	f, e := os.OpenFile(filepath.Join(s.root, metricsFile), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		s.logErr("consolidation metrics", e)
		return
	}
	_, e = f.Write(append(b, '\n'))
	s.logErr("consolidation metrics", errors.Join(e, f.Close()))
}

type promotion struct {
	Source  string `json:"source"`
	Version string `json:"version"`
}

// promote makes src the current memory and keeps the replaced memory as a new
// version. A marker records the move, so a crash part way is completed by
// finishPromotion at the next start.
func (s *Service) promote(src string) error {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	p := promotion{Source: src, Version: path.Join(versionsDir, time.Now().UTC().Format("20060102T150405.000000000Z"))}
	b, _ := json.Marshal(p)
	if e := atomicWrite(filepath.Join(s.root, promotingMarker), b, 0600); e != nil {
		return e
	}
	return s.finishPromotion()
}

// finishPromotion completes a recorded promotion. Each move runs only while
// its source exists and its target does not, so repeating it is harmless.
// Caller holds memoryMu or is Open.
func (s *Service) finishPromotion() error {
	b, e := os.ReadFile(filepath.Join(s.root, promotingMarker))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var p promotion
	if e = json.Unmarshal(b, &p); e != nil {
		return e
	}
	ws := filepath.Join(s.root, "workspace")
	src, version := filepath.Join(s.root, p.Source), filepath.Join(s.root, p.Version)
	if e = os.MkdirAll(version, 0700); e != nil {
		return e
	}
	for _, m := range [][2]string{
		{filepath.Join(ws, "MEMORY.md"), filepath.Join(version, "MEMORY.md")},
		{filepath.Join(ws, "notes"), filepath.Join(version, "notes")},
		{filepath.Join(ws, "skills-drafts"), filepath.Join(version, "skills-drafts")},
		{filepath.Join(src, "MEMORY.md"), filepath.Join(ws, "MEMORY.md")},
		{filepath.Join(src, "notes"), filepath.Join(ws, "notes")},
		{filepath.Join(src, "skills-drafts"), filepath.Join(ws, "skills-drafts")},
	} {
		if fileExists(m[0]) && !fileExists(m[1]) {
			if e = os.Rename(m[0], m[1]); e != nil {
				return e
			}
		}
	}
	for _, tree := range memoryTrees {
		if e = os.MkdirAll(filepath.Join(ws, tree), 0700); e != nil {
			return e
		}
	}
	if e = errors.Join(syncDir(ws), os.RemoveAll(src)); e != nil {
		return e
	}
	if e = os.Remove(filepath.Join(s.root, promotingMarker)); e != nil {
		return e
	}
	return s.pruneVersions()
}

func (s *Service) versions() ([]string, error) {
	entries, e := os.ReadDir(filepath.Join(s.root, versionsDir))
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	var names []string
	for _, d := range entries {
		if d.IsDir() {
			names = append(names, d.Name())
		}
	}
	slices.Sort(names)
	return names, e
}

func (s *Service) pruneVersions() error {
	names, e := s.versions()
	for len(names) > keepVersions && e == nil {
		e = os.RemoveAll(filepath.Join(s.root, versionsDir, names[0]))
		names = names[1:]
	}
	return e
}

// MemoryControl lets an operator act on consolidation results: promote or
// discard a held candidate, or roll memory back to the previous version.
// Each change keeps the memory it replaces as a version, so it can be undone.
func (s *Service) MemoryControl(op string) error {
	if s.consolidating.Load() {
		return fmt.Errorf("%w: a consolidation is running", ErrConflict)
	}
	switch op {
	case "promote":
		if !fileExists(filepath.Join(s.root, heldDir)) {
			return fmt.Errorf("%w: no held consolidation", ErrConflict)
		}
		return s.promote(heldDir)
	case "discard":
		return os.RemoveAll(filepath.Join(s.root, heldDir))
	case "rollback":
		names, e := s.versions()
		if e != nil {
			return e
		}
		if len(names) == 0 {
			return fmt.Errorf("%w: no earlier version", ErrConflict)
		}
		return s.promote(path.Join(versionsDir, names[len(names)-1]))
	}
	return errors.New("operation must be promote, discard or rollback")
}

// memoryStatus summarizes consolidation state for GET /v1/status.
func (s *Service) memoryStatus() map[string]any {
	names, _ := s.versions()
	drafts, _ := s.Drafts()
	return map[string]any{"held": fileExists(filepath.Join(s.root, heldDir)), "versions": len(names), "strong_model": s.strong != nil, "drafts": len(drafts)}
}
