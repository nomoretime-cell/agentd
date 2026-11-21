package agent

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if e := s.read.PingContext(ctx); e != nil {
			writeJSON(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key  string `json:"key"`
			Text string `json:"text"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		id, new, e := s.Append(r.Context(), req.Key, Payload{Kind: "message", Text: req.Text})
		respond(w, map[string]any{"id": id, "inserted": new}, e)
	})
	mux.HandleFunc("POST /v1/news/test", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key    string `json:"key"`
			Source string `json:"source"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		id, inserted, err := s.Append(r.Context(), req.Key, Payload{Kind: "news_test", Name: req.Source})
		respond(w, map[string]any{"id": id, "inserted": inserted, "outbox_key": fmt.Sprintf("ai-news:test:%d", id)}, err)
	})
	mux.HandleFunc("POST /v1/action-requests", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key       string          `json:"key"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Text      string          `json:"text"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		id, new, e := s.Append(r.Context(), req.Key, Payload{Kind: "action_request", Name: req.Name, Arguments: req.Arguments, Text: req.Text})
		respond(w, map[string]any{"id": id, "inserted": new}, e)
	})
	mux.HandleFunc("POST /v1/timers", func(w http.ResponseWriter, r *http.Request) {
		var t Timer
		if !decodeHTTP(w, r, &t) {
			return
		}
		respond(w, map[string]string{"id": t.ID}, s.UpsertTimer(r.Context(), t))
	})
	mux.HandleFunc("POST /v1/control", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Command string `json:"command"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		switch req.Command {
		case "pause", "stop":
			respond(w, map[string]bool{"paused": true}, s.SetPaused(true))
		case "resume":
			respond(w, map[string]bool{"paused": false}, s.SetPaused(false))
		default:
			respond(w, nil, errors.New("command must be pause, stop or resume"))
		}
	})
	mux.HandleFunc("POST /v1/events/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			respond(w, nil, e)
			return
		}
		var req struct {
			Operation string `json:"operation"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		respond(w, map[string]int64{"id": id}, s.ResolveEvent(r.Context(), id, req.Operation))
	})
	mux.HandleFunc("POST /v1/actions/{id}/decision", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Decision string `json:"decision"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		respond(w, map[string]string{"id": r.PathValue("id")}, s.Decide(r.Context(), r.PathValue("id"), req.Decision))
	})
	mux.HandleFunc("POST /v1/actions/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Status   string `json:"status"`
			Evidence string `json:"evidence"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		respond(w, map[string]string{"id": r.PathValue("id")}, s.ReconcileAction(r.Context(), r.PathValue("id"), req.Status, req.Evidence))
	})
	mux.HandleFunc("POST /v1/outbox/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		id, e := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if e != nil {
			respond(w, nil, e)
			return
		}
		var req struct {
			Status string `json:"status"`
		}
		if !decodeHTTP(w, r, &req) {
			return
		}
		respond(w, map[string]int64{"id": id}, s.ResolveDelivery(r.Context(), id, req.Status))
	})
	mux.HandleFunc("GET /v1/status", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		paused := s.paused
		active := s.activeCancel != nil
		wcConnected := s.wecomConn != nil
		dtConnected := s.dingTalkConnected
		s.mu.Unlock()
		counts := map[string]int{}
		for _, table := range []string{"inbox", "timers", "actions", "outbox"} {
			var n int
			if e := s.read.QueryRowContext(r.Context(), "SELECT count(*) FROM "+table).Scan(&n); e != nil {
				respond(w, nil, e)
				return
			}
			counts[table] = n
		}
		executor := "native"
		if s.cfg.OpenCodeBinary != "" {
			executor = "opencode"
		}
		respond(w, map[string]any{"executor": executor, "paused": paused, "active": active, "counts": counts, "model": s.cfg.Model.Name, "im_provider": s.cfg.provider(), "wecom": map[string]any{"enabled": s.cfg.provider() == "wecom", "connected": wcConnected, "discovery_only": s.cfg.WeCom.DiscoveryOnly, "allow_all_groups": s.cfg.WeCom.AllowAllGroups, "group_owner_tools": s.cfg.WeCom.GroupOwnerTools}, "dingtalk": map[string]any{"enabled": s.cfg.provider() == "dingtalk" && s.cfg.DingTalk.Enabled, "connected": dtConnected, "discovery_only": s.cfg.DingTalk.DiscoveryOnly}}, nil)
	})
	for _, table := range []string{"inbox", "timers", "actions", "outbox"} {
		mux.HandleFunc("GET /v1/"+table, func(w http.ResponseWriter, r *http.Request) {
			rows, e := s.list(r.Context(), table)
			respond(w, rows, e)
		})
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		if r.URL.Path != "/healthz" {
			auth := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(auth), []byte(s.token)) != 1 {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "valid bearer token required"})
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				writeJSON(w, http.StatusForbidden, map[string]string{"error": "browser cross-origin access disabled"})
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.Limits.MaxInputBytes+4096))
		mux.ServeHTTP(w, r)
	})
}
func decodeHTTP(w http.ResponseWriter, r *http.Request, v any) bool {
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON: " + e.Error()})
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		writeJSON(w, 400, map[string]string{"error": "expected exactly one JSON object"})
		return false
	}
	return true
}
func respond(w http.ResponseWriter, v any, e error) {
	if e == nil {
		writeJSON(w, 200, v)
		return
	}
	code := 400
	if errors.Is(e, ErrConflict) {
		code = 409
	} else if errors.Is(e, ErrDenied) {
		code = 403
	} else if errors.Is(e, ErrPaused) {
		code = 409
	} else if errors.Is(e, sql.ErrNoRows) {
		code = 404
	}
	writeJSON(w, code, map[string]string{"error": e.Error()})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func (s *Service) list(ctx context.Context, table string) ([]map[string]any, error) {
	allowed := map[string]bool{"inbox": true, "timers": true, "actions": true, "outbox": true}
	if !allowed[table] {
		return nil, ErrDenied
	}
	rows, e := s.read.QueryContext(ctx, "SELECT * FROM "+table+" ORDER BY rowid DESC LIMIT 200")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	columns, e := rows.Columns()
	if e != nil {
		return nil, e
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		dest := make([]any, len(columns))
		for i := range values {
			dest[i] = &values[i]
		}
		if e = rows.Scan(dest...); e != nil {
			return nil, e
		}
		item := map[string]any{}
		for i, k := range columns {
			v := values[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if str, ok := v.(string); ok {
				v = s.redact(str)
			}
			item[k] = v
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
