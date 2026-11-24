package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"slices"
	"time"
)

type dreamNameKey struct{}

const dreamPrompt = "Read skills/memory-consolidation/SKILL.md and follow it to consolidate memory and draft skills from the available evidence."

func (s *Service) dreamModel(id string) (ModelConfig, bool) {
	if id == "default" {
		return s.cfg.StrongModel.ModelConfig, s.strong != nil
	}
	m, ok := s.cfg.DreamModels[id]
	return m, ok
}
func modelVersion(c ModelConfig) string { return stableID(c.Endpoint + "\x00" + c.Name) }

type dreamOption struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Host    string `json:"host"`
	Version string `json:"version"`
}

func (s *Service) dreamOptions() []dreamOption {
	ids := []string{"default"}
	for id := range s.cfg.DreamModels {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	options := []dreamOption{}
	for _, id := range ids {
		if c, ok := s.dreamModel(id); ok {
			u, _ := url.Parse(c.Endpoint)
			options = append(options, dreamOption{id, s.redact(c.Name), u.Hostname(), modelVersion(c)})
		}
	}
	return options
}
func (s *Service) queueDream(ctx context.Context, key, profile, version string) (int64, error) {
	if !batchKey.MatchString(key) {
		return 0, errors.New("整理请求编号无效")
	}
	m, ok := s.dreamModel(profile)
	if !ok || modelVersion(m) != version {
		return 0, ErrConflict
	}
	payload := Payload{Kind: kindConsolidation, Text: dreamPrompt, DreamProfile: profile, DreamVersion: version}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memoryMu.Lock()
	held := fileExists(filepath.Join(s.root, heldDir)) && s.checkBase(heldDir) == nil
	s.memoryMu.Unlock()
	var id int64
	err := transaction(ctx, s.db, func(tx *sql.Tx) error {
		var exists bool
		if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM inbox WHERE dedup_key=?)", "dream:"+key).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			if s.paused {
				return ErrPaused
			}
			var busy bool
			if e := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM inbox WHERE status IN ('pending','running') AND payload->>'kind'=?)", kindConsolidation).Scan(&busy); e != nil {
				return e
			}
			if busy {
				return fmt.Errorf("%w: 整理已在队列中", ErrConflict)
			}
			if held {
				return fmt.Errorf("%w: 请先决定待审候选", ErrConflict)
			}
		}
		var e error
		id, _, e = insertEvent(ctx, tx, "dream:"+key, payload, time.Now().UnixMilli())
		return e
	})
	if err == nil {
		s.notify()
	}
	return id, err
}

type dreamRun struct {
	Event   int64  `json:"event"`
	Profile string `json:"profile"`
	Trigger string `json:"trigger"`
	State   string `json:"state"`
	Run     string `json:"run"`
	At      string `json:"ts"`
	Error   string `json:"error"`
}

func (s *Service) dreamRuns(ctx context.Context) ([]dreamRun, error) {
	rows, e := s.read.QueryContext(ctx, `SELECT id,coalesce(payload->>'dream_profile',''),status, available_at, coalesce((SELECT json_object('key',j.key,'item',json(j.value)) FROM json_each(i.run_index) j WHERE j.type='object' ORDER BY j.value->>'at' DESC LIMIT 1),'{}') FROM inbox i WHERE payload->>'kind'=? ORDER BY id DESC LIMIT 50`, kindConsolidation)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	runs := []dreamRun{}
	for rows.Next() {
		var r dreamRun
		var at int64
		var raw string
		if e = rows.Scan(&r.Event, &r.Profile, &r.State, &at, &raw); e != nil {
			return nil, e
		}
		r.Trigger = "手动"
		if r.Profile == "" {
			r.Profile = "default"
			r.Trigger = "定时"
		}
		var rec struct {
			Key  string
			Item struct{ Status, Error, TS string }
		}
		if e = json.Unmarshal([]byte(raw), &rec); e != nil {
			return nil, e
		}
		r.At = time.UnixMilli(at).UTC().Format(time.RFC3339)
		if rec.Key != "" {
			r.Run = fmt.Sprintf("%d~%s", r.Event, rec.Key)
			r.Error = s.redact(rec.Item.Error)
			r.At = rec.Item.TS
			if r.State == "done" {
				r.State = rec.Item.Status
			}
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}
