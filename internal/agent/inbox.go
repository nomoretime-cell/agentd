package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

func (s *Service) Append(ctx context.Context, key string, p Payload) (id int64, inserted bool, err error) {
	if p.fromIM() {
		return 0, false, ErrDenied
	}
	if key == "" || len(key) > 256 {
		return 0, false, errors.New("key must contain 1..256 bytes")
	}
	if p.Kind == "news_test" {
		if s.cfg.provider() == "wecom" {
			p.NewsTarget = s.ownerTarget()
			key = "wecom:" + stableID(p.NewsTarget+"\x00"+key)
		}
		if !s.canMessageOwner() {
			return 0, false, ErrDenied
		}
		if _, ok := s.testNewsFeed(p.Name); !ok {
			return 0, false, errors.New("unknown news source")
		}
	}
	if p.Kind != "message" && p.Kind != "action_request" && p.Kind != "news_test" {
		return 0, false, errors.New("unsupported external event kind")
	}
	if strings.TrimSpace(p.Text) == "" && p.Kind == "message" {
		return 0, false, errors.New("text is required")
	}
	b, _ := json.Marshal(p)
	if e := checkSize(string(b), s.cfg.Limits.MaxInputBytes); e != nil {
		return 0, false, e
	}
	if p.Kind == "action_request" {
		if op, ok := s.cfg.Actions[p.Name]; !ok || op.Mode == "deny" {
			return 0, false, ErrDenied
		}
		if len(p.Arguments) == 0 {
			p.Arguments = json.RawMessage(`{}`)
		}
		var obj map[string]any
		if e := json.Unmarshal(p.Arguments, &obj); e != nil || obj == nil {
			return 0, false, errors.New("arguments must be an object")
		}
		p.Arguments, _ = json.Marshal(obj)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err = transaction(ctx, s.db, func(tx *sql.Tx) error {
		now := time.Now().UnixMilli()
		due := now
		if p.Kind == "message" {
			due = now + int64(s.cfg.Limits.DebounceMS)
			var window int64
			// Only ordinary messages may open/join a debounce window, never retries or controls.
			e := tx.QueryRowContext(ctx, "SELECT available_at FROM inbox WHERE status='pending' AND retries=0 AND available_at>? AND json_extract(payload,'$.kind')='message' ORDER BY id LIMIT 1", now).Scan(&window)
			if e == nil {
				due = window
			} else if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		var e error
		id, inserted, e = insertEvent(ctx, tx, "external:"+key, p, due)
		return e
	})
	if err == nil {
		s.notify()
	}
	return
}

func (s *Service) claim(ctx context.Context) (batch []Event, err error) {
	// Caller holds s.mu. Dead heads deliberately block until an authenticated operator resolves them.
	err = transaction(ctx, s.db, func(tx *sql.Tx) error {
		// An outstanding approval belongs to the channel that created its action.
		// Keep old-channel work queued until that provider is selected again.
		rows, e := tx.QueryContext(ctx, `SELECT id,dedup_key,payload,available_at,status,retries FROM inbox AS i
WHERE status IN ('pending','running','dead') AND json_extract(payload,'$.kind') != ?
AND (json_extract(payload,'$.kind') NOT IN ('news_test','ai_news') OR COALESCE(json_extract(payload,'$.news_target'),'')=?)
AND (json_extract(payload,'$.kind') != 'approval' OR NOT EXISTS (
 SELECT 1 FROM actions a JOIN inbox origin ON origin.id=CAST(substr(a.dedup_key,4) AS INTEGER)
 WHERE a.id=json_extract(i.payload,'$.action_id') AND a.dedup_key GLOB 'ev:[0-9]*:*'
 AND json_extract(origin.payload,'$.kind')=?))
ORDER BY id LIMIT ?`, s.cfg.inactiveProvider(), s.newsScope(), s.cfg.inactiveProvider(), s.cfg.Limits.MaxBatch)
		if e != nil {
			return e
		}
		var candidates []Event
		for rows.Next() {
			var ev Event
			var raw string
			if e = rows.Scan(&ev.ID, &ev.Key, &raw, &ev.AvailableAt, &ev.Status, &ev.Retries); e != nil {
				rows.Close()
				return e
			}
			if e = json.Unmarshal([]byte(raw), &ev.Payload); e != nil {
				rows.Close()
				return e
			}
			candidates = append(candidates, ev)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		now := time.Now().UnixMilli()
		size := 0
		for _, ev := range candidates {
			if ev.Status != "pending" || ev.AvailableAt > now {
				break
			}
			if len(batch) > 0 && (ev.Payload.Kind != "message" || batch[0].Payload.Kind != "message" || ev.AvailableAt != batch[0].AvailableAt || ev.Retries != batch[0].Retries) {
				break
			}
			size += len(ev.Payload.Text)
			if len(batch) > 0 && size > s.cfg.Limits.MaxInputBytes {
				break
			}
			batch = append(batch, ev)
		}
		for _, ev := range batch {
			if _, e = tx.ExecContext(ctx, "UPDATE inbox SET status='running' WHERE id=? AND status='pending'", ev.ID); e != nil {
				return e
			}
			if s.progressFor(ev.Payload) && s.cfg.Permissions["send_message"] == "allow" {
				n := Notification{Key: fmt.Sprintf("progress:%d", ev.ID), Target: ev.Payload.imTarget(ev.ID), Content: randomIMProgress()}
				if e := s.enqueueTx(ctx, tx, n); e != nil {
					return e
				}
			}
		}
		return nil
	})
	if err != nil {
		batch = nil
	}
	return
}

func (s *Service) workerLoop(ctx context.Context) {
	tick := time.NewTicker(time.Duration(s.cfg.Limits.PollMS) * time.Millisecond)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		now := time.Now()
		cut := now.Add(-time.Hour)
		for len(s.starts) > 0 && s.starts[0].Before(cut) {
			s.starts = s.starts[1:]
		}
		var batch []Event
		var err error
		if !s.paused && len(s.starts) < s.cfg.Limits.MaxRunsPerHour {
			batch, err = s.claim(ctx)
		}
		var runCtx context.Context
		var cancel context.CancelFunc
		if len(batch) > 0 {
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(s.cfg.Limits.RunSeconds)*time.Second)
			s.activeCancel = cancel
			s.activeAborted = false
			s.starts = append(s.starts, now)
		}
		s.mu.Unlock()
		s.logErr("claim", err)
		if len(batch) > 0 {
			outcome, runErr := s.execute(runCtx, batch)
			cancel()
			s.mu.Lock()
			aborted := s.activeAborted || ctx.Err() != nil
			commitCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			if aborted {
				err = s.returnBatch(commitCtx, batch, false)
			} else if runErr != nil {
				err = s.returnBatch(commitCtx, batch, true)
			} else {
				err = s.commitBatch(commitCtx, batch, outcome)
			}
			stop()
			s.activeCancel = nil
			s.mu.Unlock()
			s.logErr("execute", runErr)
			s.logErr("commit", err)
			if err != nil {
				// Do not run a second model turn against uncertain local commit state.
				s.logErr("pause after commit failure", s.SetPaused(true))
			} else if !aborted && runErr == nil && outcome.ReplyTarget == "" && len(outcome.Messages) > 0 {
				s.logErr("session cache", s.saveSession(outcome.Messages))
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-tick.C:
		}
	}
}
func (s *Service) returnBatch(ctx context.Context, batch []Event, failure bool) error {
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		for _, ev := range batch {
			n := ev.Retries
			status := "pending"
			due := time.Now().UnixMilli()
			if failure {
				n++
				due = time.Now().Add(s.retryDelay(n)).UnixMilli()
				if n >= 3 {
					status = "dead"
				}
			}
			if status == "dead" && s.permitsIM(ev.Payload) {
				if e := s.enqueueTx(ctx, tx, Notification{Key: fmt.Sprintf("failure:%d", ev.ID), Target: ev.Payload.imTarget(ev.ID), Content: "任务执行失败（已达到重试上限），请联系本机管理员查看 Inbox 死信并处理。"}); e != nil {
					return e
				}
			}
			if _, e := tx.ExecContext(ctx, "UPDATE inbox SET status=?,retries=?,available_at=? WHERE id=? AND status='running'", status, n, due, ev.ID); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Service) commitBatch(ctx context.Context, batch []Event, out Outcome) error {
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		for _, n := range out.Notifications {
			if e := s.enqueueTx(ctx, tx, n); e != nil {
				return e
			}
		}
		for _, t := range out.Timers {
			if e := upsertTimerTx(ctx, tx, t); e != nil {
				return e
			}
		}
		if out.Approval != nil {
			a := out.Approval
			_, e := tx.ExecContext(ctx, "INSERT INTO actions(id,dedup_key,tool_name,tool_args,resume_hint,status,expires_at) VALUES(?,?,?,?,?,'pending_approval',?) ON CONFLICT(dedup_key) DO NOTHING", a.ID, a.Key, a.Name, a.Args, a.Hint, a.ExpiresAt)
			if e != nil {
				return e
			}
			existing, e := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM actions WHERE dedup_key=?", a.Key))
			if e != nil {
				return e
			}
			if existing.Name != a.Name || existing.Args != a.Args {
				return ErrConflict
			}
			if existing.Status == "pending_approval" {
				text := fmt.Sprintf("Approval required: %s\nAction: %s\nFrozen request: %s\nApprove/reject using POST /v1/actions/%s/decision. Expires: %s", a.Name, a.ID, a.Args, a.ID, time.UnixMilli(a.ExpiresAt).Format(time.RFC3339))
				if e = s.enqueueTx(ctx, tx, Notification{Key: "approval:" + a.ID, Target: out.ReplyTarget, Content: s.bounded(text), DeliverAt: time.Now().UnixMilli()}); e != nil {
					return e
				}
			}
		}
		for _, ev := range batch {
			res, e := tx.ExecContext(ctx, "UPDATE inbox SET status='done' WHERE id=? AND status='running'", ev.ID)
			if e != nil {
				return e
			}
			n, _ := res.RowsAffected()
			if n != 1 {
				return ErrConflict
			}
		}
		return nil
	})
}
func (s *Service) ResolveEvent(ctx context.Context, id int64, op string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var e error
	var r sql.Result
	switch op {
	case "retry":
		r, e = s.db.ExecContext(ctx, "UPDATE inbox SET status='pending',retries=0,available_at=? WHERE id=? AND status='dead'", time.Now().UnixMilli(), id)
	case "skip":
		r, e = s.db.ExecContext(ctx, "UPDATE inbox SET status='done' WHERE id=? AND status IN ('dead','pending')", id)
	default:
		return errors.New("operation must be retry or skip")
	}
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	s.notify()
	return nil
}
