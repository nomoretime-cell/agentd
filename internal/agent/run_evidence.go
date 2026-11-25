package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Evidence is owner-only. In particular it never enters model-searchable transcripts.
type evidenceKey struct{}
type evidenceSource struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Offset    int    `json:"offset"`
	End       int    `json:"end"`
}
type evidenceUse struct {
	Step   string `json:"step"`
	Source int    `json:"source"`
}
type runEvidence struct {
	Sources []evidenceSource `json:"sources"`
	Uses    []evidenceUse    `json:"uses"`
	Omitted int              `json:"omitted"`
}
type capture struct {
	mu sync.Mutex
	runEvidence
	pending []evidenceSource
	bytes   int
}

func captureFrom(ctx context.Context) *capture { c, _ := ctx.Value(evidenceKey{}).(*capture); return c }
func clipEvidence(v string, limit int) (string, bool) {
	if len(v) <= limit {
		return v, false
	}
	for limit > 0 && !utf8.RuneStart(v[limit]) {
		limit--
	}
	return v[:limit], true
}
func (s *Service) source(kind, name, version, text string) evidenceSource {
	text = s.redact(text)
	excerpt, cut := clipEvidence(text, 16<<10)
	return evidenceSource{Kind: kind, Name: s.redact(name), Version: version, Digest: digest([]byte(text)), Text: excerpt, Truncated: cut}
}
func (c *capture) add(step string, src evidenceSource) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.addLocked(step, src)
}
func (c *capture) addLocked(step string, src evidenceSource) {
	if len(c.Uses) >= 512 {
		c.Omitted++
		return
	}
	for i, prior := range c.Sources {
		if prior.Kind == src.Kind && prior.Name == src.Name && prior.Digest == src.Digest && prior.Version == src.Version && prior.Offset == src.Offset && prior.End == src.End {
			c.Uses = append(c.Uses, evidenceUse{step, i})
			return
		}
	}
	if len(c.Sources) >= 256 || c.bytes+len(src.Text) > 512<<10 {
		c.Omitted++
		return
	}
	c.bytes += len(src.Text)
	c.Uses = append(c.Uses, evidenceUse{step, len(c.Sources)})
	c.Sources = append(c.Sources, src)
}
func (c *capture) staged(src evidenceSource) {
	if c != nil {
		c.pending = append(c.pending, src)
	}
}
func (s *Service) submitted(ctx context.Context, step string, messages []Message) {
	c := captureFrom(ctx)
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, src := range c.pending {
		c.addLocked(step, src)
	}
	for _, msg := range messages {
		// Include tool call IDs/arguments as sent, even if the text is empty.
		msg.Content = s.redact(msg.Content)
		b, _ := json.Marshal(msg)
		src := s.source("request", msg.Role, "", string(b))
		if msg.historyID != "" {
			src.Kind, src.Name = "history", msg.historyID
		}
		c.addLocked(step, src)
	}
}
func (s *Service) captureTool(ctx context.Context, key string, call ToolCall, result string, err error) {
	c := captureFrom(ctx)
	if c == nil {
		return
	}
	name := call.Function.Name
	c.add(key, s.source("tool_args", name, "", call.Function.Arguments))
	if err != nil {
		c.add(key, s.source("tool_error", name, "", err.Error()))
		return
	}
	if name == "read_file" {
		var page struct {
			Path, SHA256, Content string
			Next                  int `json:"next_offset"`
		}
		if json.Unmarshal([]byte(result), &page) == nil && page.Path != "" {
			src := s.source("read", page.Path, page.SHA256, page.Content)
			src.End = page.Next
			var args struct{ Offset int }
			_ = json.Unmarshal([]byte(call.Function.Arguments), &args)
			src.Offset = args.Offset
			c.add(key, src)
			return
		}
	}
	if name == "search" {
		var r searchResult
		if json.Unmarshal([]byte(result), &r) == nil {
			for _, hit := range r.Hits {
				src := s.source("hit", hit.Path, "", hit.Snippet)
				src.Offset = int(hit.Offset)
				c.add(key, src)
			}
		}
	}
	c.add(key, s.source("tool", name, "", result))
}

type runWrite struct {
	id                       int64
	key, index, data, status string
}

