package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The gate is where writes that change future behavior wait for a person.
// Critical runs draft skills under skills-drafts/<name>/ and rule changes
// under skills-drafts/identity/; nothing there reaches a run's context until
// an operator approves it. Every decision is logged and every replaced file
// is kept.
const (
	draftsPrefix   = "skills-drafts/"
	rejectedDrafts = "skills-drafts/.rejected/"
	identityDraft  = "identity"
	gateVersions   = "data/gate/versions"
	gateLog        = "data/gate/decisions.jsonl"

	maxSkills         = 50
	maxSkillBytes     = 256 << 10
	maxProposalBytes  = 4 << 10
	maxCatalogBytes   = 4 << 10
	maxDescriptionLen = 1024
)

var skillName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// draftName returns the draft a model path under skills-drafts/ belongs to.
func draftName(p string) string {
	name, _, _ := strings.Cut(strings.TrimPrefix(p, draftsPrefix), "/")
	return name
}

func (s *Service) draftsNotice(eventID int64, names []string) Notification {
	return Notification{Key: fmt.Sprintf("drafts:%d", eventID), Target: s.cfg.DefaultTarget, DeliverAt: time.Now().UnixMilli(),
		Content: "Drafts waiting for review: " + strings.Join(names, ", ") + ". List them with GET /v1/drafts; decide with POST /v1/drafts/{name}/approve or /reject."}
}

type skillMeta struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

func parseSkill(b []byte) (skillMeta, error) {
	var m skillMeta
	rest, ok := strings.CutPrefix(string(b), "---\n")
	if !ok {
		return m, errors.New("SKILL.md must start with frontmatter")
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return m, errors.New("SKILL.md frontmatter is not closed with ---")
	}
	if e := yaml.Unmarshal([]byte(front), &m); e != nil {
		return m, fmt.Errorf("SKILL.md frontmatter: %w", e)
	}
	return m, nil
}

// skillNames lists the skills runs can use: built in, and operator or
// approved ones under workspace/skills/, which override built-in ones.
func (s *Service) skillNames() []string {
	var names []string
	if entries, e := fs.ReadDir(defaults, "defaults/skills"); e == nil {
		for _, d := range entries {
			if d.IsDir() {
				names = append(names, d.Name())
			}
		}
	}
	if entries, e := os.ReadDir(filepath.Join(s.root, "workspace/skills")); e == nil {
		for _, d := range entries {
			if d.IsDir() && !slices.Contains(names, d.Name()) {
				names = append(names, d.Name())
			}
		}
	}
	slices.Sort(names)
	return names
}

// skillCatalog is the skills index loaded into every run: one line per skill,
// capped like MEMORY.md so the loaded layer stays small.
func (s *Service) skillCatalog() string {
	var b strings.Builder
	shown := 0
	names := s.skillNames()
	for _, name := range names {
		raw, e := s.workspaceRead("skills/" + name + "/SKILL.md")
		if e != nil {
			continue
		}
		m, e := parseSkill(raw)
		if e != nil || m.Description == "" {
			continue
		}
		line := "- " + name + ": " + strings.Join(strings.Fields(m.Description), " ") + "\n"
		if shown == maxSkills || b.Len()+len(line) > maxCatalogBytes {
			fmt.Fprintf(&b, "(%d more skills not listed)\n", len(names)-shown)
			break
		}
		b.WriteString(line)
		shown++
	}
	return b.String()
}

type draftInfo struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"` // skill or identity
	Description string   `json:"description,omitempty"`
	Files       []string `json:"files"`
	Problem     string   `json:"problem,omitempty"` // why approve would refuse it
}

func (s *Service) draftDir(name string) string {
	return filepath.Join(s.root, "workspace/skills-drafts", name)
}

// Drafts lists drafts waiting for review.
func (s *Service) Drafts() ([]draftInfo, error) {
	entries, e := os.ReadDir(filepath.Join(s.root, "workspace/skills-drafts"))
	if errors.Is(e, os.ErrNotExist) {
		return []draftInfo{}, nil
	}
	if e != nil {
		return nil, e
	}
	list := []draftInfo{}
	for _, d := range entries {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		info := draftInfo{Name: d.Name(), Kind: "skill"}
		files, e := treeFiles(s.draftDir(d.Name()), "")
		if e != nil {
			return nil, e
		}
		info.Files = files
		if d.Name() == identityDraft {
			info.Kind = identityDraft
			_, e = s.identityProposal()
		} else {
			var m skillMeta
			m, e = s.checkSkillDraft(d.Name())
			info.Description = m.Description
		}
		if e != nil {
			info.Problem = e.Error()
		}
		list = append(list, info)
	}
	return list, nil
}

