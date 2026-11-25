package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type messageTrace struct {
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Target       string   `json:"target"`
	Body         string   `json:"body"`
	Status       string   `json:"status"`
	ConfirmedBy  string   `json:"confirmed_by"`
	Event        int64    `json:"event"`
	Run          string   `json:"run"`
	Conversation string   `json:"conversation"`
	Key          string   `json:"key"`
	Runs         []string `json:"related_runs"`
	EventDay     string   `json:"event_day"`
	At           int64    `json:"at"`
}

func noticeKind(key string) string {
	if _, test, ok := newsNotice(key); ok {
		if test {
			return "news-test"
		}
		return "news"
	}
	prefix, _, _ := strings.Cut(key, ":")
	switch prefix {
	case "progress", "approval", "failure", "action-result", "reply", "message", "drafts", "memory-held":
		return prefix
	default:
		return "notification"
	}
}
func (s *Service) messageDetail(ctx context.Context, id string) (messageTrace, error) {
	var m messageTrace
	m.ID = id
	var attempt string
	if raw, ok := strings.CutPrefix(id, "delivery:"); ok {
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n < 1 {
			return m, sql.ErrNoRows
		}
		err = s.read.QueryRowContext(ctx, `SELECT dedup_key,target,content,status,coalesce(confirmed_by,''),coalesce(origin_event,0),coalesce(origin_attempt,''),coalesce(sent_at,0) FROM outbox WHERE id=?`, n).Scan(&m.Key, &m.Target, &m.Body, &m.Status, &m.ConfirmedBy, &m.Event, &attempt, &m.At)
		if err != nil {
			return m, err
		}
		m.Kind = noticeKind(m.Key)
		if m.Event == 0 {
			m.Event = s.legacyNoticeEvent(ctx, m.Key)
		}
	} else {
		var raw string
		parts := strings.Split(id, ":")
		switch {
		case len(parts) == 2 && (parts[0] == "local" || parts[0] == "reply"):
			raw = parts[1]
		case len(parts) == 4 && parts[0] == "message" && parts[1] == "ev":
			raw = parts[2]
		default:
			return m, sql.ErrNoRows
		}
		event, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || event < 1 {
			return m, sql.ErrNoRows
		}
		err = s.read.QueryRowContext(ctx, `SELECT i.id,i.reply_at,j.value->>'attempt',j.value->>'conversation',coalesce(o.content,j.value->>'body'),coalesce(o.target,i.payload->>'web_chat','local'),coalesce(o.status,'committed'),coalesce(o.confirmed_by,'') FROM inbox i JOIN json_each(i.replies) j LEFT JOIN outbox o ON o.dedup_key=j.value->>'delivery_key' WHERE i.id=? AND j.value->>'id'=?`, event, id).Scan(&m.Event, &m.At, &attempt, &m.Conversation, &m.Body, &m.Target, &m.Status, &m.ConfirmedBy)
		if err != nil {
			return m, err
		}
		m.Key = id
		m.Kind = noticeKind(id)
		if parts[0] == "local" {
			m.Kind = "reply"
		}
	}
	if m.Event > 0 {
		// A batched attempt is stored on its first input. A progress notice only
		// identifies an event, so link its timeline rather than guessing an attempt.
		if attempt != "" {
			var first int64
			var conversation string
			err := s.read.QueryRowContext(ctx, `SELECT id,json_extract(run_index,?||'.conversation') FROM inbox WHERE id<=? AND run_index IS NOT NULL AND json_type(run_index,?)='object' ORDER BY id DESC LIMIT 1`, "$."+attempt, m.Event, "$."+attempt).Scan(&first, &conversation)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return m, err
			}
			if err == nil {
				m.Run = durableRunID(first, attempt)
				m.Conversation = conversation
			}
		}
		if m.Run == "" {
			var at int64
			if err := s.read.QueryRowContext(ctx, "SELECT available_at FROM inbox WHERE id=?", m.Event).Scan(&at); err == nil {
				loc, err := time.LoadLocation(s.cfg.Timezone)
				if err != nil {
					return m, err
				}
				m.EventDay = time.UnixMilli(at).In(loc).Format(time.DateOnly)
			} else if !errors.Is(err, sql.ErrNoRows) {
				return m, err
			}
			rows, err := s.read.QueryContext(ctx, `SELECT i.id,j.key FROM inbox i JOIN json_each(i.run_index) j WHERE i.run_index IS NOT NULL AND i.id<=? AND EXISTS (SELECT 1 FROM json_each(j.value,'$.inputs') e WHERE e.value->>'event'=?) ORDER BY i.id DESC,j.value->>'at' DESC LIMIT 20`, m.Event, m.Event)
			if err != nil {
				return m, err
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var key string
				if err = rows.Scan(&id, &key); err != nil {
					return m, err
				}
				m.Runs = append(m.Runs, durableRunID(id, key))
			}
			if err = rows.Err(); err != nil {
				return m, err
			}
		}
	}
	m.Key = s.redact(m.Key)
	m.Body, m.Target, m.Conversation = s.redact(m.Body), s.redact(m.Target), s.redact(m.Conversation)
	return m, nil
}
func (s *Service) legacyNoticeEvent(ctx context.Context, key string) int64 {
	if ev, _, ok := newsNotice(key); ok {
		n, _ := strconv.ParseInt(ev, 10, 64)
		return n
	}
	p := strings.Split(key, ":")
	if len(p) < 2 {
		return 0
	}
	var raw string
	switch p[0] {
	case "reply", "progress", "failure", "drafts", "memory-held":
		raw = p[1]
	case "message":
		if len(p) > 2 && p[1] == "ev" {
			raw = p[2]
		}
	case "approval", "action-result":
		if p[0] == "action-result" {
			var event int64
			if s.read.QueryRowContext(ctx, `SELECT min(id) FROM inbox WHERE payload->>'action_id'=? AND payload->>'kind'='approval' HAVING count(*)=1`, p[1]).Scan(&event) == nil {
				return event
			}
			return 0
		}
		var source string
		if s.read.QueryRowContext(ctx, "SELECT dedup_key FROM actions WHERE id=?", p[1]).Scan(&source) == nil {
			fields := strings.Split(source, ":")
			if len(fields) > 2 && fields[0] == "ev" {
				raw = fields[1]
			}
		}
	}
	n, _ := strconv.ParseInt(raw, 10, 64)
	return n
}