func (s *Service) prepareRun(batch []Event, out Outcome, status string, runErr error, c *capture) runWrite {
	rec := s.runRecord(batch, out, status, runErr)
	now := time.Now()
	rec["ts"] = now.Format(time.RFC3339Nano)
	rec["at"] = now.UnixMilli()
	rec["channel"] = runChannel(batch[0], out.ReplyTarget)
	raw, _ := json.Marshal(rec)
	data := "null"
	if c != nil {
		c.mu.Lock()
		b, _ := json.Marshal(c.runEvidence)
		c.mu.Unlock()
		data = string(b)
	}
	return runWrite{batch[0].ID, out.Attempt, string(raw), data, status}
}
func (r runWrite) save(ctx context.Context, tx *sql.Tx) error {
	// Keep the original attempt timestamp for stable pagination. The bounds
	// let date queries skip old events before expanding their attempts.
	result, err := tx.ExecContext(ctx, `UPDATE inbox SET
	  run_index=json_set(coalesce(run_index,'{}'),
	    '$._first',coalesce(run_index->>'_first',json_extract(?1,'$.at')),
	    '$._last',max(coalesce(run_index->>'_last',0),json_extract(?1,'$.at')),
	    ?3,json_set(json(?1),'$.status',?2,
	      '$.at',coalesce(json_extract(run_index,?3||'.at'),json_extract(?1,'$.at')),
	      '$.ts',coalesce(json_extract(run_index,?3||'.ts'),json_extract(?1,'$.ts')))),
	  run_data=json_set(coalesce(run_data,'{}'),?3,json(?4))
	  WHERE id=?5 AND status='running'`, r.index, r.status, "$."+r.key, r.data, r.id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return ErrConflict
	}
	return err
}

// Run evidence holds the redacted request of every attempt, so it is kept only
// for the longest range the workbench browses. The run index stays, and run
// details for older attempts show the transcript record without evidence.
const runEvidenceDays = 30

// pruneRunEvidence runs at most hourly from the worker after a batch settles,
// so retention needs no background loop of its own. Only finished events are
// pruned; dead events keep their evidence until the owner resolves them.
func (s *Service) pruneRunEvidence(now time.Time) {
	last := s.evidencePruned.Load()
	if now.UnixMilli()-last < time.Hour.Milliseconds() || !s.evidencePruned.CompareAndSwap(last, now.UnixMilli()) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE inbox SET run_data=NULL
	  WHERE run_data IS NOT NULL AND status='done' AND run_index IS NOT NULL AND run_index->>'_last'<?`,
		now.AddDate(0, 0, -runEvidenceDays).UnixMilli())
	s.logErr("prune run evidence", err)
}

func durableRunID(event int64, attempt string) string { return fmt.Sprintf("%d~%s", event, attempt) }
func parseRunID(id string) (int64, string, error) {
	ev, key, ok := strings.Cut(id, "~")
	n, e := strconv.ParseInt(ev, 10, 64)
	if !ok || n < 1 || e != nil || len(key) != 48 || strings.Trim(key, "0123456789abcdef") != "" {
		return 0, "", errors.New("invalid run identifier")
	}
	return n, key, nil
}
func (s *Service) indexedRun(ctx context.Context, id string) (historyRecord, error) {
	var rec historyRecord
	event, attempt, err := parseRunID(id)
	if err != nil {
		return rec, err
	}
	path := "$." + attempt
	var raw, trace string
	err = s.read.QueryRowContext(ctx, `SELECT json_extract(run_index,?),coalesce(json_extract(run_data,?),'null') FROM inbox WHERE id=? AND json_type(run_index,?)='object'`, path, path, event, path).Scan(&raw, &trace)
	if err != nil {
		return rec, err
	}
	rec, err = s.decodeHistory([]byte(raw))
	if err != nil {
		return rec, err
	}
	if err = json.Unmarshal([]byte(trace), &rec.Evidence); err != nil {
		return rec, err
	}
	if rec.Evidence != nil {
		for i := range rec.Evidence.Sources {
			src := &rec.Evidence.Sources[i]
			src.Text = s.redact(src.Text)
			src.Name = s.redact(src.Name)
		}
	}
	return s.runReceipts(ctx, rec)
}
