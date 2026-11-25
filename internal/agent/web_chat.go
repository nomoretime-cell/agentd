package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
)

func validChatID(id string) bool {
	key, ok := strings.CutPrefix(id, "web-")
	return ok && len(key) == 48 && strings.Trim(key, "0123456789abcdef") == ""
}
func webChatScope(id string) runScope { return runScope{defaultRole, "owner", id} }

type chatSummary struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}
type chatPage[T any] struct {
	Items []T    `json:"items"`
	Next  string `json:"next"`
}
type chatEvent struct {
	ID           int64  `json:"id"`
	Kind         string `json:"kind"`
	Text         string `json:"text"`
	State        string `json:"state"`
	Retries      int    `json:"retries"`
	Reply        string `json:"reply"`
	ReplyID      string `json:"reply_id"`
	Action       string `json:"action"`
	ActionStatus string `json:"action_status"`
	Run          string `json:"run"`
	Error        string `json:"error"`
}

func chatCursor(raw string) (int64, error) {
	if raw == "" {
		return 1<<63 - 1, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 1 {
		return 0, errors.New("invalid chat cursor")
	}
	return n, nil
}
func (s *Service) webChats(ctx context.Context, before int64) (chatPage[chatSummary], error) {
	p := chatPage[chatSummary]{Items: []chatSummary{}}
	rows, err := s.read.QueryContext(ctx, `SELECT first.id,first.payload->>'web_chat',first.payload->>'text'
 FROM (SELECT min(id) AS id FROM inbox WHERE payload->>'web_chat' IS NOT NULL GROUP BY payload->>'web_chat' HAVING min(id)<? ORDER BY min(id) DESC LIMIT 31) roots JOIN inbox first ON first.id=roots.id ORDER BY first.id DESC`, before)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	var last int64
	for rows.Next() {
		if len(p.Items) == 30 {
			p.Next = strconv.FormatInt(last, 10)
			break
		}
		var c chatSummary
		if err = rows.Scan(&last, &c.ID, &c.Title); err != nil {
			return p, err
		}
		c.Title, _ = clipEvidence(s.redact(c.Title), 120)
		p.Items = append(p.Items, c)
	}
	return p, rows.Err()
}

func (s *Service) webChatEvents(ctx context.Context, chat string, before int64, limit int) (chatPage[chatEvent], error) {
	p := chatPage[chatEvent]{Items: []chatEvent{}}
	if !validChatID(chat) {
		return p, sql.ErrNoRows
	}
	// A batch's last input points to its first, which owns the reply and run ledger.
	// Read only the visible event window, even when a conversation spans many pages.
	rows, err := s.read.QueryContext(ctx, `WITH page AS MATERIALIZED (
   SELECT id,payload,status,retries,chat_head FROM inbox WHERE payload->>'web_chat'=? AND id<? ORDER BY id DESC LIMIT ?
 ), turns AS MATERIALIZED (
   SELECT i.*,h.replies, (SELECT j.value FROM json_each(h.run_index) j WHERE j.type='object' ORDER BY j.value->>'at' DESC,j.id DESC LIMIT 1) AS run
   FROM page i LEFT JOIN inbox h ON h.id=i.chat_head
 )
 SELECT t.id,t.payload->>'kind',coalesce(t.payload->>'text',''),t.status,t.retries,
 coalesce(t.replies,'[]'),coalesce(t.chat_head,0),coalesce(t.run->>'attempt',''),coalesce(t.run->>'error',''),
 coalesce(a.id,''),coalesce(a.status,'')
 FROM turns t LEFT JOIN actions a ON a.id=coalesce(t.run->>'pending_action',t.payload->>'action_id') ORDER BY t.id DESC`, chat, before, limit+1)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(p.Items) == limit {
			p.Next = strconv.FormatInt(p.Items[limit-1].ID, 10)
			break
		}
		var e chatEvent
		var raw, attempt string
		var head int64
		if err = rows.Scan(&e.ID, &e.Kind, &e.Text, &e.State, &e.Retries, &raw, &head, &attempt, &e.Error, &e.Action, &e.ActionStatus); err != nil {
			return p, err
		}
		if attempt != "" {
			e.Run = durableRunID(head, attempt)
		}
		var replies []formalReply
		if err = json.Unmarshal([]byte(raw), &replies); err != nil {
			return p, err
		}
		for _, r := range replies {
			if r.Conversation == chat {
				e.ReplyID, e.Reply = r.ID, s.redact(r.Body)
				break
			}
		}
		e.Text, e.Error = s.redact(e.Text), s.redact(e.Error)
		p.Items = append(p.Items, e)
	}
	return p, rows.Err()
}

func (s *Service) webChatHistory(ctx context.Context, chat string, before int64) ([]Message, error) {
	limit := s.cfg.Limits.MaxHistoryMessages
	if limit == 0 {
		return nil, nil
	}
	p, err := s.webChatEvents(ctx, chat, before, limit)
	if err != nil {
		return nil, err
	}
	// Collect the newest committed turns first, then restore conversational order.
	var history []Message
	bytes := 0
	for _, e := range p.Items {
		if e.State != "done" {
			continue
		}
		var turns []Message
		if e.ReplyID != "" {
			turns = append(turns, Message{Role: "assistant", Content: s.bounded(e.Reply), historyID: e.ReplyID})
		}
		if e.Kind == "message" {
			turns = append(turns, Message{Role: "user", Content: s.bounded(e.Text), historyID: fmt.Sprintf("inbox/%d", e.ID)})
		}
		// Keep an event's reply and input together so history never starts
		// with an answer whose question was cut off.
		size := 0
		for _, m := range turns {
			size += len(m.Content)
		}
		if len(history)+len(turns) > limit || bytes+size > 48<<10 {
			break
		}
		bytes += size
		history = append(history, turns...)
	}
	slices.Reverse(history)
	return history, nil
}
func (s *Service) registerWebChats(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/chats", func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !decodeHTTP(w, r, &req) {
			return
		}
		respond(w, chatSummary{ID: "web-" + randomID()}, nil)
	})
	mux.HandleFunc("GET /v1/chats", func(w http.ResponseWriter, r *http.Request) {
		before, err := chatCursor(r.URL.Query().Get("cursor"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		page, err := s.webChats(r.Context(), before)
		respond(w, page, err)
	})
	mux.HandleFunc("GET /v1/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		before, err := chatCursor(r.URL.Query().Get("cursor"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		page, err := s.webChatEvents(r.Context(), r.PathValue("id"), before, 30)
		respond(w, page, err)
	})

	mux.HandleFunc("GET /v1/chats/{id}/submissions/{key}", func(w http.ResponseWriter, r *http.Request) {
		id, key := r.PathValue("id"), r.PathValue("key")
		if !validChatID(id) || len(key) > 256 {
			respond(w, nil, sql.ErrNoRows)
			return
		}
		var event int64
		err := s.read.QueryRowContext(r.Context(), "SELECT id FROM inbox WHERE dedup_key=?", "chat:"+id+":"+key).Scan(&event)
		if errors.Is(err, sql.ErrNoRows) {
			err = nil
		}
		respond(w, map[string]int64{"id": event}, err)
	})
	mux.HandleFunc("POST /v1/chats/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validChatID(id) {
			respond(w, nil, sql.ErrNoRows)
			return
		}
		var req struct {
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		event, inserted, err := s.Append(r.Context(), req.Key, Payload{Kind: "message", WebChat: id, Text: req.Text})
		respond(w, map[string]any{"id": event, "inserted": inserted}, err)
	})
}
