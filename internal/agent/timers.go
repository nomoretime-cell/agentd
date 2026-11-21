package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
)

func (s *Service) normalizeTimer(t Timer) (Timer, error) {
	if t.ID == "" || len(t.ID) > 128 {
		return t, errors.New("timer id must contain 1..128 bytes")
	}
	if e := checkSize(t.Prompt, s.cfg.Limits.MaxInputBytes); e != nil {
		return t, e
	}
	if t.Prompt == "" {
		return t, errors.New("timer prompt is required")
	}
	if t.Cron != "" {
		schedule, e := s.schedule(t.Cron)
		if e != nil {
			return t, e
		}
		if t.FireAt == 0 {
			t.FireAt = schedule.Next(time.Now()).UnixMilli()
		}
	}
	if t.FireAt == 0 {
		return t, errors.New("fire_at is required for a one-shot timer")
	}
	return t, nil
}
func (s *Service) schedule(spec string) (cron.Schedule, error) {
	loc, e := time.LoadLocation(s.cfg.Timezone)
	if e != nil {
		return nil, e
	}
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	parsed, e := parser.Parse("CRON_TZ=" + loc.String() + " " + spec)
	return parsed, e
}
func upsertTimerTx(ctx context.Context, tx *sql.Tx, t Timer) error {
	var cronExpr any
	if t.Cron != "" {
		cronExpr = t.Cron
	}
	_, e := tx.ExecContext(ctx, "INSERT INTO timers(id,cron_expr,fire_at,prompt,active) VALUES(?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET cron_expr=excluded.cron_expr,fire_at=excluded.fire_at,prompt=excluded.prompt,active=excluded.active", t.ID, cronExpr, t.FireAt, t.Prompt, t.Active)
	return e
}
func (s *Service) UpsertTimer(ctx context.Context, t Timer) error {
	t, e := s.normalizeTimer(t)
	if e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return transaction(ctx, s.db, func(tx *sql.Tx) error { return upsertTimerTx(ctx, tx, t) })
}
func (s *Service) fireTimers(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.paused {
		return nil
	}
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		now := time.Now()
		rows, e := tx.QueryContext(ctx, "SELECT id,COALESCE(cron_expr,''),fire_at,prompt,active FROM timers WHERE active=1 AND fire_at<=? ORDER BY fire_at LIMIT 100", now.UnixMilli())
		if e != nil {
			return e
		}
		var list []Timer
		for rows.Next() {
			var t Timer
			if e = rows.Scan(&t.ID, &t.Cron, &t.FireAt, &t.Prompt, &t.Active); e != nil {
				rows.Close()
				return e
			}
			list = append(list, t)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, t := range list {
			payload := Payload{Kind: "timer", Text: t.Prompt}
			if t.ID == newsTimerID {
				payload.Kind = "ai_news"
				payload.NewsTarget = s.newsScope()
			}
			_, _, e = insertEvent(ctx, tx, fmt.Sprintf("timer:%s:%d", t.ID, t.FireAt), payload, now.UnixMilli())
			if e != nil {
				return e
			}
			if t.Cron == "" {
				t.Active = false
			} else {
				sc, e := s.schedule(t.Cron)
				if e != nil {
					return e
				}
				t.FireAt = sc.Next(now).UnixMilli()
			} // Missed occurrences coalesce into one wakeup.
			if e = upsertTimerTx(ctx, tx, t); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Service) timerLoop(ctx context.Context) {
	tick := time.NewTicker(time.Duration(s.cfg.Limits.PollMS) * time.Millisecond)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.logErr("timers", s.fireTimers(ctx))
		s.logErr("approval expiry", s.expireActions(ctx))
		s.notify()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
