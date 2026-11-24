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
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Knowledge is imported reference material: raw data like transcripts, so
// runs only read it. An operator imports files or directories; the host
// converts each to Markdown, keeps the original privately, and writes an
// index that loads into every run. Re-importing unchanged files does nothing;
// a changed file replaces its Markdown and archives the old one.
const (
	knowledgeDir      = "workspace/knowledge"
	knowledgeRaw      = "data/knowledge/raw"
	knowledgeManifest = "data/knowledge/manifest.json"
	knowledgeLog      = "data/knowledge/imports.jsonl"
	knowledgeWork     = "data/knowledge/tmp"

	maxSourceBytes   = 50 << 20
	maxMarkdownBytes = 1 << 20 // read_file serves workspace files up to 1 MiB
)

var (
	nativeFormats    = []string{".md", ".markdown", ".txt"}
	convertedFormats = []string{".pdf", ".docx", ".pptx", ".xlsx", ".html", ".htm", ".csv", ".epub"}
)

// KnowledgeConfig names the converter for formats other than Markdown and
// text, such as MarkItDown or Docling. It runs sandboxed with the file path as
// its last argument and must print Markdown.
type KnowledgeConfig struct {
	Converter        []string `yaml:"converter"`
	ConverterPaths   []string `yaml:"converter_paths"` // read-only directories the converter needs
	ConverterSeconds int      `yaml:"converter_seconds"`
}

