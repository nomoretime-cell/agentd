package agent

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Service struct {
	metricsSince  time.Time
	cfg           Config
	im            atomic.Pointer[imRuntime]
	imLife        imLifecycle
	root          string
	db, read      *sql.DB
	lock          *os.File
	model         Model
	strong        Model // nil unless strong_model.enabled; critical actions never fall back to model
	client        *http.Client
	token         string
	log           *slog.Logger
	mu            sync.Mutex // Coordinates control changes, claims and commits; never held during model inference.
	paused        bool
	activeCancel  context.CancelFunc
	activeAborted bool
	wake          chan struct{}
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	imConfigMu    sync.Mutex
	memoryMu      sync.Mutex
	consolidating atomic.Bool
	dreamChecking atomic.Bool
	// evidencePruned is the Unix millisecond time of the last run evidence pruning.
	evidencePruned atomic.Int64
	knowledge      knowledgeState
	convert        func(context.Context, string, []byte) ([]byte, error) // converts a knowledge file to Markdown
	life           context.Context                                       // background work such as imports; ends at Close
	endLife        context.CancelFunc
	started        bool
	lastSend       time.Time
	starts         []time.Time
}

func Open(root string, cfg Config, model Model) (s *Service, err error) {
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if cfg.OpenCodeBinary != "" {
		// Programs resolve against the working directory, not the state root.
		if cfg.OpenCodeBinary, err = filepath.Abs(cfg.OpenCodeBinary); err != nil {
			return nil, err
		}
		if fi, e := os.Stat(cfg.OpenCodeBinary); e != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0111 == 0 {
			return nil, errors.New("opencode_binary must point to an installed executable")
		}
		if !strings.HasSuffix(cfg.Model.Endpoint, "/chat/completions") {
			return nil, errors.New("OpenCode requires a model endpoint ending in /chat/completions")
		}
	}
	for _, name := range []string{"data", "data/deliveries", "data/outputs", "data/sessions", "data/memory", "data/gate", "data/knowledge/raw", "data/knowledge/tmp", knowledgeUploads, knowledgeIncoming, "workspace", "workspace/knowledge/md", "workspace/skills", "workspace/scratch"} {
		path := filepath.Join(root, name)
		if err = os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
		fi, e := os.Lstat(path)
		if e != nil {
			return nil, e
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("managed directory cannot be a symlink")
		}
	}
	lock, err := os.OpenFile(filepath.Join(root, "data/agentd.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("another agentd instance owns this data directory")
	}
	s = &Service{cfg: cfg, root: root, lock: lock, model: model, wake: make(chan struct{}, 1), log: slog.Default(), client: &http.Client{Timeout: time.Duration(cfg.Limits.ToolSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	defer func(opened *Service) {
		if err != nil {
			opened.Close()
		}
	}(s)
	s.rememberIMSecrets(cfg)
	s.imLife.factory = newIMChannel
	s.im.Store(&imRuntime{&s.cfg, newIMChannel(s, &s.cfg)})
	s.convert = s.runConverter
	s.life, s.endLife = context.WithCancel(context.Background())
	s.db, s.read, err = openDB(root)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(filepath.Join(root, "data/state.db"), 0600); err != nil {
		return nil, err
	}
	s.metricsSince, err = workbenchCoverage(root)
	if err != nil {
		return nil, err
	}
	s.paused = fileExists(filepath.Join(root, "data/paused.lock"))
	tokenPath := filepath.Join(root, "data/api-token")
	b, e := os.ReadFile(tokenPath)
	if errors.Is(e, os.ErrNotExist) {
		b = []byte(randomID())
		e = atomicWrite(tokenPath, b, 0600)
	}
	if e != nil {
		return nil, e
	}
	s.token = strings.TrimSpace(string(b))
	if len(s.token) < 32 {
		return nil, errors.New("API token must have at least 32 characters")
	}
	// Finish a memory promotion a crash interrupted before templates could
	// mistake its half-moved workspace for a new one.
	if err = s.finishPromotion(); err != nil {
		return nil, err
	}
	// Promotion moves these directories as units. Recreate them only after
	// recovery, otherwise an empty destination can hide a pending rename.
	for _, tree := range memoryTrees {
		dir := filepath.Join(root, "workspace", tree)
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		info, e := os.Lstat(dir)
		if e != nil || !info.IsDir() {
			return nil, fmt.Errorf("invalid managed memory directory %s", tree)
		}
	}
	// Templates are copied once and then belong to the operator and the model.
	for _, name := range []string{"MEMORY.md", "IDENTITY.md"} {
		p := filepath.Join(root, "workspace", name)
		if !fileExists(p) {
			content, e := defaults.ReadFile("defaults/" + name)
			if e != nil {
				return nil, e
			}
			if err = atomicWrite(p, content, 0600); err != nil {
				return nil, err
			}
		}
	}
	if s.model == nil {
		s.model = NewHTTPModel(cfg.Model)
	}
	if cfg.StrongModel.Enabled {
		s.strong = newStandardModel(cfg.StrongModel.ModelConfig)
	}
	err = transaction(context.Background(), s.db, func(tx *sql.Tx) error {
		for _, q := range []string{
			`UPDATE inbox SET run_index=(
				SELECT json_group_object(key,json(CASE value->>'status'
				WHEN 'running' THEN json_set(value,'$.status','interrupted') ELSE value END))
				FROM json_each(run_index)) WHERE status='running' AND run_index IS NOT NULL`,
			"UPDATE inbox SET retries=retries+1,status=CASE WHEN retries+1>=3 THEN 'dead' ELSE 'pending' END,chat_head=NULL WHERE status='running'", "UPDATE actions SET status='unknown',result='process interrupted; reconcile before retry' WHERE status='executing'", "UPDATE outbox SET status='unknown' WHERE status='sending'"} {
			if _, e := tx.Exec(q); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err = s.finishSkillPublish(); err != nil {
		return nil, err
	}
	if err = s.recoverMemoryEdit(); err != nil {
		return nil, err
	}
	if err = s.recoverUploads(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) Start(parent context.Context) {
	s.imConfigMu.Lock()
	defer s.imConfigMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return
	}
	s.started = true
	ctx, cancel := context.WithCancel(parent)
	s.cancel = cancel
	s.imLife.ctx = ctx
	s.startIM(nil)
	loops := []func(context.Context){s.workerLoop, s.timerLoop, s.outboxLoop}
	for _, f := range loops {
		s.wg.Add(1)
		go func(fn func(context.Context)) { defer s.wg.Done(); fn(ctx) }(f)
	}
}
func (s *Service) Close() error {
	s.knowledge.mu.Lock()
	if s.endLife != nil {
		s.endLife()
	}
	s.knowledge.mu.Unlock()
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	if s.activeCancel != nil {
		s.activeAborted = true
		s.activeCancel()
	}
	s.mu.Unlock()
	s.imConfigMu.Lock()
	s.stopIM()
	s.imConfigMu.Unlock()
	s.wg.Wait()
	var e error
	if s.read != nil {
		e = errors.Join(e, s.read.Close())
	}
	if s.db != nil {
		e = errors.Join(e, s.db.Close())
	}
	if s.lock != nil {
		e = errors.Join(e, s.lock.Close())
	}
	return e
}
func (s *Service) Token() string { return s.token }

// halted reports whether new work waits: the owner paused the service or the
// IM channel is being replaced. Caller holds s.mu.
func (s *Service) halted() bool { return s.paused || s.imLife.holding }

func (s *Service) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) SetPaused(paused bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := filepath.Join(s.root, "data/paused.lock")
	if paused {
		if e := atomicWrite(path, []byte("paused\n"), 0600); e != nil {
			return e
		}
	} else {
		if e := os.Remove(path); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if e := syncDir(filepath.Dir(path)); e != nil {
			return e
		}
	}
	s.paused = paused
	if paused && s.activeCancel != nil {
		s.activeAborted = true
		s.activeCancel()
	}
	s.notify()
	return nil
}
func (s *Service) retryDelay(n int) time.Duration {
	if n > 6 {
		n = 6
	}
	return time.Duration(s.cfg.Limits.RetryBaseMS) * time.Millisecond * time.Duration(1<<max(n-1, 0))
}
func randomID() string {
	var b [24]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func stableID(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:16]) }
func fileExists(p string) bool { _, e := os.Stat(p); return e == nil }
func atomicWrite(path string, b []byte, mode os.FileMode) error {
	f, e := os.CreateTemp(filepath.Dir(path), ".agentd-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if e = f.Chmod(mode); e == nil {
		_, e = f.Write(b)
	}
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	return syncDir(filepath.Dir(path))
}
func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Service) logErr(op string, e error) {
	if e != nil && !errors.Is(e, context.Canceled) {
		s.log.Error("operation failed", "operation", op, "error", s.redact(e.Error()))
	}
}
func (s *Service) redact(v string) string {
	return s.redactText(v, func(_, label string) string { return label })
}

// File views mask before slicing so even a page boundary inside a credential
// is safe. Keeping byte lengths preserves offsets and hashes of the source.
func (s *Service) maskedFile(v string) string {
	return s.redactText(v, func(value, _ string) string { return strings.Repeat("*", len(value)) })
}

// secretSpellings lists a secret as written and as JSON encodes it: tool
// results and evidence are JSON, which escapes <, >, &, quotes and controls.
func secretSpellings(secret string) []string {
	if secret == "" {
		return nil
	}
	b, _ := json.Marshal(secret)
	if escaped := string(b[1 : len(b)-1]); escaped != secret {
		return []string{secret, escaped}
	}
	return []string{secret}
}

func (s *Service) redactText(v string, mask func(value, label string) string) string {
	v = s.redactIM(v, mask)
	replace := func(secret string) {
		for _, spelling := range secretSpellings(secret) {
			v = strings.ReplaceAll(v, spelling, mask(spelling, "[redacted]"))
		}
	}
	replace(s.token)
	replace(s.cfg.Model.APIKey)
	replace(s.cfg.StrongModel.APIKey)
	for _, m := range s.cfg.DreamModels {
		replace(m.APIKey)
	}
	for _, a := range s.cfg.Actions {
		replace(a.BearerToken)
	}
	for _, t := range s.cfg.Targets {
		replace(t.Token)
	}
	return v
}
func (s *Service) bounded(v string) string {
	v = s.redact(v)
	if len(v) > s.cfg.Limits.MaxOutputBytes {
		return strings.ToValidUTF8(v[:s.cfg.Limits.MaxOutputBytes], "") + "\n[truncated]"
	}
	return v
}
func checkSize(v string, n int) error {
	if len(v) > n {
		return fmt.Errorf("input exceeds %d bytes", n)
	}
	return nil
}
