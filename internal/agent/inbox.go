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
	business, err := s.admitBusiness(&p, &key)
	if err != nil {
		return 0, false, err
	}
	if !business && p.Kind != "message" && p.Kind != "action_request" {
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
	dedup := "external:" + key
	if p.WebChat != "" {
		if !validChatID(p.WebChat) || p.Kind != "message" {
			return 0, false, ErrDenied
		}
		dedup = "chat:" + p.WebChat + ":" + key
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
			e := tx.QueryRowContext(ctx, "SELECT available_at FROM inbox WHERE status='pending' AND retries=0 AND available_at>? AND json_extract(payload,'$.kind')='message' AND coalesce(payload->>'web_chat','')=? ORDER BY id LIMIT 1", now, p.WebChat).Scan(&window)
			if e == nil {
				due = window
			} else if !errors.Is(e, sql.ErrNoRows) {
				return e
			}
		}
		var e error
		id, inserted, e = insertEvent(ctx, tx, dedup, p, due)
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
		parked, other := s.cfg.parkedProviders()
		// An outstanding approval belongs to the channel that created its action.
		// Keep old-channel work queued until that provider is selected again.
		rows, e := tx.QueryContext(ctx, `SELECT id,dedup_key,payload,available_at,status,retries FROM inbox AS i
WHERE status IN ('pending','running','dead') AND json_extract(payload,'$.kind') NOT IN (?,?)
AND (json_extract(payload,'$.kind') NOT IN (SELECT value FROM json_each(?)) OR COALESCE(json_extract(payload,?),'')=?)
AND (json_extract(payload,'$.kind') != 'approval' OR NOT EXISTS (
 SELECT 1 FROM actions a JOIN inbox origin ON origin.id=CAST(substr(a.dedup_key,4) AS INTEGER)
 WHERE a.id=json_extract(i.payload,'$.action_id') AND a.dedup_key GLOB 'ev:[0-9]*:*'
 AND json_extract(origin.payload,'$.kind') IN (?,?)))
ORDER BY id LIMIT ?`, parked, other, scopedKinds, scopeField, s.eventScope(), parked, other, s.cfg.Limits.MaxBatch)
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
			if len(batch) > 0 && (ev.Payload.Kind != "message" || batch[0].Payload.Kind != "message" || ev.AvailableAt != batch[0].AvailableAt || ev.Retries != batch[0].Retries || ev.Payload.WebChat != batch[0].Payload.WebChat) {
				break
			}
			size += len(ev.Payload.Text)
			if len(batch) > 0 && size > s.cfg.Limits.MaxInputBytes {
				break
			}
			batch = append(batch, ev)
		}
		for _, ev := range batch {
			// The last input carries the link so a combined reply follows all its inputs.
			var head any
			if ev.Payload.WebChat != "" && ev.ID == batch[len(batch)-1].ID {
				head = batch[0].ID
			}
			if _, e = tx.ExecContext(ctx, "UPDATE inbox SET status='running',chat_head=? WHERE id=? AND status='pending'", head, ev.ID); e != nil {
				return e
			}
			if s.progressFor(ev.Payload) && s.cfg.Permissions["send_message"] == "allow" {
				n := Notification{Key: fmt.Sprintf("progress:%d", ev.ID), Target: ev.Payload.imTarget(ev.ID), Content: randomIMProgress()}
				if e := s.enqueueTx(context.WithValue(ctx, callScopeKey{}, callScope{Event: ev.ID}), tx, n); e != nil {
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
			seconds := s.cfg.Limits.RunSeconds
			if criticalKinds[batch[0].Payload.Kind] {
				seconds = s.cfg.StrongModel.RunSeconds
			}
			runCtx, cancel = context.WithTimeout(ctx, time.Duration(seconds)*time.Second)
			s.activeCancel = cancel
			s.activeAborted = false
			s.starts = append(s.starts, now)
		}
		s.mu.Unlock()
		s.logErr("claim", err)
		if len(batch) > 0 {
			attempt := callScope{Event: batch[0].ID, Attempt: randomID(), Purpose: batch[0].Payload.Kind}
			c := &capture{}
			runCtx = context.WithValue(context.WithValue(runCtx, callScopeKey{}, attempt), evidenceKey{}, c)
			initial := Outcome{Attempt: attempt.Attempt, Scope: s.scopeFor(runCtx, batch[0])}
			opening := s.prepareRun(batch, initial, "running", nil, nil)
			startErr := transaction(runCtx, s.db, func(tx *sql.Tx) error { return opening.save(runCtx, tx) })
			var outcome Outcome
			runErr := startErr
			if startErr == nil {
				outcome, runErr = s.execute(runCtx, batch)
			}
			outcome.Attempt = attempt.Attempt
			cancel()
			record := s.prepareRun(batch, outcome, "done", runErr, c)
			s.mu.Lock()
			aborted := s.activeAborted || ctx.Err() != nil
			var skipped *skippedError
			isSkipped := errors.As(runErr, &skipped)
			commitCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
			if startErr != nil {
				record.status = "failed"
				err = s.returnBatch(commitCtx, batch, false, nil)
			} else if aborted {
				record.status = "aborted"
				err = s.returnBatch(commitCtx, batch, false, &record)
			} else if isSkipped {
				// Effects of a failed critical run are dropped; the event is done.
				record.status = "skipped"
				err = s.commitBatch(commitCtx, batch, Outcome{Scope: outcome.Scope, Attempt: outcome.Attempt}, record)
			} else if runErr != nil {
				record.status = "failed"
				err = s.returnBatch(commitCtx, batch, true, &record)
			} else {
				err = s.commitBatch(commitCtx, batch, outcome, record)
			}
			stop()
			s.activeCancel = nil
			s.mu.Unlock()
			if err != nil {
				record.status = "commit_failed"
			}
			if err != nil && startErr == nil {
				// Only mark the opening entry. The failed transaction published no answer or effects.
				opening.status = "commit_failed"
				markCtx, finish := context.WithTimeout(context.Background(), 2*time.Second)
				s.logErr("mark failed run commit", transaction(markCtx, s.db, func(tx *sql.Tx) error { return opening.save(markCtx, tx) }))
				finish()
			}
			outcome.indexed = startErr == nil
			s.recordRun(batch, outcome, record.status, runErr)
			s.pruneRunEvidence(time.Now())
			s.logErr("execute", runErr)
			s.logErr("commit", err)
			if err != nil || (startErr != nil && !aborted) {
				// Do not run a second model turn against uncertain local commit state.
				s.logErr("pause after commit failure", s.SetPaused(true))
			} else if !aborted && runErr == nil && outcome.Scope == localScope && outcome.ReplyTarget == "" && len(outcome.Messages) > 0 {
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
func (s *Service) returnBatch(ctx context.Context, batch []Event, failure bool, record *runWrite) error {
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		var attempt string
		if record != nil {
			if err := record.save(ctx, tx); err != nil {
				return err
			}
			attempt = record.key
		}
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
				if e := s.enqueueTx(context.WithValue(ctx, callScopeKey{}, callScope{Event: ev.ID, Attempt: attempt}), tx, Notification{Key: fmt.Sprintf("failure:%d", ev.ID), Target: ev.Payload.imTarget(ev.ID), Content: "任务执行失败（已达到重试上限），请联系本机管理员查看 Inbox 死信并处理。"}); e != nil {
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
func (s *Service) commitBatch(ctx context.Context, batch []Event, out Outcome, records ...runWrite) error {
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		for _, r := range records {
			if err := r.save(ctx, tx); err != nil {
				return err
			}
		}
		ctx = context.WithValue(ctx, callScopeKey{}, callScope{Event: batch[0].ID, Attempt: out.Attempt})
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
			if existing.Status == "pending_approval" && batch[0].Payload.WebChat == "" {
				text := fmt.Sprintf("Approval required: %s\nAction: %s\nFrozen request: %s\nApprove/reject using POST /v1/actions/%s/decision. Expires: %s", a.Name, a.ID, a.Args, a.ID, time.UnixMilli(a.ExpiresAt).Format(time.RFC3339))
				if e = s.enqueueTx(ctx, tx, Notification{Key: "approval:" + a.ID, Target: out.ReplyTarget, Content: s.bounded(text), DeliverAt: time.Now().UnixMilli()}); e != nil {
					return e
				}
			}
		}
		if err := s.commitReplies(ctx, tx, batch[0], out); err != nil {
			return err
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
