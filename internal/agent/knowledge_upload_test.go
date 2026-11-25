package agent

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func uploadPayload(t *testing.T, selection []uploadChoice, contents ...string) ([]byte, string) {
	t.Helper()
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	meta, e := json.Marshal(selection)
	if e != nil {
		t.Fatal(e)
	}
	if e = m.WriteField("selection", string(meta)); e != nil {
		t.Fatal(e)
	}
	for _, v := range contents {
		f, e := m.CreateFormFile("file", "blob")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.WriteString(f, v); e != nil {
			t.Fatal(e)
		}
	}
	if e = m.Close(); e != nil {
		t.Fatal(e)
	}
	return body.Bytes(), m.FormDataContentType()
}
func uploadTo(t *testing.T, s *Service, id string, choices []uploadChoice, code int, contents ...string) uploadBatch {
	t.Helper()
	body, typ := uploadPayload(t, choices, contents...)
	req := httptest.NewRequest("POST", "/v1/knowledge/uploads?key="+id, bytes.NewReader(body))
	req.Host = s.cfg.Listen
	req.Header.Set("Content-Type", typ)
	req.Header.Set("Origin", "http://"+req.Host)
	req.Header.Set("X-Agentd-Workbench", "1")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != code {
		t.Fatalf("upload got %d want %d: %s", w.Code, code, w.Body.String())
	}
	if code >= 300 {
		return uploadBatch{}
	}
	waitFor(t, 4*time.Second, func() bool { s.knowledge.mu.Lock(); defer s.knowledge.mu.Unlock(); return !s.knowledge.running })
	b, e := s.uploadReceipt(id)
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestKnowledgeUploadVersions(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	first := uploadChoice{Name: "规则.md", Mode: "match"}
	initial := []uploadChoice{first, {Name: "另一个.txt", Mode: "match"}, {Name: "附件.pdf", Mode: "match"}, {Name: "损坏.txt", Mode: "match"}}
	key := stableID("initial")
	texts := []string{"# 标题\n检索短语", "retain", "%PDF-1.7", "\x00"}
	b := uploadTo(t, s, key, initial, 202, texts...)
	for i, v := range []string{"added", "added", "failed", "failed"} {
		if b.Files[i].Status != v {
			t.Fatal(b.Files[i])
		}
	}
	d := b.Files[0]
	raw, e := os.ReadFile(filepath.Join(s.root, knowledgeRaw, d.DocID+".md"))
	if e != nil || digest(raw) != d.SHA || string(raw) != texts[0] {
		t.Fatal("original mismatch", e)
	}
	uploadTo(t, s, key, initial, 200, texts...)
	uploadTo(t, s, key, []uploadChoice{first}, 409, "changed")
	for _, tc := range []struct{ mode, body, target, version, want string }{
		{"match", texts[0], "", "", "unchanged"}, {"match", "second", "", "", "failed"}, {"update", "second", d.DocID, d.SHA, "updated"}, {"update", "stale", d.DocID, d.SHA, "failed"}, {"copy", "third", "", "", "added"},
	} {
		f := uploadChoice{Name: first.Name, Mode: tc.mode, Target: tc.target, Version: tc.version}
		b = uploadTo(t, s, randomID()[:32], []uploadChoice{f}, 202, tc.body)
		if b.Files[0].Status != tc.want {
			t.Fatal(b.Files)
		}
	}
	docs, e := s.loadManifest()
	if e != nil || len(docs) != 3 || docs[d.DocID].SHA256 != digest([]byte("second")) {
		t.Fatal("versions", e)
	}
	paths, _ := filepath.Glob(filepath.Join(s.root, knowledgeUploads, ".receiving-*"))
	if len(paths) != 0 {
		t.Fatal("partial uploads retained")
	}
}
func TestUploadRecovery(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, testConfig(), nil)
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(root, knowledgeUploads, stableID("recover"))
	damaged := filepath.Join(root, knowledgeUploads, ".receiving-truncated")
	if e = os.MkdirAll(dir, 0700); e != nil {
		t.Fatal(e)
	}
	if e = os.Mkdir(damaged, 0700); e != nil {
		t.Fatal(e)
	}
	data := []byte("# recovered\nunique-after-restart")
	if e = os.WriteFile(filepath.Join(dir, "0"), data, 0600); e != nil {
		t.Fatal(e)
	}
	b := uploadBatch{ID: filepath.Base(dir), Files: []uploadFile{{uploadChoice: uploadChoice{Name: "work.md", Mode: "match"}, SHA: digest(data), Status: "processing"}}}
	if e = saveUpload(dir, b); e != nil {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, testConfig(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	waitFor(t, 3*time.Second, func() bool { b, e = s.uploadReceipt(b.ID); return e == nil && b.Files[0].Status == "added" })
	res, e := s.search(context.Background(), "unique-after-restart", "knowledge/md")
	if e != nil || !strings.Contains(res, b.Files[0].DocID) {
		t.Fatal(res, e)
	}
	waitFor(t, time.Second, func() bool { return !fileExists(filepath.Join(dir, "0")) })
	if fileExists(damaged) {
		t.Fatal("abandoned transmission retained")
	}
}
func TestSlowKnowledgeUpload(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	srv := httptest.NewUnstartedServer(s.Handler())
	srv.Config.ReadTimeout = 50 * time.Millisecond
	srv.Config.WriteTimeout = 50 * time.Millisecond
	srv.Start()
	defer srv.Close()
	data, typ := uploadPayload(t, []uploadChoice{{Name: "slow.md", Mode: "match"}}, "slow upload")
	input, output := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, e := output.Write(data[:len(data)/2])
		time.Sleep(160 * time.Millisecond)
		if e == nil {
			_, e = output.Write(data[len(data)/2:])
		}
		done <- errors.Join(e, output.Close())
	}()
	req, e := http.NewRequest("POST", srv.URL+"/v1/knowledge/uploads?key="+stableID("slow"), input)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", typ)
	res, e := srv.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	defer res.Body.Close()
	if res.StatusCode != 202 {
		t.Fatal(res.Status)
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
}

func TestUploadLimitsAndIncompleteTransmission(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	bad := []uploadChoice{{Name: "../bad.md", Mode: "match"}}
	uploadTo(t, s, stableID("name"), bad, 400, "text")
	many := make([]uploadChoice, maxUploadFiles+1)
	uploadTo(t, s, stableID("count"), many, 400)
	one := []uploadChoice{{Name: "incomplete.md", Mode: "match"}}
	uploadTo(t, s, stableID("missing"), one, 400)
	b := uploadTo(t, s, stableID("oversize text"), one, 202, strings.Repeat("x", maxMarkdownBytes+1))
	if b.Files[0].Status != "failed" {
		t.Fatal("oversized Markdown accepted")
	}
	// Generate the large part from a stream, without holding a test fixture in memory.
	dir := t.TempDir()
	body, typ := uploadPayload(t, one, "PLACEHOLDER")
	prefix, suffix, ok := bytes.Cut(body, []byte("PLACEHOLDER"))
	if !ok {
		t.Fatal("fixture")
	}
	chunk := strings.Repeat("x", 1<<20)
	readers := []io.Reader{bytes.NewReader(prefix)}
	for range 51 {
		readers = append(readers, strings.NewReader(chunk))
	}
	readers = append(readers, bytes.NewReader(suffix))
	r := httptest.NewRequest("POST", "/", io.MultiReader(readers...))
	r.Header.Set("Content-Type", typ)
	if _, err := stageUpload(r, dir, stableID("size")); err == nil {
		t.Fatal("oversized raw accepted")
	}
}

func TestKnowledgeUploadInterruptedConversion(t *testing.T) {
	cfg := testConfig()
	cfg.Knowledge.Converter = []string{"unavailable-on-restart"}
	root := t.TempDir()
	s, e := Open(root, cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	started := make(chan struct{})
	s.convert = func(ctx context.Context, _ string, _ []byte) ([]byte, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}
	choices := []uploadChoice{{Name: "first.pdf", Mode: "match"}, {Name: "second.md", Mode: "match"}}
	data, typ := uploadPayload(t, choices, "%PDF-1.7", "resume-next-file")
	id := stableID("cancel")
	r := httptest.NewRequest("POST", "/v1/knowledge/uploads?key="+id, bytes.NewReader(data))
	r.Header.Set("Authorization", "Bearer "+s.Token())
	r.Header.Set("Content-Type", typ)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatal(w.Code)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("conversion did not start")
	}
	if e = s.StartImport(root); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	waitFor(t, 3*time.Second, func() bool {
		b, e := s.uploadReceipt(id)
		return e == nil && b.Files[0].Status == "failed" && b.Files[1].Status == "added"
	})
}

func TestStalledUploadDoesNotBlockImports(t *testing.T) {
	s := openTest(t, testConfig(), nil)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	data, typ := uploadPayload(t, []uploadChoice{{Name: "slow.md", Mode: "match"}}, "# slow")
	input, output := io.Pipe()
	req, e := http.NewRequest("POST", srv.URL+"/v1/knowledge/uploads?key="+stableID("stalled"), input)
	if e != nil {
		t.Fatal(e)
	}
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Content-Type", typ)
	done := make(chan int, 1)
	go func() {
		res, e := srv.Client().Do(req)
		if e != nil {
			done <- 0
			return
		}
		res.Body.Close()
		done <- res.StatusCode
	}()
	if _, e = output.Write(data[:len(data)/2]); e != nil {
		t.Fatal(e)
	}
	waitFor(t, 2*time.Second, func() bool { s.knowledge.mu.Lock(); defer s.knowledge.mu.Unlock(); return s.knowledge.receiving == 1 })
	// While its bytes are still due, imports and other uploads proceed.
	src := filepath.Join(t.TempDir(), "other.md")
	if e = os.WriteFile(src, []byte("# other\nfree"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = s.StartImport(src); e != nil {
		t.Fatal("import blocked by a receiving upload:", e)
	}
	waitFor(t, 4*time.Second, func() bool { s.knowledge.mu.Lock(); defer s.knowledge.mu.Unlock(); return !s.knowledge.running })
	uploadTo(t, s, stableID("second"), []uploadChoice{{Name: "second.md", Mode: "match"}}, 202, "# second")
	if _, e = output.Write(data[len(data)/2:]); e != nil {
		t.Fatal(e)
	}
	output.Close()
	if code := <-done; code != 202 {
		t.Fatalf("stalled upload finished with %d", code)
	}
}

func TestIncomingImportResolvesSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	root := filepath.Join(t.TempDir(), "root")
	if e := os.Symlink(real, root); e != nil {
		t.Fatal(e)
	}
	s, e := Open(root, testConfig(), nil)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	in := filepath.Join(root, knowledgeIncoming)
	if e = os.WriteFile(filepath.Join(in, "a.md"), []byte("# a\nincoming"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = os.Symlink(t.TempDir(), filepath.Join(in, "escape")); e != nil {
		t.Fatal(e)
	}
	post := func(path string) int {
		body, _ := json.Marshal(map[string]string{"path": path})
		req := httptest.NewRequest("POST", "/v1/knowledge/incoming", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+s.Token())
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		return w.Code
	}
	for path, want := range map[string]int{"../x": 403, "escape": 403} {
		if got := post(path); got != want {
			t.Errorf("%s: got %d want %d", path, got, want)
		}
	}
	if got := post("a.md"); got != 200 {
		t.Fatalf("import under a symlinked root got %d", got)
	}
}

func TestKnowledgeBodyIsMasked(t *testing.T) {
	cfg := testConfig()
	cfg.Model.APIKey = "sk-test-mask-0123456789"
	s := openTest(t, cfg, nil)
	src := filepath.Join(t.TempDir(), "secret.md")
	if e := os.WriteFile(src, []byte("# s\nkey sk-test-mask-0123456789"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := s.StartImport(src); e != nil {
		t.Fatal(e)
	}
	waitFor(t, 4*time.Second, func() bool { s.knowledge.mu.Lock(); defer s.knowledge.mu.Unlock(); return !s.knowledge.running })
	req := httptest.NewRequest("GET", "/v1/knowledge/"+docID(src), nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 200 || strings.Contains(w.Body.String(), "sk-test-mask") || !strings.Contains(w.Body.String(), "key ***") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
}

func TestUploadFormatsFollowKnowledgeFormats(t *testing.T) {
	samples := map[string][]byte{".pdf": []byte("%PDF-1.7")}
	for ext, marker := range zipMarkers {
		var buf bytes.Buffer
		z := zip.NewWriter(&buf)
		if _, e := z.Create(marker); e != nil {
			t.Fatal(e)
		}
		z.Close()
		samples[ext] = buf.Bytes()
	}
	for _, ext := range append(slices.Clone(nativeFormats), convertedFormats...) {
		raw, ok := samples[ext]
		if !ok {
			raw = []byte("text")
		}
		if e := validateUploadContent("a"+ext, raw); e != nil {
			t.Errorf("%s rejected: %v", ext, e)
		}
	}
	// A CSV exported in GBK goes to the converter like a server import.
	if e := validateUploadContent("a.csv", []byte{0xb1, 0xea, 0xcc, 0xe2, ',', '1'}); e != nil {
		t.Error("GBK CSV rejected:", e)
	}
	for name, raw := range map[string][]byte{"a.exe": []byte("MZ"), "a.pdf": []byte("not pdf"), "a.md": {0xff, 0xfe}, "a.docx": []byte("PK")} {
		if validateUploadContent(name, raw) == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
