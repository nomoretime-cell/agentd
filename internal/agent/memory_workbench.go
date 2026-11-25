package agent

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type memoryEntry struct {
	Path   string `json:"path"`
	Source string `json:"source"`
}
type memoryView struct {
	Path     string     `json:"path"`
	Body     string     `json:"body"`
	Version  string     `json:"version"`
	Base     string     `json:"base"`
	Writable bool       `json:"writable"`
	Evidence []evidence `json:"evidence"`
	History  []string   `json:"history"`
}

func visibleEntry(p string) bool {
	if !fs.ValidPath(p) || strings.Contains(p, "/.") || strings.Contains(p, "\\") {
		return false
	}
	if editableNote(p) || p == "IDENTITY.md" {
		return true
	}
	parts := strings.Split(p, "/")
	return len(parts) >= 3 && (parts[0] == "skills" || parts[0] == "skills-drafts") && skillName.MatchString(parts[1])
}
func (s *Service) memoryEntries() ([]memoryEntry, error) {
	entries := []memoryEntry{{"MEMORY.md", "记忆索引"}, {"IDENTITY.md", "身份规则"}}
	home := filepath.Join(s.root, "workspace")
	for _, tree := range memoryTrees {
		files, e := treeFiles(filepath.Join(home, tree), tree+"/")
		if e != nil {
			return nil, e
		}
		for _, p := range files {
			if visibleEntry(p) {
				label := "笔记"
				if tree == "skills-drafts" {
					label = "待审草稿"
				}
				entries = append(entries, memoryEntry{p, label})
			}
		}
	}
	for _, name := range s.skillNames() {
		p := "skills/" + name + "/SKILL.md"
		if !visibleEntry(p) {
			continue
		}
		label := "内置技能"
		if fileExists(filepath.Join(home, p)) {
			label = "工作区技能"
		}
		entries = append(entries, memoryEntry{p, label})
	}
	return entries, nil
}
func versionName(v string) bool { _, e := time.Parse("20060102T150405.000000000Z", v); return e == nil }
func (s *Service) memoryDocument(p, at string) (memoryView, error) {
	var d memoryView
	if !visibleEntry(p) {
		return d, ErrDenied
	}
	d.Path = p
	d.History = []string{}
	var raw []byte
	var e error
	parts := strings.Split(p, "/")
	switch {
	case at == "":
		raw, e = s.workspaceRead(p)
	case at == "builtin" && strings.HasPrefix(p, "skills/"):
		raw, e = defaults.ReadFile("defaults/" + p)
	case versionName(at):
		if strings.HasPrefix(p, "skills/") {
			raw, e = os.ReadFile(filepath.Join(s.root, gateVersions, parts[1], at, strings.Join(parts[2:], "/")))
		} else if p == "IDENTITY.md" {
			raw, e = os.ReadFile(filepath.Join(s.root, gateVersions, p, at+".md"))
		} else {
			raw, e = os.ReadFile(filepath.Join(s.root, versionsDir, at, p))
		}
	default:
		return d, ErrDenied
	}
	if errors.Is(e, fs.ErrNotExist) {
		return d, sql.ErrNoRows
	}
	if e != nil {
		return d, e
	}
	d.Body = s.maskedFile(string(raw))
	d.Version = digest(raw)
	d.Writable = at == "" && d.Body == string(raw) && (editableNote(p) || (len(parts) == 3 && parts[2] == "SKILL.md" && parts[1] != identityDraft))
	if at != "" {
		return d, nil
	}
	if strings.HasPrefix(p, "skills/") {
		f, e := s.skillFiles(parts[1])
		if e != nil {
			return d, e
		}
		d.Base = memoryDigest(f)
		if _, e := defaults.ReadFile("defaults/" + p); e == nil {
			d.History = append(d.History, "builtin")
		}
		dirs, e := os.ReadDir(filepath.Join(s.root, gateVersions, parts[1]))
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return d, e
		}
		for _, v := range dirs {
			if versionName(v.Name()) && fileExists(filepath.Join(s.root, gateVersions, parts[1], v.Name(), parts[2])) {
				d.History = append(d.History, v.Name())
			}
		}
	} else if p == "IDENTITY.md" {
		files, e := os.ReadDir(filepath.Join(s.root, gateVersions, p))
		if e != nil && !errors.Is(e, fs.ErrNotExist) {
			return d, e
		}
		for _, f := range files {
			v := strings.TrimSuffix(f.Name(), ".md")
			if versionName(v) {
				d.History = append(d.History, v)
			}
		}
	} else {
		versions, e := s.versions()
		if e != nil {
			return d, e
		}
		for _, v := range versions {
			if fileExists(filepath.Join(s.root, versionsDir, v, p)) {
				d.History = append(d.History, v)
			}
		}
	}
	d.Evidence, _ = parseEvidence(d.Body)
	return d, nil
}
func (s *Service) editDocument(p, body, version, base string) (string, error) {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	if !visibleEntry(p) || s.maskedFile(body) != body {
		return "", ErrDenied
	}
	if editableNote(p) {
		return p, s.reviseLocked(p, body, version, "owner", callScope{})
	}
	parts := strings.Split(p, "/")
	if len(parts) != 3 || parts[2] != "SKILL.md" || parts[1] == identityDraft {
		return "", ErrDenied
	}
	meta, e := parseSkill([]byte(body))
	if e != nil || meta.Name != parts[1] || strings.TrimSpace(meta.Description) == "" || len(meta.Description) > maxDescriptionLen || len(body) > maxSkillBytes {
		return "", errors.New("技能名称、描述或大小无效")
	}
	if s.consolidating.Load() {
		return "", ErrConflict
	}
	if parts[0] == "skills" {
		d, e := s.memoryDocument(p, "")
		if e != nil {
			return "", e
		}
		if !d.Writable || d.Version != version {
			return "", ErrConflict
		}
		if e = s.makeSkillDraft(parts[1], base); e != nil {
			return "", e
		}
		p = draftsPrefix + parts[1] + "/SKILL.md"
	}
	if e = s.checkDraftBase(parts[1]); e != nil {
		return "", e
	}
	return p, s.reviseLocked(p, body, version, "owner", callScope{})
}