func (s *Service) checkSkillDraft(name string) (skillMeta, error) {
	if !skillName.MatchString(name) {
		return skillMeta{}, fmt.Errorf("draft name %q must match %s", name, skillName)
	}
	dir := s.draftDir(name)
	size := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return errors.New("draft contains a symlink")
		}
		if info, e := d.Info(); e == nil && d.Type().IsRegular() {
			size += int(info.Size())
		}
		return nil
	})
	if err != nil {
		return skillMeta{}, err
	}
	if size > maxSkillBytes {
		return skillMeta{}, fmt.Errorf("draft has %d bytes; the limit is %d", size, maxSkillBytes)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		return skillMeta{}, errors.New("draft has no SKILL.md")
	}
	m, err := parseSkill(raw)
	if err != nil {
		return m, err
	}
	if m.Name != name {
		return m, fmt.Errorf("SKILL.md name %q does not match draft %q", m.Name, name)
	}
	if m.Description == "" || len(m.Description) > maxDescriptionLen {
		return m, fmt.Errorf("SKILL.md description must have 1..%d bytes", maxDescriptionLen)
	}
	if !slices.Contains(s.skillNames(), name) && len(s.skillNames()) >= maxSkills {
		return m, fmt.Errorf("there are already %d skills; merge or remove one first", maxSkills)
	}
	return m, nil
}

func (s *Service) identityProposal() (string, error) {
	b, err := os.ReadFile(filepath.Join(s.draftDir(identityDraft), "PROPOSAL.md"))
	if err != nil {
		return "", errors.New("identity draft has no PROPOSAL.md")
	}
	text := strings.TrimSpace(string(b))
	if text == "" || len(text) > maxProposalBytes {
		return "", fmt.Errorf("PROPOSAL.md must have 1..%d bytes", maxProposalBytes)
	}
	return text, nil
}

// DecideDraft approves or rejects a draft. Approving a skill installs it
// under workspace/skills/, keeping any skill it replaces; approving the
// identity draft appends its rules to IDENTITY.md, keeping the old file.
// Rejecting keeps the draft with the reason under skills-drafts/.rejected/,
// where later consolidations read it before drafting again.
func (s *Service) DecideDraft(name, decision, reason string) error {
	if s.consolidating.Load() {
		return fmt.Errorf("%w: a consolidation is running", ErrConflict)
	}
	if name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, `/\`) || !fileExists(s.draftDir(name)) {
		return fmt.Errorf("%w: no draft %q", ErrConflict, name)
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	entry := map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "draft": name, "decision": decision}
	var err error
	switch decision {
	case "approve":
		if name == identityDraft {
			err = s.approveIdentity(stamp)
		} else {
			err = s.approveSkill(name, stamp)
		}
	case "reject":
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return errors.New("a rejection needs a reason; the next consolidation reads it")
		}
		entry["reason"] = reason
		dst := filepath.Join(s.root, "workspace", filepath.FromSlash(rejectedDrafts), name+"."+stamp)
		if err = os.MkdirAll(filepath.Dir(dst), 0700); err == nil {
			if err = os.Rename(s.draftDir(name), dst); err == nil {
				err = atomicWrite(filepath.Join(dst, "REJECTED.md"), []byte("Rejected "+entry["ts"].(string)+": "+reason+"\n"), 0600)
			}
		}
	default:
		return errors.New("decision must be approve or reject")
	}
	if err != nil {
		return err
	}
	b, _ := json.Marshal(entry)
	f, err := os.OpenFile(filepath.Join(s.root, gateLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

func (s *Service) approveSkill(name, stamp string) error {
	if _, err := s.checkSkillDraft(name); err != nil {
		return err
	}
	dst := filepath.Join(s.root, "workspace/skills", name)
	if fileExists(dst) {
		keep := filepath.Join(s.root, gateVersions, name, stamp)
		if err := os.MkdirAll(filepath.Dir(keep), 0700); err != nil {
			return err
		}
		if err := os.Rename(dst, keep); err != nil {
			return err
		}
	}
	return errors.Join(os.Rename(s.draftDir(name), dst), syncDir(filepath.Join(s.root, "workspace/skills")))
}

func (s *Service) approveIdentity(stamp string) error {
	rules, err := s.identityProposal()
	if err != nil {
		return err
	}
	path := filepath.Join(s.root, "workspace/IDENTITY.md")
	old, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	keep := filepath.Join(s.root, gateVersions, "IDENTITY.md", stamp+".md")
	if err = os.MkdirAll(filepath.Dir(keep), 0700); err != nil {
		return err
	}
	if err = atomicWrite(keep, old, 0600); err != nil {
		return err
	}
	text := strings.TrimRight(string(old), "\n") + "\n\n## Approved rules (" + time.Now().UTC().Format("2006-01-02") + ")\n\n" + rules + "\n"
	if err = atomicWrite(path, []byte(text), 0600); err != nil {
		return err
	}
	return os.RemoveAll(s.draftDir(identityDraft))
}
