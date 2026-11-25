package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const editDir = "data/memory/edits"
const editBodies = "data/memory/edit-content"
const pendingEdit = "data/memory/edit-pending.json"
const draftBase = ".owner-base"

type memoryRevision struct {
	ID       string `json:"id"`
	At       string `json:"ts"`
	Actor    string `json:"actor"`
	Path     string `json:"path"`
	Before   string `json:"before"`
	After    string `json:"after"`
	Previous string `json:"previous"`
	Version  string `json:"version"`
	Event    int64  `json:"event,omitempty"`
	Attempt  string `json:"attempt,omitempty"`
	// Masked is set only in workbench responses whose bodies hide secrets.
	Masked bool `json:"masked,omitempty"`
}

func (s *Service) recoverMemoryEdit() error {
	pending := filepath.Join(s.root, pendingEdit)
	b, e := os.ReadFile(pending)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var r memoryRevision
	if e = json.Unmarshal(b, &r); e != nil {
		return e
	}
	cur, e := s.workspaceRead(r.Path)
	if e != nil && !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	if e == nil && digest(cur) == r.Version {
		dir := filepath.Join(s.root, editDir)
		if e = os.MkdirAll(dir, 0700); e != nil {
			return e
		}
		if e = os.MkdirAll(filepath.Join(s.root, editBodies), 0700); e != nil {
			return e
		}
		for hash, body := range map[string]string{r.Previous: r.Before, r.Version: r.After} {
			if hash == "absent" {
				continue
			}
			p := filepath.Join(s.root, editBodies, hash)
			if !fileExists(p) {
				if e = atomicWrite(p, []byte(body), 0600); e != nil {
					return e
				}
			}
		}
		r.Before = ""
		r.After = ""
		meta, e := json.Marshal(r)
		if e != nil {
			return e
		}
		if e = atomicWrite(filepath.Join(dir, r.ID+".json"), meta, 0600); e != nil {
			return e
		}
		if e = os.Remove(pending); e != nil {
			return e
		}
		// The revision is durable; a pruning failure must not fail the write.
		s.logErr("prune memory revisions", s.pruneEdits())
		return nil
	}
	if (e == nil && digest(cur) == r.Previous) || (errors.Is(e, fs.ErrNotExist) && r.Previous == "absent") {
		return os.Remove(pending)
	}
	// The file changed since (a promotion, rollback or edit on the server).
	// The record only holds history and the live file is consistent, so keep
	// the record aside for inspection instead of blocking writes and startup.
	orphan := strings.TrimSuffix(pending, ".json") + "." + time.Now().UTC().Format("20060102T150405Z") + ".orphan.json"
	s.logErr("memory revision recovery", fmt.Errorf("live %s differs from both recorded versions; record kept at %s", r.Path, filepath.Base(orphan)))
	return os.Rename(pending, orphan)
}

// keepEdits bounds the revision log as keepVersions bounds consolidation
// versions. Pruning starts only when the log exceeds the bound by a tenth, so
// most writes skip the directory scan. Bodies are content-addressed and are
// removed once no kept revision refers to them.
const keepEdits = 1000

// pruneEdits runs with memoryMu held or before the service starts.
func (s *Service) pruneEdits() error {
	dir := filepath.Join(s.root, editDir)
	entries, e := os.ReadDir(dir)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var names []string
	for _, d := range entries {
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".json") {
			names = append(names, d.Name())
		}
	}
	if len(names) <= keepEdits+keepEdits/10 {
		return nil
	}
	// Revision IDs start with their UTC time, so name order is age order.
	for _, n := range names[:len(names)-keepEdits] {
		if e = os.Remove(filepath.Join(dir, n)); e != nil && !errors.Is(e, fs.ErrNotExist) {
			return e
		}
	}
	keep := map[string]bool{}
	for _, n := range names[len(names)-keepEdits:] {
		b, e := os.ReadFile(filepath.Join(dir, n))
		if e != nil {
			return e
		}
		var r memoryRevision
		if e = json.Unmarshal(b, &r); e != nil {
			// Without its hashes no body can be proven unused.
			return fmt.Errorf("revision %s: %w", n, e)
		}
		keep[r.Previous], keep[r.Version] = true, true
	}
	bodies, e := os.ReadDir(filepath.Join(s.root, editBodies))
	if e != nil {
		return e
	}
	for _, d := range bodies {
		if !keep[d.Name()] {
			if e = os.Remove(filepath.Join(s.root, editBodies, d.Name())); e != nil && !errors.Is(e, fs.ErrNotExist) {
				return e
			}
		}
	}
	return nil
}