type fileDelta struct {
	Path   string `json:"path"`
	Before string `json:"before"`
	After  string `json:"after"`
}

func (s *Service) deltas(live, next map[string]string) []fileDelta {
	changes := []fileDelta{}
	names := map[string]string{}
	for p := range live {
		names[p] = ""
	}
	for p := range next {
		names[p] = ""
	}
	for _, p := range slices.Sorted(mapKeys(names)) {
		if visibleEntry(p) && live[p] != next[p] {
			changes = append(changes, fileDelta{p, s.redact(live[p]), s.redact(next[p])})
		}
	}
	return changes
}

type candidateView struct {
	Stage   string      `json:"stage"`
	Stale   bool        `json:"stale"`
	Token   string      `json:"token"`
	Changes []fileDelta `json:"changes"`
}

func (s *Service) candidateView() (candidateView, error) {
	v := candidateView{Changes: []fileDelta{}}
	versions, e := s.versions()
	if e != nil {
		return v, e
	}
	if len(versions) == 0 && !fileExists(filepath.Join(s.root, candidateDir)) && !fileExists(filepath.Join(s.root, heldDir)) && !fileExists(filepath.Join(s.root, rejectedDir)) {
		return v, nil
	}
	live, e := readMemory(filepath.Join(s.root, "workspace"))
	if e != nil {
		return v, e
	}
	v.Token = memoryDigest(live) + ":" + strings.Join(versions, ",")
	for _, c := range []struct{ dir, stage string }{{candidateDir, "processing"}, {heldDir, "held"}, {rejectedDir, "rejected"}} {
		if !fileExists(filepath.Join(s.root, c.dir)) {
			continue
		}
		cand, e := readMemory(filepath.Join(s.root, c.dir))
		if e != nil {
			return v, e
		}
		v.Stage = c.stage
		if c.dir == candidateDir && !s.consolidating.Load() {
			v.Stage = "interrupted"
		}
		v.Stale = errors.Is(s.checkBase(c.dir), errStaleCandidate)
		v.Token += ":" + memoryDigest(cand)
		v.Changes = s.deltas(live, cand)
		break
	}
	return v, nil
}

type draftView struct {
	draftInfo
	Token   string      `json:"token"`
	Changes []fileDelta `json:"changes"`
}

