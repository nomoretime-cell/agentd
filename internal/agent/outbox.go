package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (s *Service) enqueueTx(ctx context.Context, tx *sql.Tx, n Notification) error {
	if n.Target == "" {
		n.Target = s.cfg.DefaultTarget
	}
	if imReplyTarget(n.Target) {
		if _, e := s.imRoute(ctx, tx, n.Target); e != nil {
			return e
		}
	} else if imOwnerTarget(n.Target) {
		if n.Target != s.ownerTarget() || !s.canMessageOwner() || !proactiveKey(n.Key) {
			return ErrDenied
		}
	} else if _, ok := s.cfg.Targets[n.Target]; !ok {
		return ErrDenied
	}
	if strings.TrimSpace(n.Content) == "" {
		return errors.New("notification is empty")
	}
	n.Content = s.redact(n.Content)
	if e := checkSize(n.Content, s.cfg.Limits.MaxOutputBytes); e != nil {
		return e
	}
	if n.DeliverAt == 0 {
		n.DeliverAt = time.Now().UnixMilli()
	}
	origin, _ := ctx.Value(callScopeKey{}).(callScope)
	_, e := tx.ExecContext(ctx, "INSERT INTO outbox(dedup_key,target,content,deliver_at,origin_event,origin_attempt) VALUES(?,?,?,?,nullif(?,0),nullif(?,'')) ON CONFLICT(dedup_key) DO NOTHING", n.Key, n.Target, n.Content, n.DeliverAt, origin.Event, origin.Attempt)
	if e != nil {
		return e
	}
	var target, content string
	if e = tx.QueryRowContext(ctx, "SELECT target,content FROM outbox WHERE dedup_key=?", n.Key).Scan(&target, &content); e != nil {
		return e
	}
	// A retried progress hint keeps the wording originally queued for this event.
	if target != n.Target || (content != n.Content && !strings.HasPrefix(n.Key, "progress:")) {
		return fmt.Errorf("%w: notification changed on replay", ErrConflict)
	}
	return nil
}
func (s *Service) quietUntil(now time.Time) time.Time {
	if s.cfg.Quiet.Start == "" || s.cfg.Quiet.Start == s.cfg.Quiet.End {
		return now
	}
	loc, _ := time.LoadLocation(s.cfg.Timezone)
	local := now.In(loc)
	st, _ := time.Parse("15:04", s.cfg.Quiet.Start)
	en, _ := time.Parse("15:04", s.cfg.Quiet.End)
	start := time.Date(local.Year(), local.Month(), local.Day(), st.Hour(), st.Minute(), 0, 0, loc)
	end := time.Date(local.Year(), local.Month(), local.Day(), en.Hour(), en.Minute(), 0, 0, loc)
	if start.Before(end) {
		if !local.Before(start) && local.Before(end) {
			return end
		}
	} else {
		if !local.Before(start) {
			return end.AddDate(0, 0, 1)
		}
		if local.Before(end) {
			return end
		}
	}
	return now
}
func (s *Service) deliverOne(ctx context.Context) error {
	s.mu.Lock()
	now := time.Now()
	if s.paused || ctx.Err() != nil || now.Before(s.lastSend.Add(time.Duration(s.cfg.DeliveryIntervalMS)*time.Millisecond)) {
		s.mu.Unlock()
		return nil
	}
	var n Notification
	parkedReply, parkedOwner := s.cfg.parkedTargetGlobs()
	e := s.db.QueryRowContext(ctx, "SELECT id,dedup_key,target,content,deliver_at,status,retries FROM outbox WHERE status='pending' AND deliver_at<=? AND target NOT GLOB ? AND target NOT GLOB ? ORDER BY id LIMIT 1", now.UnixMilli(), parkedReply, parkedOwner).Scan(&n.ID, &n.Key, &n.Target, &n.Content, &n.DeliverAt, &n.Status, &n.Retries)
	if errors.Is(e, sql.ErrNoRows) {
		s.mu.Unlock()
		return nil
	}
	if e != nil {
		s.mu.Unlock()
		return e
	}
	// An expired progress hint should never follow the final answer, including after a 429 or restart.
	if eventID, progress := strings.CutPrefix(n.Key, "progress:"); progress {
		res, err := s.db.ExecContext(ctx, "DELETE FROM outbox WHERE id=? AND NOT EXISTS (SELECT 1 FROM inbox WHERE id=? AND status='running')", n.ID, eventID)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		removed, err := res.RowsAffected()
		if removed != 0 || err != nil {
			s.mu.Unlock()
			return err
		}
	}
	target, ok := s.cfg.Targets[n.Target]
	if imReplyTarget(n.Target) || (n.Target == s.ownerTarget() && proactiveKey(n.Key)) {
		target = Target{Kind: "im"}
		ok = true
	}
	if !ok {
		_, e = s.db.ExecContext(ctx, "UPDATE outbox SET status='failed' WHERE id=?", n.ID)
		s.mu.Unlock()
		return e
	}
	// Approval cards bypass quiet hours; ordinary model-generated notifications cannot claim urgency.
	if !strings.HasPrefix(n.Key, "approval:") && target.Kind != "im" {
		due := s.quietUntil(now)
		if due.After(now) {
			_, e = s.db.ExecContext(ctx, "UPDATE outbox SET deliver_at=? WHERE id=?", due.UnixMilli(), n.ID)
			s.mu.Unlock()
			return e
		}
	}
	_, e = s.db.ExecContext(ctx, "UPDATE outbox SET status='sending' WHERE id=? AND status='pending'", n.ID)
	s.lastSend = now
	s.mu.Unlock()
	if e != nil {
		return e
	}
	status := "unknown"
	retries := n.Retries
	due := n.DeliverAt
	switch target.Kind {
	case "im":
		status = s.im.deliver(ctx, n)
		s.log.Info("IM delivery result", "provider", s.cfg.provider(), "outbox_id", n.ID, "target", n.Target, "status", status)
		if status == "offline" {
			status = "pending"
			due = time.Now().Add(time.Second * 5).UnixMilli()
		} else if status == "pending" {
			retries++
			due = time.Now().Add(s.retryDelay(retries)).UnixMilli()
			if retries >= 3 {
				status = "failed"
			}
		}
	case "local":
		n.Status = "sent"
		b, _ := json.MarshalIndent(n, "", "  ")
		// One immutable file per stable notification key: safe even if the process dies after rename.
		path := filepath.Join(s.root, "data/deliveries", stableID(n.Key)+".json")
		if old, err := os.ReadFile(path); err == nil {
			var prior Notification
			if json.Unmarshal(old, &prior) == nil && prior.Key == n.Key && prior.Content == n.Content && prior.Target == n.Target {
				status = "sent"
			}
		} else if errors.Is(err, os.ErrNotExist) {
			if atomicWrite(path, b, 0600) == nil {
				status = "sent"
			}
		}
	case "webhook":
		b, _ := json.Marshal(map[string]any{"id": n.ID, "key": n.Key, "target": n.Target, "content": n.Content})
		_, code, err := s.performHTTP(ctx, FrozenRequest{URL: target.URL, Method: "POST", Arguments: b}, target.Token, "notification-"+stableID(n.Key))
		if err == nil {
			if code >= 200 && code < 300 {
				status = "sent"
			} else if code == 429 {
				retries++
				status = "pending"
				due = time.Now().Add(s.retryDelay(retries)).UnixMilli()
				if retries >= 3 {
					status = "failed"
				}
			} else if definiteRejection(code) {
				status = "failed"
				retries++
			}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, e = s.db.ExecContext(saveCtx, "UPDATE outbox SET status=?,retries=?,deliver_at=? WHERE id=? AND status='sending'", status, retries, due, n.ID)
	return e
}
func (s *Service) outboxLoop(ctx context.Context) {
	tick := time.NewTicker(time.Duration(s.cfg.Limits.PollMS) * time.Millisecond)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.logErr("delivery", s.deliverOne(ctx))
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Service) ResolveDelivery(ctx context.Context, id int64, status string) error {
	if status != "sent" && status != "pending" && status != "failed" {
		return errors.New("resolution must be sent, pending (confirmed not delivered), or failed")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, e := s.db.ExecContext(ctx, "UPDATE outbox SET status=?,deliver_at=?,retries=0 WHERE id=? AND status IN ('unknown','failed')", status, time.Now().UnixMilli(), id)
	if e != nil {
		return e
	}
	n, _ := r.RowsAffected()
	if n != 1 {
		return ErrConflict
	}
	return nil
}
