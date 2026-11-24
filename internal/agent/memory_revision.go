package agent

import (
	"context"
	"encoding/json"
	"errors"
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
		return os.Remove(pending)
	}
	if (e == nil && digest(cur) == r.Previous) || (errors.Is(e, fs.ErrNotExist) && r.Previous == "absent") {
		return os.Remove(pending)
	}
	return errors.New("memory revision recovery: live content differs from both recorded versions")
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
		return ErrConflict
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