type knowledgeDoc struct {
	ID         string `json:"doc_id"`
	Source     string `json:"source"`
	Title      string `json:"title"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	MDBytes    int    `json:"md_bytes"`
	Converter  string `json:"converter"`
	ImportedAt string `json:"imported_at"`
}

type importResult struct {
	Source string `json:"source"`
	DocID  string `json:"doc_id,omitempty"`
	Status string `json:"status"` // added, updated, unchanged, archived, failed
	Error  string `json:"error,omitempty"`
}

type knowledgeState struct {
	mu      sync.Mutex
	running bool
	last    []importResult
}

func docID(source string) string { return stableID("knowledge\x00" + source)[:16] }

func (s *Service) loadManifest() (map[string]knowledgeDoc, error) {
	docs := map[string]knowledgeDoc{}
	b, e := os.ReadFile(filepath.Join(s.root, knowledgeManifest))
	if errors.Is(e, os.ErrNotExist) {
		return docs, nil
	}
	if e != nil {
		return nil, e
	}
	return docs, json.Unmarshal(b, &docs)
}

// Knowledge lists imported documents and the last import's results.
func (s *Service) Knowledge() (map[string]any, error) {
	docs, e := s.loadManifest()
	if e != nil {
		return nil, e
	}
	list := slices.SortedFunc(func(yield func(knowledgeDoc) bool) {
		for _, d := range docs {
			if !yield(d) {
				return
			}
		}
	}, func(a, b knowledgeDoc) int { return strings.Compare(a.Source, b.Source) })
	s.knowledge.mu.Lock()
	defer s.knowledge.mu.Unlock()
	return map[string]any{"documents": list, "importing": s.knowledge.running, "last_import": s.knowledge.last}, nil
}

// StartImport imports a file or a directory tree in the background; one
// import runs at a time. For a directory, documents imported from it before
// whose files are gone are archived.
func (s *Service) StartImport(source string) error {
	abs, e := filepath.Abs(source)
	if e != nil {
		return e
	}
	info, e := os.Stat(abs)
	if e != nil {
		return e
	}
	if e = s.beginImport(); e != nil {
		return e
	}
	go func() {
		results := s.importKnowledge(s.life, abs, info.IsDir())
		s.finishImport(results)
	}()
	return nil
}

func (s *Service) beginImport() error {
	s.knowledge.mu.Lock()
	defer s.knowledge.mu.Unlock()
	if err := s.life.Err(); err != nil {
		return err
	}
	if s.knowledge.running {
		return fmt.Errorf("%w: an import is running", ErrConflict)
	}
	s.knowledge.running = true
	s.wg.Add(1)
	return nil
}

func (s *Service) finishImport(results []importResult) {
	s.knowledge.mu.Lock()
	if results != nil {
		s.knowledge.last = results
	}
	s.knowledge.running = false
	s.knowledge.mu.Unlock()
	s.wg.Done()
}

func (s *Service) importKnowledge(ctx context.Context, root string, dir bool) []importResult {
	var files []string
	if dir {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && p != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			ext := strings.ToLower(filepath.Ext(p))
			if d.Type().IsRegular() && (slices.Contains(nativeFormats, ext) || slices.Contains(convertedFormats, ext)) {
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			return s.logImport([]importResult{{Source: root, Status: "failed", Error: err.Error()}})
		}
	} else {
		files = []string{root}
	}
	// The manifest is read once and saved once: Markdown written before a
	// crash is simply imported again, since its hash is not yet recorded.
	docs, err := s.loadManifest()
	if err != nil {
		return s.logImport([]importResult{{Source: root, Status: "failed", Error: err.Error()}})
	}
	var results []importResult
	for _, p := range files {
		if ctx.Err() != nil {
			results = append(results, importResult{Source: p, Status: "failed", Error: "import stopped"})
			continue
		}
		results = append(results, s.importFile(ctx, p, docs))
	}
	if dir && ctx.Err() == nil {
		results = append(results, s.archiveMissing(root, files, docs)...)
	}
	s.memoryMu.Lock()
	err = s.saveKnowledge(docs)
	s.memoryMu.Unlock()
	if err != nil {
		results = append(results, importResult{Source: root, Status: "failed", Error: err.Error()})
	}
	return s.logImport(results)
}

func (s *Service) logImport(results []importResult) []importResult {
	var b bytes.Buffer
	for _, r := range results {
		line, _ := json.Marshal(map[string]any{"ts": time.Now().UTC().Format(time.RFC3339), "source": r.Source, "doc_id": r.DocID, "status": r.Status, "error": r.Error})
		b.Write(append(line, '\n'))
	}
	f, e := os.OpenFile(filepath.Join(s.root, knowledgeLog), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if e == nil {
		_, e = f.Write(b.Bytes())
		e = errors.Join(e, f.Close())
	}
	s.logErr("knowledge import log", e)
	return results
}

// importFile converts one file and records it in docs; the caller saves docs.
func (s *Service) importFile(ctx context.Context, source string, docs map[string]knowledgeDoc) importResult {
	r := importResult{Source: source, DocID: docID(source)}
	info, err := os.Stat(source)
	if err != nil {
		r.Status, r.Error = "failed", s.redact(err.Error())
		return r
	}
	if !info.Mode().IsRegular() || info.Size() > maxSourceBytes {
		r.Status, r.Error = "failed", "not a regular file of at most 50 MiB"
		return r
	}
	f, err := os.Open(source)
	if err != nil {
		r.Status, r.Error = "failed", s.redact(err.Error())
		return r
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxSourceBytes+1))
	if err != nil || len(raw) > maxSourceBytes {
		r.Status, r.Error = "failed", "cannot read source within 50 MiB"
		return r
	}
	return s.importBytes(ctx, source, raw, docs)
}

func (s *Service) importBytes(ctx context.Context, source string, raw []byte, docs map[string]knowledgeDoc) importResult {
	r := importResult{Source: source, DocID: docID(source)}
	fail := func(e error) importResult { r.Status, r.Error = "failed", s.redact(e.Error()); return r }
	hash := digest(raw)
	old, existed := docs[r.DocID]
	if existed && old.SHA256 == hash {
		r.Status = "unchanged"
		return r
	}
	md, converter, e := s.toMarkdown(ctx, source, raw)
	if e != nil {
		return fail(e)
	}
	if len(md) > maxMarkdownBytes {
		return fail(fmt.Errorf("converted text has %d bytes; the limit is %d, so split the document", len(md), maxMarkdownBytes))
	}
	doc := knowledgeDoc{ID: r.DocID, Source: source, Title: docTitle(md, source), SHA256: hash, Bytes: int64(len(raw)), MDBytes: len(md), Converter: converter, ImportedAt: time.Now().UTC().Format(time.RFC3339)}
	docs[doc.ID] = doc
	index := knowledgeIndex(docs)
	if existed {
		docs[doc.ID] = old
	} else {
		delete(docs, doc.ID)
	}
	if n := lineCount(index); n > memoryIndexMaxLines || len(index) > memoryIndexMaxBytes {
		return fail(fmt.Errorf("knowledge/INDEX.md would have %d lines and %d bytes; the limit is %d lines and %d bytes", n, len(index), memoryIndexMaxLines, memoryIndexMaxBytes))
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	mdPath := filepath.Join(s.root, knowledgeDir, "md", doc.ID+".md")
	if existed {
		if e = s.archiveKnowledge(doc.ID, true); e != nil {
			return fail(e)
		}
	}
	ext := strings.ToLower(filepath.Ext(source))
	if e = atomicWrite(filepath.Join(s.root, knowledgeRaw, doc.ID+ext), raw, 0600); e != nil {
		return fail(e)
	}
	if e = atomicWrite(mdPath, md, 0600); e != nil {
		return fail(e)
	}
	docs[doc.ID] = doc
	r.Status = "added"
	if existed {
		r.Status = "updated"
	}
	return r
}

// archiveKnowledge keeps a version of the current Markdown before replacement or removal.
// Caller holds memoryMu.
func (s *Service) archiveKnowledge(id string, keepCurrent bool) error {
	src := filepath.Join(s.root, knowledgeDir, "md", id+".md")
	if !fileExists(src) {
		return nil
	}
	dst := filepath.Join(s.root, knowledgeDir, ".archive", id+"."+time.Now().UTC().Format("20060102T150405.000000000Z")+".md")
	if e := os.MkdirAll(filepath.Dir(dst), 0700); e != nil {
		return e
	}
	if keepCurrent {
		b, e := os.ReadFile(src)
		if e != nil {
			return e
		}
		return atomicWrite(dst, b, 0600)
	}
	return os.Rename(src, dst)
}

func (s *Service) archiveMissing(root string, present []string, docs map[string]knowledgeDoc) []importResult {
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	var results []importResult
	for id, d := range docs {
		if !strings.HasPrefix(d.Source, root+string(filepath.Separator)) || slices.Contains(present, d.Source) || fileExists(d.Source) {
			continue
		}
		r := importResult{Source: d.Source, DocID: id, Status: "archived"}
		if e := s.archiveKnowledge(id, false); e != nil {
			r.Status, r.Error = "failed", e.Error()
		} else {
			delete(docs, id)
		}
		results = append(results, r)
	}
	return results
}

// RemoveKnowledge archives one document. It waits for no import: an import
// saves the manifest it loaded, so removing meanwhile would be undone.
func (s *Service) RemoveKnowledge(id string) error {
	s.knowledge.mu.Lock()
	defer s.knowledge.mu.Unlock()
	if s.knowledge.running {
		return fmt.Errorf("%w: an import is running", ErrConflict)
	}
	s.memoryMu.Lock()
	defer s.memoryMu.Unlock()
	docs, e := s.loadManifest()
	if e != nil {
		return e
	}
	if _, ok := docs[id]; !ok {
		return fmt.Errorf("%w: no document %q", ErrConflict, id)
	}
	if e = s.archiveKnowledge(id, false); e != nil {
		return e
	}
	delete(docs, id)
	return s.saveKnowledge(docs)
}

// saveKnowledge writes the manifest and the index generated from it.
// Caller holds memoryMu.
func (s *Service) saveKnowledge(docs map[string]knowledgeDoc) error {
	b, e := json.MarshalIndent(docs, "", "  ")
	if e != nil {
		return e
	}
	if e = atomicWrite(filepath.Join(s.root, knowledgeManifest), b, 0600); e != nil {
		return e
	}
	return atomicWrite(filepath.Join(s.root, knowledgeDir, "INDEX.md"), []byte(knowledgeIndex(docs)), 0600)
}

func knowledgeIndex(docs map[string]knowledgeDoc) string {
	if len(docs) == 0 {
		return ""
	}
	lines := make([]string, 0, len(docs))
	for _, d := range docs {
		lines = append(lines, fmt.Sprintf("- %s (knowledge/md/%s.md, %d KB)", d.Title, d.ID, (d.MDBytes+1023)/1024))
	}
	slices.Sort(lines)
	return strings.Join(lines, "\n") + "\n"
}

// docTitle is the first heading, else the file name.
func docTitle(md []byte, source string) string {
	for _, line := range strings.SplitN(string(md), "\n", 50) {
		if t, ok := strings.CutPrefix(strings.TrimSpace(line), "# "); ok && strings.TrimSpace(t) != "" {
			return clipTitle(strings.TrimSpace(t))
		}
	}
	return clipTitle(filepath.Base(source))
}

func clipTitle(t string) string {
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > 120 {
		t = strings.ToValidUTF8(t[:120], "") + "…"
	}
	return t
}

func (s *Service) toMarkdown(ctx context.Context, source string, raw []byte) ([]byte, string, error) {
	ext := strings.ToLower(filepath.Ext(source))
	switch {
	case slices.Contains(nativeFormats, ext):
		if !utf8.Valid(raw) {
			return nil, "", errors.New("text is not UTF-8")
		}
		return raw, "native", nil
	case slices.Contains(convertedFormats, ext):
		if len(s.cfg.Knowledge.Converter) == 0 {
			return nil, "", fmt.Errorf("%s needs knowledge.converter (for example markitdown)", ext)
		}
		md, e := s.convert(ctx, source, raw)
		if e == nil && !utf8.Valid(md) {
			e = errors.New("converter output is not UTF-8")
		}
		if e == nil && len(bytes.TrimSpace(md)) == 0 {
			e = errors.New("converter produced no text")
		}
		return md, filepath.Base(s.cfg.Knowledge.Converter[0]), e
	}
	return nil, "", fmt.Errorf("unsupported format %q", ext)
}

// runConverter converts a copy of the file in a sandbox that has no network,
// sees only system directories, the converter's own directories and the
// copy, and can write only beside the copy.
func (s *Service) runConverter(ctx context.Context, source string, raw []byte) ([]byte, error) {
	k := s.cfg.Knowledge
	work, e := os.MkdirTemp(filepath.Join(s.root, knowledgeWork), "convert-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(work)
	input := filepath.Join(work, "input"+strings.ToLower(filepath.Ext(source)))
	if e = os.WriteFile(input, raw, 0600); e != nil {
		return nil, e
	}
	bin, e := exec.LookPath(k.Converter[0])
	if e != nil {
		return nil, e
	}
	readable := append([]string{filepath.Dir(bin)}, k.ConverterPaths...)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(k.ConverterSeconds)*time.Second)
	defer cancel()
	argv := append(append([]string{bin}, k.Converter[1:]...), input)
	cmd, e := sandboxed(ctx, argv, readable, work)
	if e != nil {
		return nil, fmt.Errorf("converter needs a sandbox: %w", e)
	}
	var out, errs bytes.Buffer
	cmd.Stdout = &limitedBuffer{b: &out, n: maxMarkdownBytes + 1}
	cmd.Stderr = &limitedBuffer{b: &errs, n: 2048}
	if e = cmd.Run(); e != nil {
		return nil, fmt.Errorf("converter failed: %w: %s", e, strings.TrimSpace(errs.String()))
	}
	return out.Bytes(), nil
}

type limitedBuffer struct {
	b *bytes.Buffer
	n int
}

func (w *limitedBuffer) Write(p []byte) (int, error) {
	if room := w.n - w.b.Len(); room > 0 {
		w.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuffer)(nil)