func (s *Service) runWorkspaceWrite(ctx context.Context, name, content, expected string) error {
	name = path.Clean(name)
	if path.Base(name) == draftBase {
		return ErrDenied
	}
	home := s.memoryHome(ctx)
	if strings.HasPrefix(name, draftsPrefix) && home != filepath.Join(s.root, "workspace") {
		part := draftName(name)
		if skillName.MatchString(part) && part != identityDraft {
			s.memoryMu.Lock()
			defer s.memoryMu.Unlock()
			marker := filepath.Join(home, draftsPrefix, part, draftBase)
			if !fileExists(marker) {
				base, e := s.skillFiles(part)
				if e != nil {
					return e
				}
				if e = s.writeWorkspaceLocked(home, name, content, expected, s.cfg.Limits.MaxOutputBytes); e != nil {
					return e
				}
				return atomicWrite(marker, []byte(memoryDigest(base)), 0600)
			}
			return s.writeWorkspaceLocked(home, name, content, expected, s.cfg.Limits.MaxOutputBytes)
		}
	}
	if home != filepath.Join(s.root, "workspace") || !editableNote(name) {
		return s.workspaceWriteIn(s.memoryHome(ctx), name, content, expected)
	}
	sc, _ := ctx.Value(callScopeKey{}).(callScope)
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	return s.reviseLocked(name, content, expected, "agentd", sc)
}
func editableNote(name string) bool {
	return name == "MEMORY.md" || (strings.HasPrefix(name, "notes/") && !frozen(name) && fs.ValidPath(name) && !strings.Contains(name, "/."))
}

// Write-ahead record makes a completed replacement and its version history recoverable together.
func (s *Service) reviseLocked(name, content, expected, actor string, scope callScope) error {
	if e := s.recoverMemoryEdit(); e != nil {
		return e
	}
	old, e := s.workspaceRead(name)
	previous := digest(old)
	if errors.Is(e, fs.ErrNotExist) {
		previous = "absent"
	} else if e != nil {
		return e
	}
	if previous != expected && (actor == "owner" || string(old) != content) {
		// Same hint as other workspace writes: the model should reread.
		return fmt.Errorf("%w: file changed; reread it", ErrConflict)
	}
	if previous != "absent" && string(old) == content {
		return nil
	}
	if actor == "owner" && s.maskedFile(string(old)) != string(old) {
		return errors.New("此文件含受保护凭证，请在服务器上修订")
	}
	now := time.Now().UTC()
	rec := memoryRevision{ID: now.Format("20060102T150405.000000000Z") + "-" + randomID()[:8], At: now.Format(time.RFC3339Nano), Actor: actor, Path: name, Before: string(old), After: content, Previous: previous, Version: digest([]byte(content)), Event: scope.Event, Attempt: scope.Attempt}
	raw, e := json.Marshal(rec)
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(s.root, pendingEdit), raw, 0600); e != nil {
		return e
	}
	limit := s.cfg.Limits.MaxOutputBytes
	if actor == "owner" && strings.HasPrefix(name, draftsPrefix) {
		limit = maxSkillBytes
	}
	e = s.writeWorkspaceLocked(filepath.Join(s.root, "workspace"), name, content, expected, limit)
	return errors.Join(e, s.recoverMemoryEdit())
}

// Effective skill files overlay workspace files on the built-in package, just as workspaceRead does.
func (s *Service) skillFiles(name string) (map[string]string, error) {
	if !skillName.MatchString(name) {
		return nil, ErrDenied
	}
	files := map[string]string{}
	prefix := "defaults/skills/" + name
	err := fs.WalkDir(defaults, prefix, func(p string, d fs.DirEntry, e error) error {
		if errors.Is(e, fs.ErrNotExist) {
			return nil
		}
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		b, e := defaults.ReadFile(p)
		if e == nil {
			files[strings.TrimPrefix(p, prefix+"/")] = string(b)
		}
		return e
	})
	if err != nil {
		return nil, err
	}
	buf := map[string]string{}
	tree := "skills/" + name
	if err = readTree(filepath.Join(s.root, "workspace"), tree, buf); err != nil {
		return nil, err
	}
	for p, v := range buf {
		rel := strings.TrimPrefix(p, tree+"/")
		if rel != draftBase {
			files[rel] = v
		}
	}
	return files, nil
}
func (s *Service) checkDraftBase(name string) error {
	b, e := os.ReadFile(filepath.Join(s.draftDir(name), draftBase))
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	files, e := s.skillFiles(name)
	if e != nil {
		return e
	}
	if memoryDigest(files) != string(b) {
		return ErrConflict
	}
	return nil
}
func (s *Service) makeSkillDraft(name, base string) error {
	files, e := s.skillFiles(name)
	if e != nil {
		return e
	}
	if _, ok := files["SKILL.md"]; !ok {
		return ErrDenied
	}
	if memoryDigest(files) != base || fileExists(s.draftDir(name)) {
		return ErrConflict
	}
	dir, e := os.MkdirTemp(filepath.Dir(s.draftDir(name)), ".agentd-skill-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(dir)
	for rel, v := range files {
		p := filepath.Join(dir, rel)
		if e = os.MkdirAll(filepath.Dir(p), 0700); e != nil {
			return e
		}
		if e = atomicWrite(p, []byte(v), 0600); e != nil {
			return e
		}
	}
	if e = atomicWrite(filepath.Join(dir, draftBase), []byte(base), 0600); e != nil {
		return e
	}
	return os.Rename(dir, s.draftDir(name))
}