func (s *Service) draftView(d draftInfo) (draftView, error) {
	v := draftView{draftInfo: d}
	before := map[string]string{}
	after := map[string]string{}
	files, e := treeFiles(s.draftDir(d.Name), "")
	if e != nil {
		return v, e
	}
	for _, file := range files {
		p := draftsPrefix + d.Name + "/" + file
		b, e := s.workspaceRead(p)
		if e != nil {
			return v, e
		}
		after[p] = string(b)
	}
	if d.Name == identityDraft {
		b, e := s.workspaceRead("IDENTITY.md")
		if e != nil {
			return v, e
		}
		before[draftsPrefix+d.Name+"/PROPOSAL.md"] = string(b)
		p := draftsPrefix + d.Name + "/PROPOSAL.md"
		after[p] = strings.TrimRight(string(b), "\n") + "\n\n" + after[p]
	} else if skillName.MatchString(d.Name) {
		files, e := s.skillFiles(d.Name)
		if e != nil {
			return v, e
		}
		for p, b := range files {
			before[draftsPrefix+d.Name+"/"+p] = b
		}
		if e = s.checkDraftBase(d.Name); e != nil {
			v.Problem = "有效技能已改变，请重新起草"
		}
	}
	v.Token = memoryDigest(before) + ":" + memoryDigest(after)
	v.Changes = s.deltas(before, after)
	return v, nil
}
func (s *Service) recentMemoryLog(file string) ([]map[string]any, error) {
	out := []map[string]any{}
	f, e := os.Open(filepath.Join(s.root, file))
	if errors.Is(e, fs.ErrNotExist) {
		return out, nil
	}
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	start := max(int64(0), st.Size()-(256<<10))
	buf, e := io.ReadAll(io.NewSectionReader(f, start, st.Size()-start))
	if e != nil {
		return nil, e
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0 && len(out) < 80; i-- {
		var row map[string]any
		if json.Unmarshal(lines[i], &row) == nil {
			for k, v := range row {
				switch value := v.(type) {
				case string:
					row[k] = s.redact(value)
				case []any:
					for j, x := range value {
						if text, ok := x.(string); ok {
							value[j] = s.redact(text)
						}
					}
				}
			}
			out = append(out, row)
		}
	}
	return out, nil
}
func (s *Service) revisionList() ([]memoryRevision, error) {
	out := []memoryRevision{}
	dir := filepath.Join(s.root, editDir)
	files, e := os.ReadDir(dir)
	if errors.Is(e, fs.ErrNotExist) {
		return out, nil
	}
	if e != nil {
		return nil, e
	}
	for i := len(files) - 1; i >= 0 && len(out) < 100; i-- {
		if !strings.HasSuffix(files[i].Name(), ".json") {
			continue
		}
		b, e := os.ReadFile(filepath.Join(dir, files[i].Name()))
		if e != nil {
			return nil, e
		}
		var r memoryRevision
		if e = json.Unmarshal(b, &r); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, nil
}

func (s *Service) registerMemoryWorkbench(m *http.ServeMux) {
	m.HandleFunc("GET /v1/evolution", func(w http.ResponseWriter, r *http.Request) {
		board, e := s.memoryBoard()
		if e != nil {
			respond(w, nil, e)
			return
		}
		runs, e := s.dreamRuns(r.Context())
		board["runs"] = runs
		respond(w, board, e)
	})
	m.HandleFunc("GET /v1/evolution/document", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.memoryMu.Lock()
		d, e := s.memoryDocument(q.Get("path"), q.Get("at"))
		s.memoryMu.Unlock()
		respond(w, d, e)
	})
	m.HandleFunc("GET /v1/evolution/uses", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("path")
		if !visibleEntry(p) {
			respond(w, nil, ErrDenied)
			return
		}
		rows, e := s.read.QueryContext(r.Context(), `SELECT DISTINCT i.id||'~'||a.key, json_extract(i.run_index,'$."'||a.key||'".ts'), src.value->>'kind',coalesce(src.value->>'version','') FROM inbox i JOIN json_each(i.run_data) a JOIN json_each(a.value,'$.sources') src WHERE i.run_index->>'_last'>=? AND src.value->>'name'=? AND src.value->>'kind' IN ('document','read','hit') ORDER BY 2 DESC LIMIT 30`, time.Now().AddDate(0, 0, -30).UnixMilli(), p)
		if e != nil {
			respond(w, nil, e)
			return
		}
		defer rows.Close()
		list := []map[string]string{}
		for rows.Next() {
			var run, ts, kind, version string
			if e = rows.Scan(&run, &ts, &kind, &version); e != nil {
				respond(w, nil, e)
				return
			}
			list = append(list, map[string]string{"run": run, "ts": ts, "kind": kind, "version": version})
		}
		respond(w, list, rows.Err())
	})
	m.HandleFunc("POST /v1/evolution/edit", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Path, Body, Version, Base string }
		if !decodeHTTP(w, r, &req) {
			return
		}
		p, e := s.editDocument(req.Path, req.Body, req.Version, req.Base)
		respond(w, map[string]string{"path": p}, e)
	})
	m.HandleFunc("POST /v1/evolution/dream", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Key, Profile, Version string }
		if !decodeHTTP(w, r, &req) {
			return
		}
		id, e := s.queueDream(r.Context(), req.Key, req.Profile, req.Version)
		respond(w, map[string]int64{"event": id}, e)
	})
	m.HandleFunc("POST /v1/evolution/review", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Decision, Token, Reason string }
		if !decodeHTTP(w, r, &req) {
			return
		}
		e := s.reviewMemory(req.Name, req.Decision, req.Token, req.Reason)
		respond(w, map[string]string{"decision": req.Decision}, e)
	})
	m.HandleFunc("GET /v1/evolution/revisions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if len(id) != len("20060102T150405.000000000Z-12345678") || !fs.ValidPath(id) || strings.ContainsAny(id, "/\\") {
			respond(w, nil, sql.ErrNoRows)
			return
		}
		b, e := os.ReadFile(filepath.Join(s.root, editDir, id+".json"))
		if errors.Is(e, fs.ErrNotExist) {
			e = sql.ErrNoRows
		}
		var rev memoryRevision
		if e == nil {
			e = json.Unmarshal(b, &rev)
			if e == nil {
				for hash, dst := range map[string]*string{rev.Previous: &rev.Before, rev.Version: &rev.After} {
					if hash == "absent" {
						continue
					}
					body, err := os.ReadFile(filepath.Join(s.root, editBodies, hash))
					if errors.Is(err, fs.ErrNotExist) {
						// Pruned together with its revision after this read began.
						err = sql.ErrNoRows
					}
					if err != nil {
						e = err
						break
					}
					*dst = s.maskedFile(string(body))
				}
			}
		}
		respond(w, rev, e)
	})
}
func (s *Service) memoryBoard() (map[string]any, error) {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	entries, e := s.memoryEntries()
	if e != nil {
		return nil, e
	}
	candidate, e := s.candidateView()
	if e != nil {
		return nil, e
	}
	drafts, e := s.Drafts()
	if e != nil {
		return nil, e
	}
	reviews := []draftView{}
	for _, d := range drafts {
		v, e := s.draftView(d)
		if e != nil {
			return nil, e
		}
		reviews = append(reviews, v)
	}
	rev, e := s.revisionList()
	if e != nil {
		return nil, e
	}

	metrics, e := s.recentMemoryLog(metricsFile)
	if e != nil {
		return nil, e
	}
	decisions, e := s.recentMemoryLog(gateLog)
	if e != nil {
		return nil, e
	}
	versions, e := s.versions()
	if e != nil {
		return nil, e
	}
	return map[string]any{"entries": entries, "models": s.dreamOptions(), "checking": s.dreamChecking.Load(), "working": s.consolidating.Load(), "versions": len(versions), "candidate": candidate, "drafts": reviews, "edits": rev, "metrics": metrics, "decisions": decisions}, nil
}
func (s *Service) reviewMemory(name, decision, token, reason string) error {
	s.memoryMu.Lock()
	if name != "" {
		defer s.memoryMu.Unlock()
		if !fs.ValidPath(name) || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") {
			return ErrDenied
		}
		v, e := s.draftView(draftInfo{Name: name})
		if e != nil {
			return e
		}
		if v.Token != token {
			return ErrConflict
		}
		return s.decideDraftLocked(name, decision, reason)
	}
	v, e := s.candidateView()
	if e == nil && v.Token != token {
		e = ErrConflict
	}
	if e == nil && (decision == "promote" || decision == "discard") && v.Stage != "held" {
		e = ErrConflict
	}
	if e == nil {
		e = s.memoryControlLocked(decision)
	}
	s.memoryMu.Unlock()
	if e == nil {
		metric := map[string]any{"result": decision, "actor": "owner"}
		if decision == "promote" {
			var paths, added []string
			for _, d := range v.Changes {
				paths = append(paths, d.Path)
				if entryPath(d.Path) && d.Before == "" && d.After != "" {
					added = append(added, d.Path)
				}
			}
			metric["paths"] = paths
			metric["new_notes"] = added
		}
		s.recordConsolidation(metric)
	}
	return e
}
