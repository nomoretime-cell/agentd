package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// search is the read path for finding earlier work without paging through
// whole files: a plain substring scan, no index. It follows the same scope
// rules as read_file: shared conversations cannot search transcripts, and
// critical runs see only records that may leave this machine.
const (
	searchMaxHits    = 20
	searchMaxQuery   = 200
	searchScanBudget = 64 << 20
	searchSnippet    = 120 // bytes of context on each side of a match
)

type searchHit struct {
	Path    string `json:"path"`
	Offset  int64  `json:"offset"`
	Snippet string `json:"snippet"`
}

type searchResult struct {
	Query     string      `json:"query"`
	Hits      []searchHit `json:"hits"`
	Truncated bool        `json:"truncated,omitempty"`
	Skipped   string      `json:"skipped,omitempty"`
	Hint      string      `json:"hint"`
}

// searchArea is one place search can look: a model path prefix and how to
// list and read its files.
type searchArea struct {
	prefix string
	files  func() ([]string, error)            // model paths, in search order
	open   func(string) (io.ReadCloser, error) // by model path
	keep   func([]byte) bool                   // which lines may be shown
}

func (s *Service) searchAreas(ctx context.Context) []searchArea {
	home := s.memoryHome(ctx)
	notes := searchArea{prefix: "notes/", files: func() ([]string, error) { return treeFiles(filepath.Join(home, "notes"), "notes/") },
		open: func(p string) (io.ReadCloser, error) {
			return openUnder(filepath.Join(home, "notes"), strings.TrimPrefix(p, "notes/"))
		}}
	sessions := searchArea{prefix: "sessions/", files: s.transcriptFiles,
		open: func(p string) (io.ReadCloser, error) { f, _, e := s.openTranscript(p, 0); return f, e }}
	if isCritical(ctx) {
		sessions.keep = egressVisible
	}
	kdir := filepath.Join(s.root, knowledgeDir)
	knowledge := searchArea{prefix: "knowledge/", files: func() ([]string, error) {
		all, e := treeFiles(kdir, "knowledge/")
		return slices.DeleteFunc(all, func(p string) bool { return strings.HasPrefix(p, "knowledge/.archive/") }), e
	}, open: func(p string) (io.ReadCloser, error) { return openUnder(kdir, strings.TrimPrefix(p, "knowledge/")) }}
	return []searchArea{notes, knowledge, sessions}
}

// transcriptFiles lists transcripts newest day first.
func (s *Service) transcriptFiles() ([]string, error) {
	entries, e := os.ReadDir(filepath.Join(s.root, "data/sessions"))
	if e != nil {
		return nil, e
	}
	var names []string
	for _, d := range entries {
		if d.Type().IsRegular() && strings.HasSuffix(d.Name(), ".jsonl") {
			names = append(names, "sessions/"+d.Name())
		}
	}
	slices.Sort(names)
	slices.Reverse(names)
	return names, nil
}

// treeFiles lists regular files under dir without following symlinks.
func treeFiles(dir, prefix string) ([]string, error) {
	r, e := os.OpenRoot(dir)
	if errors.Is(e, os.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	defer r.Close()
	var names []string
	e = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() && !strings.HasPrefix(d.Name(), ".agentd-") {
			names = append(names, prefix+p)
		}
		return nil
	})
	return names, e
}

func openUnder(dir, name string) (io.ReadCloser, error) {
	r, e := os.OpenRoot(dir)
	if e != nil {
		return nil, e
	}
	defer r.Close()
	f, e := r.Open(name)
	if e != nil {
		return nil, e
	}
	if info, e := f.Stat(); e != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, errors.Join(e, ErrDenied)
	}
	return f, nil
}

func (s *Service) search(ctx context.Context, query, where string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" || len(query) > searchMaxQuery {
		return "", errors.New("query must contain 1..200 bytes")
	}
	terms := strings.Fields(strings.ToLower(query))
	where = path.Clean(strings.TrimSpace(where))
	if where == "." {
		where = ""
	}
	if strings.HasPrefix(where, "../") || path.IsAbs(where) {
		return "", ErrDenied
	}
	res := searchResult{Query: query, Hits: []searchHit{}, Hint: "Read around a hit with read_file using its path and offset."}
	matched := false
	budget := int64(searchScanBudget)
	for _, area := range s.searchAreas(ctx) {
		base := strings.TrimSuffix(area.prefix, "/")
		if where != "" && where != base && !strings.HasPrefix(where, area.prefix) {
			continue
		}
		matched = true
		if area.prefix == "sessions/" && isSharedTurn(ctx) {
			if where != "" {
				return "", ErrDenied
			}
			res.Skipped = "sessions/ is not searchable in a shared conversation"
			continue
		}
		files, e := area.files()
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return "", e
		}
		for _, name := range files {
			if where != "" && where != base && name != where && !strings.HasPrefix(name, strings.TrimSuffix(where, "/")+"/") {
				continue
			}
			if res.Truncated {
				break
			}
			if e = s.searchFile(area, name, terms, &res, &budget); e != nil {
				return "", e
			}
		}
	}
	if !matched {
		return "", errors.New("search path must be under notes/, knowledge/ or sessions/")
	}
	return s.fitSearch(res), nil
}

func (s *Service) searchFile(area searchArea, name string, terms []string, res *searchResult, budget *int64) error {
	f, e := area.open(name)
	if e != nil {
		return e
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 64<<10)
	var offset int64
	for {
		line, e := r.ReadBytes('\n')
		if len(line) > 0 {
			*budget -= int64(len(line))
			if (area.keep == nil || area.keep(line)) && s.addHit(name, offset, line, terms, res) {
				return nil
			}
			offset += int64(len(line))
		}
		if *budget <= 0 {
			res.Truncated = true
			return nil
		}
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return e
		}
	}
}

// addHit records line if it contains every term, and reports whether the
// result is full.
func (s *Service) addHit(name string, offset int64, line []byte, terms []string, res *searchResult) bool {
	view := s.maskedFile(strings.TrimRight(string(line), "\r\n"))
	lower := strings.ToLower(view)
	at := -1
	for _, t := range terms {
		i := strings.Index(lower, t)
		if i < 0 {
			return false
		}
		if at < 0 {
			at = i
		}
	}
	if len(res.Hits) == searchMaxHits {
		res.Truncated = true
		return true
	}
	// Lowercasing can change byte lengths; clamp before cutting the original.
	at = min(at, len(view))
	start, end := max(0, at-searchSnippet), min(len(view), at+searchSnippet)
	snippet := strings.ToValidUTF8(view[start:end], "")
	if start > 0 {
		snippet = "…" + snippet
	}
	if end < len(view) {
		snippet += "…"
	}
	res.Hits = append(res.Hits, searchHit{Path: name, Offset: offset, Snippet: snippet})
	return false
}

// fitSearch drops the last hits until the result fits the tool output limit.
func (s *Service) fitSearch(res searchResult) string {
	for {
		b, _ := json.Marshal(res)
		if len(b) <= s.cfg.Limits.MaxOutputBytes || len(res.Hits) == 0 {
			return string(b)
		}
		res.Hits = res.Hits[:len(res.Hits)-1]
		res.Truncated = true
	}
}
