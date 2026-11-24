package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	knowledgeUploads  = "data/knowledge/uploads"
	knowledgeIncoming = "data/knowledge/incoming"
	maxUploadFiles    = 10
	maxUploadBytes    = 100 << 20
)

var batchKey = regexp.MustCompile("^[0-9a-f]{32}$")

type uploadChoice struct {
	Name    string `json:"name"`
	Mode    string `json:"mode"` // match, copy, update
	Target  string `json:"target,omitempty"`
	Version string `json:"version,omitempty"`
}
type uploadFile struct {
	uploadChoice
	SHA    string `json:"sha256"`
	Status string `json:"status"`
	DocID  string `json:"doc_id,omitempty"`
	Error  string `json:"error,omitempty"`
}
type uploadBatch struct {
	ID      string       `json:"id"`
	Created string       `json:"created"`
	Files   []uploadFile `json:"files"`
}

func (s *Service) registerKnowledgeUploads(m *http.ServeMux) {
	m.HandleFunc("POST /v1/knowledge/uploads", s.receiveUpload)
	m.HandleFunc("GET /v1/knowledge/uploads/{id}", func(w http.ResponseWriter, r *http.Request) {
		b, e := s.uploadReceipt(r.PathValue("id"))
		respond(w, b, e)
	})
	m.HandleFunc("GET /v1/knowledge/uploads", func(w http.ResponseWriter, r *http.Request) {
		dirs, e := os.ReadDir(filepath.Join(s.root, knowledgeUploads))
		if e != nil {
			respond(w, nil, e)
			return
		}
		batches := []uploadBatch{}
		for _, d := range dirs {
			if batchKey.MatchString(d.Name()) {
				b, e := s.uploadReceipt(d.Name())
				if e != nil {
					respond(w, nil, e)
					return
				}
				batches = append(batches, b)
			}
		}
		slices.SortFunc(batches, func(a, b uploadBatch) int { return strings.Compare(b.Created, a.Created) })
		respond(w, map[string]any{"batches": batches[:min(len(batches), 20)], "native": nativeFormats, "converted": convertedFormats, "converter": len(s.cfg.Knowledge.Converter) > 0, "max_file": maxSourceBytes, "max_text": maxMarkdownBytes, "max_count": maxUploadFiles, "max_total": maxUploadBytes}, nil)
	})
	m.HandleFunc("GET /v1/knowledge/search", func(w http.ResponseWriter, r *http.Request) {
		data, e := s.search(r.Context(), r.URL.Query().Get("q"), "knowledge/md")
		respond(w, json.RawMessage(data), e)
	})
	m.HandleFunc("GET /v1/knowledge/{id}", func(w http.ResponseWriter, r *http.Request) {
		docs, e := s.loadManifest()
		if e != nil {
			respond(w, nil, e)
			return
		}
		d, ok := docs[r.PathValue("id")]
		if !ok {
			respond(w, nil, sql.ErrNoRows)
			return
		}
		body, e := s.workspaceRead("knowledge/md/" + d.ID + ".md")
		respond(w, map[string]any{"document": d, "body": string(body)}, e)
	})
	// The workbench may import files deposited in the server's incoming folder.
	// The owner CLI retains the existing unrestricted server-path import.
	m.HandleFunc("POST /v1/knowledge/incoming", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Path string `json:"path"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		base := filepath.Join(s.root, knowledgeIncoming)
		if !filepath.IsLocal(req.Path) {
			respond(w, nil, ErrDenied)
			return
		}
		target, e := filepath.EvalSymlinks(filepath.Join(base, req.Path))
		if e == nil {
			rel, err := filepath.Rel(base, target)
			if err != nil || !filepath.IsLocal(rel) {
				e = ErrDenied
			} else {
				e = s.StartImport(target)
			}
		}
		respond(w, map[string]string{"import": "started"}, e)
	})
}

func (s *Service) uploadReceipt(id string) (uploadBatch, error) {
	var b uploadBatch
	if !batchKey.MatchString(id) {
		return b, sql.ErrNoRows
	}
	raw, e := os.ReadFile(filepath.Join(s.root, knowledgeUploads, id, "receipt.json"))
	if errors.Is(e, os.ErrNotExist) {
		return b, sql.ErrNoRows
	}
	if e != nil {
		return b, e
	}
	e = json.Unmarshal(raw, &b)
	return b, e
}
func saveUpload(dir string, b uploadBatch) error {
	raw, e := json.Marshal(b)
	if e != nil {
		return e
	}
	return atomicWrite(filepath.Join(dir, "receipt.json"), raw, 0600)
}

func (s *Service) receiveUpload(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("key")
	if !batchKey.MatchString(id) {
		respond(w, nil, errors.New("上传编号无效"))
		return
	}
	if err := s.beginImport(); err != nil {
		if errors.Is(err, ErrConflict) {
			writeJSON(w, 409, map[string]string{"error": "服务器正在导入资料，请稍后再试。"})
			return
		}
		respond(w, nil, err)
		return
	}
	handedOff := false
	defer func() {
		if !handedOff {
			s.finishImport(nil)
		}
	}()
	// Override the normal short API deadlines only after workbench access checks.
	deadline := time.Now().Add(10 * time.Minute)
	rc := http.NewResponseController(w)
	if e := rc.SetReadDeadline(deadline); e != nil && !errors.Is(e, http.ErrNotSupported) {
		respond(w, nil, e)
		return
	}
	if e := rc.SetWriteDeadline(deadline.Add(5 * time.Second)); e != nil && !errors.Is(e, http.ErrNotSupported) {
		respond(w, nil, e)
		return
	}
	interrupted := make(chan struct{})
	stop := context.AfterFunc(s.life, func() { defer close(interrupted); _ = rc.SetReadDeadline(time.Now()) })
	defer func() {
		if !stop() {
			<-interrupted
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	parent := filepath.Join(s.root, knowledgeUploads)
	staging, e := os.MkdirTemp(parent, ".receiving-")
	if e != nil {
		respond(w, nil, e)
		return
	}
	defer os.RemoveAll(staging)
	b, e := stageUpload(r, staging, id)
	if e != nil {
		respond(w, nil, e)
		return
	}
	prior, e := s.uploadReceipt(id)
	if e == nil {
		if len(prior.Files) != len(b.Files) {
			respond(w, nil, ErrConflict)
			return
		}
		for i, f := range b.Files {
			p := prior.Files[i]
			if f.uploadChoice != p.uploadChoice || f.SHA != p.SHA {
				respond(w, nil, ErrConflict)
				return
			}
		}
		respond(w, prior, nil)
		return
	}
	if !errors.Is(e, sql.ErrNoRows) {
		respond(w, nil, e)
		return
	}
	if e = saveUpload(staging, b); e == nil {
		e = os.Rename(staging, filepath.Join(parent, id))
	}
	if e != nil {
		respond(w, nil, e)
		return
	}
	if e = syncDir(parent); e != nil {
		respond(w, nil, e)
		return
	}
	handedOff = true
	s.runUploads([]uploadBatch{b})
	writeJSON(w, http.StatusAccepted, b)
}

func stageUpload(r *http.Request, dir, id string) (uploadBatch, error) {
	b := uploadBatch{ID: id, Created: time.Now().UTC().Format(time.RFC3339Nano)}
	reader, e := r.MultipartReader()
	if e != nil {
		return b, errors.New("需要 multipart 文件上传")
	}
	part, e := reader.NextPart()
	if e != nil || part.FormName() != "selection" {
		return b, errors.New("缺少文件清单")
	}
	var choices []uploadChoice
	meta, e := io.ReadAll(io.LimitReader(part, 16385))
	if e != nil || len(meta) > 16384 || json.Unmarshal(meta, &choices) != nil || len(choices) == 0 || len(choices) > maxUploadFiles {
		return b, errors.New("每批请选择 1 至 10 个文件")
	}
	var total int64
	for i, choice := range choices {
		name := choice.Name
		if len(name) > 240 || !utf8.ValidString(name) || strings.TrimSpace(name) == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return b, errors.New("文件名无效")
		}
		if choice.Mode != "match" && choice.Mode != "copy" && choice.Mode != "update" {
			return b, errors.New("同名处理方式无效")
		}
		p, e := reader.NextPart()
		if e != nil || p.FormName() != "file" {
			return b, errors.New("文件传输不完整")
		}
		f, e := os.OpenFile(filepath.Join(dir, strconv.Itoa(i)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			return b, e
		}
		hash := sha256.New()
		n, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(p, maxSourceBytes+1))
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return b, e
		}
		total += n
		if n > maxSourceBytes || total > maxUploadBytes {
			return b, errors.New("原件限 50 MiB，每批限 100 MiB")
		}
		b.Files = append(b.Files, uploadFile{uploadChoice: choice, SHA: hex.EncodeToString(hash.Sum(nil)), Status: "processing"})
	}
	if _, e = reader.NextPart(); e != io.EOF {
		return b, errors.New("文件清单与上传内容不一致")
	}
	return b, nil
}

func (s *Service) recoverUploads() error {
	parent := filepath.Join(s.root, knowledgeUploads)
	dirs, e := os.ReadDir(parent)
	if e != nil {
		return e
	}
	var pending []uploadBatch
	for _, d := range dirs {
		if strings.HasPrefix(d.Name(), ".receiving-") {
			if e = os.RemoveAll(filepath.Join(parent, d.Name())); e != nil {
				return e
			}
			continue
		}
		if !batchKey.MatchString(d.Name()) {
			continue
		}
		b, e := s.uploadReceipt(d.Name())
		if e != nil {
			return e
		}
		incomplete := false
		for i, f := range b.Files {
			if f.Status == "processing" {
				incomplete = true
			} else {
				if e := os.Remove(filepath.Join(parent, b.ID, strconv.Itoa(i))); e != nil && !errors.Is(e, os.ErrNotExist) {
					return e
				}
			}
		}
		if incomplete {
			pending = append(pending, b)
		}
	}
	if len(pending) > 0 {
		if e := s.beginImport(); e != nil {
			return e
		}
		s.runUploads(pending)
	}
	return nil
}
func (s *Service) runUploads(batches []uploadBatch) {
	go func() {
		defer s.finishImport(nil)
		for _, b := range batches {
			// HTTP is still serializing the accepted receipt.
			b.Files = slices.Clone(b.Files)
			dir := filepath.Join(s.root, knowledgeUploads, b.ID)
			for i, f := range b.Files {
				if f.Status != "processing" {
					continue
				}
				file := filepath.Join(dir, strconv.Itoa(i))
				result, e := s.acceptUpload(s.life, b.ID, i, f, file)
				// Keep the staged bytes for recovery if shutdown interrupted conversion.
				if s.life.Err() != nil {
					return
				}
				if e != nil {
					result = importResult{Status: "failed", Error: s.redact(e.Error())}
				}
				b.Files[i].DocID, b.Files[i].Status, b.Files[i].Error = result.DocID, result.Status, result.Error
				if e = saveUpload(dir, b); e != nil {
					s.logErr("save knowledge upload receipt", e)
					return
				}
				result.Source = f.Name
				s.logImport([]importResult{result})
				if e = os.Remove(file); e != nil && !errors.Is(e, os.ErrNotExist) {
					s.logErr("clean knowledge upload", e)
				}
			}
		}
	}()
}

func (s *Service) acceptUpload(ctx context.Context, batch string, i int, f uploadFile, file string) (importResult, error) {
	var result importResult
	docs, e := s.loadManifest()
	if e != nil {
		return result, e
	}
	source := "upload:" + batch + "-" + strconv.Itoa(i) + "/" + f.Name
	switch f.Mode {
	case "update":
		d, ok := docs[f.Target]
		if !ok || !strings.HasPrefix(d.Source, "upload:") || filepath.Base(d.Source) != f.Name || (d.SHA256 != f.Version && d.SHA256 != f.SHA) {
			return result, errors.New("文档已变化，请刷新后重新选择更新目标")
		}
		source = d.Source
	case "match":
		var match string
		conflict := false
		for _, d := range docs {
			if !strings.HasPrefix(d.Source, "upload:") || filepath.Base(d.Source) != f.Name {
				continue
			}
			if d.SHA256 == f.SHA {
				if match == "" || d.Source < match {
					match = d.Source
				}
			} else {
				conflict = true
			}
		}
		if match != "" {
			source = match
		} else if conflict {
			return result, errors.New("已存在同名文档，请选择更新已有版本或另存新文档")
		}
	}
	raw, e := os.ReadFile(file)
	if e != nil {
		return result, e
	}
	if digest(raw) != f.SHA {
		return result, errors.New("上传原件校验失败")
	}
	if e = validateUploadContent(f.Name, raw); e != nil {
		return result, e
	}
	result = s.importBytes(ctx, source, raw, docs)
	if result.Status != "failed" {
		s.memoryMu.Lock()
		e = s.saveKnowledge(docs)
		s.memoryMu.Unlock()
	}
	return result, e
}

func validateUploadContent(name string, raw []byte) error {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".pdf":
		if bytes.HasPrefix(raw, []byte("%PDF-")) {
			return nil
		}
	case ".docx", ".pptx", ".xlsx", ".epub":
		z, e := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if e == nil {
			marker := map[string]string{".docx": "word/document.xml", ".pptx": "ppt/presentation.xml", ".xlsx": "xl/workbook.xml", ".epub": "META-INF/container.xml"}[ext]
			for _, f := range z.File {
				if f.Name == marker {
					return nil
				}
			}
		}
	case ".md", ".markdown", ".txt", ".csv", ".html", ".htm":
		if len(bytes.TrimSpace(raw)) > 0 && utf8.Valid(raw) && !bytes.ContainsRune(raw, 0) {
			return nil
		}
	default:
		return errors.New("不支持此文件格式")
	}
	return errors.New("文件内容与格式不符，文本须为非空 UTF-8")
}