type noticePage struct {
	Items []messageTrace `json:"items"`
	Next  string         `json:"next"`
}

func (s *Service) noticeHistory(ctx context.Context, q url.Values) (noticePage, error) {
	p := noticePage{Items: []messageTrace{}}
	before := int64(1<<63 - 1)
	if text := q.Get("cursor"); text != "" {
		var e error
		before, e = strconv.ParseInt(text, 10, 64)
		if e != nil || before < 1 {
			return p, errors.New("invalid cursor")
		}
	}
	rows, err := s.read.QueryContext(ctx, `SELECT id,dedup_key,target,content,status FROM outbox WHERE id<? ORDER BY id DESC LIMIT 21`, before)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(p.Items) == 20 {
			p.Next = strings.TrimPrefix(p.Items[19].ID, "delivery:")
			break
		}
		var id int64
		var m messageTrace
		if err = rows.Scan(&id, &m.Key, &m.Target, &m.Body, &m.Status); err != nil {
			return p, err
		}
		m.ID = "delivery:" + strconv.FormatInt(id, 10)
		m.Body, _ = clipEvidence(s.redact(m.Body), 256)
		m.Target = s.redact(m.Target)
		m.Kind = noticeKind(m.Key)
		m.Key = s.redact(m.Key)
		p.Items = append(p.Items, m)
	}
	return p, rows.Err()
}

type conversationSummary struct {
	runScope
	Latest string `json:"latest"`
	Count  int    `json:"count"`
	Active bool   `json:"active"`
}
type conversationPage struct {
	Items      []conversationSummary `json:"items"`
	Next       string                `json:"next"`
	From       string                `json:"from"`
	To         string                `json:"to"`
	Incomplete bool                  `json:"incomplete"`
}

func (s *Service) conversations(ctx context.Context, q url.Values) (conversationPage, error) {
	runs, err := s.runHistory(ctx, q)
	p := conversationPage{[]conversationSummary{}, runs.Next, runs.From, runs.To, runs.Incomplete}
	seen := map[runScope]int{}
	for _, r := range runs.Items {
		i, ok := seen[r.runScope]
		if !ok {
			i = len(p.Items)
			seen[r.runScope] = i
			p.Items = append(p.Items, conversationSummary{runScope: r.runScope, Latest: r.Time})
		}
		p.Items[i].Count++
		p.Items[i].Active = p.Items[i].Active || r.Active
	}
	return p, err
}

// Attach the committed reply references separately from the clipped transcript.
func (s *Service) runReplies(ctx context.Context, rec *historyRecord) error {
	if len(rec.Inputs) == 0 || rec.Attempt == "" {
		return nil
	}
	var raw sql.NullString
	err := s.read.QueryRowContext(ctx, "SELECT replies FROM inbox WHERE id=?", rec.Inputs[0].Event).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) || !raw.Valid && err == nil {
		return nil
	}
	if err != nil {
		return err
	}
	var replies []formalReply
	if err = json.Unmarshal([]byte(raw.String), &replies); err != nil {
		return err
	}
	for _, reply := range replies {
		if reply.Attempt == rec.Attempt {
			rec.ReplyIDs = append(rec.ReplyIDs, reply.ID)
			if strings.HasPrefix(reply.ID, "local:") {
				rec.Answer = s.redact(reply.Body)
			}
		}
	}
	return nil
}
