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
	if strings.HasPrefix(n.Target, "dingtalk:") {
		if _, e := s.dingTalkRoute(ctx, tx, n.Target); e != nil {
			return e
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
	_, e := tx.ExecContext(ctx, "INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES(?,?,?,?) ON CONFLICT(dedup_key) DO NOTHING", n.Key, n.Target, n.Content, n.DeliverAt)
	if e != nil {
		return e
	}
	var target, content string
	if e = tx.QueryRowContext(ctx, "SELECT target,content FROM outbox WHERE dedup_key=?", n.Key).Scan(&target, &content); e != nil {
		return e
	}
	if target != n.Target || content != n.Content {
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
	e := s.db.QueryRowContext(ctx, "SELECT id,dedup_key,target,content,deliver_at,status,retries FROM outbox WHERE status='pending' AND deliver_at<=? ORDER BY id LIMIT 1", now.UnixMilli()).Scan(&n.ID, &n.Key, &n.Target, &n.Content, &n.DeliverAt, &n.Status, &n.Retries)
	if errors.Is(e, sql.ErrNoRows) {
		s.mu.Unlock()
		return nil
	}
	if e != nil {
		s.mu.Unlock()
		return e
	}
	target, ok := s.cfg.Targets[n.Target]
	if strings.HasPrefix(n.Target, "dingtalk:") {
		target = Target{Kind: "dingtalk"}
		ok = true
	}
	if !ok {
		_, e = s.db.ExecContext(ctx, "UPDATE outbox SET status='failed' WHERE id=?", n.ID)
		s.mu.Unlock()
		return e
	}
	// Approval cards bypass quiet hours; ordinary model-generated notifications cannot claim urgency.
	if !strings.HasPrefix(n.Key, "approval:") && target.Kind != "dingtalk" {
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
	case "dingtalk":
		status = s.deliverDingTalk(ctx, n)
		s.log.Info("dingtalk delivery result", "outbox_id", n.ID, "target", n.Target, "status", status)
		if status == "pending" {
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
