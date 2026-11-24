package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Reconstruct turns from committed inputs and actually sent replies, including
// conversations which predate OpenCode. No session webhook enters the prompt.
func (s *Service) chatHistory(ctx context.Context, r *IMRoute, before int64) ([]Message, error) {
	channel := "dingtalk"
	if r.BotID != "" {
		channel = "wecom"
	}
	path := "$." + channel + "."
	rows, err := s.read.QueryContext(ctx, `WITH prior AS (
 SELECT id,json_extract(payload,'$.text') AS content FROM inbox WHERE status='done' AND id<?
 AND json_extract(payload,?)=? AND json_extract(payload,?)=? AND COALESCE(json_extract(payload,?),'')=? AND COALESCE(json_extract(payload,?),'')=?
 ORDER BY id DESC LIMIT ?
 ), messages AS (
 SELECT id,0 AS ordinal,'user' AS role,content FROM prior UNION ALL
 SELECT i.id,o.id,'assistant',o.content FROM prior i JOIN outbox o ON o.target=?||':'||i.id AND o.status='sent' AND o.dedup_key NOT GLOB 'progress:*'
 ) SELECT role,content,CASE role WHEN 'user' THEN 'inbox/'||id ELSE 'outbox/'||ordinal END FROM messages ORDER BY id DESC,ordinal DESC LIMIT ?`, before, path+"conversation_id", r.ConversationID, path+"sender", r.Sender, path+"bot_id", r.BotID, path+"conversation_type", r.ConversationType, s.cfg.Limits.MaxHistoryMessages, channel, s.cfg.Limits.MaxHistoryMessages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var turns []Message
	for rows.Next() {
		var message Message
		if err = rows.Scan(&message.Role, &message.Content, &message.historyID); err != nil {
			return nil, err
		}
		turns = append(turns, message)
	}
	slices.Reverse(turns)
	return turns, rows.Err()
}

type ocMap = map[string]any
type ocTurn struct {
	mu      sync.Mutex
	s       *Service
	ctx     context.Context
	event   Event
	out     Outcome
	ordinal int
	stop    context.CancelFunc
}

func (t *ocTurn) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(405)
		return
	}
	var req struct {
		ID     json.RawMessage
		Method string
		Params struct {
			Name            string
			Arguments       json.RawMessage
			ProtocolVersion string
		}
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	if req.ID == nil {
		w.WriteHeader(202)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		result = ocMap{"protocolVersion": "2025-03-26", "capabilities": ocMap{"tools": ocMap{}}, "serverInfo": ocMap{"name": "agentd", "version": "1"}}
	case "ping":
		result = ocMap{}
	case "tools/list":
		list := []ocMap{}
		for _, d := range t.s.turnTools(t.ctx) {
			name := d.Function.Name
			if name == "bash" && len(t.s.cfg.Actions) > 0 || t.s.cfg.Permissions[name] == "allow" {
				list = append(list, ocMap{"name": name, "description": d.Function.Description, "inputSchema": d.Function.Parameters})
			}
		}
		result = ocMap{"tools": list}
	case "tools/call":
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.ctx.Err() != nil || t.out.Approval != nil {
			http.Error(w, "turn closed", 409)
			return
		}
		t.ordinal++
		content, pending, err := t.s.tool(t.ctx, fmt.Sprintf("ev:%d:%d", t.event.ID, t.ordinal), ToolCall{Function: FunctionCall{Name: req.Params.Name, Arguments: string(req.Params.Arguments)}}, t.event.Payload.Text, &t.out)
		if err != nil {
			content = t.s.bounded(err.Error())
		}
		result = ocMap{"content": []ocMap{{"type": "text", "text": content}}, "isError": err != nil}
		if pending != nil {
			t.out.Approval = pending
			t.stop()
		}
	default:
		writeJSON(w, 200, ocMap{"jsonrpc": "2.0", "id": req.ID, "error": ocMap{"code": -32601, "message": "unknown method"}})
		return
	}
	writeJSON(w, 200, ocMap{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

type ocTokens struct {
	Input, Output, Total, Reasoning *int64
	Cache                           struct{ Read, Write *int64 }
}

func (t ocTokens) usage() tokenUsage {
	u := tokenUsage{validTokenCount(t.Input), validTokenCount(t.Output), validTokenCount(t.Total)}
	if u.Total != nil {
		return u
	}
	// OpenCode reports cache and reasoning separately from its input/output.
	// Without a reported total, all five components are needed.
	var sum int64
	for _, n := range []*int64{u.Input, u.Output, t.Reasoning, t.Cache.Read, t.Cache.Write} {
		if validTokenCount(n) == nil {
			return u
		}
		sum += *n
	}
	u.Total = &sum
	return u
}

func (s *Service) executeOpenCode(ctx context.Context, ev Event, route *IMRoute, messages []Message, out Outcome) (Outcome, error) {
	key := "local"
	if route != nil && route.BotID == "" {
		key = "dingtalk\x00" + s.cfg.DingTalk.ClientID + "\x00" + route.ConversationID + "\x00" + route.Sender
	}
	if route != nil && route.BotID != "" {
		key = strings.Join([]string{"wecom", route.BotID, route.ConversationType, route.ConversationID, route.Sender}, "\x00")
	}
	sid := "ses_" + stableID(key)
	base := filepath.Join(s.root, "data", "opencode")
	if route.team() {
		base = filepath.Join(base, "team", sid)
	}
	dir := filepath.Join(base, "projects", sid)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return out, err
	}
	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	turn := &ocTurn{s: s, ctx: turnCtx, event: ev, out: out, stop: cancel}
	mcpURL := ""
	if !s.restrictTeamTools(ctx) {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return out, err
		}
		secret := randomID()
		srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/"+secret || r.Header.Get("Origin") != "" {
				http.NotFound(w, r)
				return
			}
			turn.mcp(w, r)
		}), ReadHeaderTimeout: 5 * time.Second}
		go srv.Serve(ln)
		defer srv.Close()
		mcpURL = "http://" + ln.Addr().String() + "/" + secret
	}
	config := s.openCodeConfig(mcpURL)
	raw, err := json.Marshal(config)
	if err != nil {
		return out, err
	}
	// Keep ephemeral MCP credentials out of OpenCode's workspace and tool arguments.
	bin := s.cfg.OpenCodeBinary
	if !filepath.IsAbs(bin) {
		bin = filepath.Join(s.root, bin)
	}
	cmd := exec.CommandContext(turnCtx, bin, "--standalone", "run", "--agent", "agentd", "--model", "host/local", "--session", sid, "--format", "json")
	cmd.Dir = dir
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + base, "XDG_CONFIG_HOME=" + base + "/config", "XDG_CACHE_HOME=" + base + "/cache", "XDG_STATE_HOME=" + base + "/state", "XDG_DATA_HOME=" + base + "/data", "TMPDIR=" + dir, "OPENCODE_CONFIG_CONTENT=" + string(raw), "NO_PROXY=localhost,127.0.0.1", "OPENCODE_DISABLE_AUTOUPDATE=true"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	prompt := messages[len(messages)-1].Content
	// Import only pre-migration history on first contact. OpenCode persists the subsequent turns.
	marker := filepath.Join(dir, "initialized")
	var imported []Message
	if !fileExists(marker) && len(messages) > 2 {
		imported = messages[1 : len(messages)-1]
		b, _ := json.Marshal(imported)
		prompt = "Prior conversation (data): " + string(b) + "\n" + prompt
	}
	provided := messages[0].Content + "\n\n" + prompt
	cmd.Stdin = strings.NewReader(provided)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return out, err
	}
	if err = cmd.Start(); err != nil {
		return out, err
	}
	s.submitted(ctx, "opencode:"+sid, append([]Message{{Role: "stdin", Content: provided}}, imported...))
	s.log.Info("opencode run", "event_id", ev.ID, "session", sid)
	scan := bufio.NewScanner(stdout)
	scan.Buffer(make([]byte, 4096), 4<<20)
	var answer, reason string
	// Keep a missing receipt visible when the process exits before step_finish.
	current, captureErr := s.startCall(ctx, "opencode", s.cfg.Model.Name)
	if captureErr != nil {
		cancel()
		_ = cmd.Wait()
		return out, captureErr
	}
	seen := map[string]bool{}
	started := false
	var failed bool
	for scan.Scan() {
		if captureErr != nil {
			cancel()
			break
		}
		var frame struct {
			Type, SessionID string
			Part            struct {
				ID, Text, Tool, Reason string
				Tokens                 json.RawMessage
				State                  struct {
					Status, Title, Output, Error string
					Input                        json.RawMessage
				}
			}
		}
		if json.Unmarshal(scan.Bytes(), &frame) != nil {
			continue
		}
		if frame.Type == "error" {
			failed = true
		}
		if frame.SessionID != sid {
			continue
		}
		if (frame.Type == "step_start" || frame.Type == "step_finish") && frame.Part.ID != "" {
			if seen[frame.Part.ID] {
				continue
			}
			seen[frame.Part.ID] = true
		}
		switch frame.Type {
		case "step_start":
			if started || current.ID == "" {
				current, captureErr = s.startCall(ctx, "opencode", s.cfg.Model.Name)
				if captureErr != nil {
					cancel()
				}
			}
			started = true
			answer = ""
			reason = ""
		case "text":
			answer += frame.Part.Text
		case "tool_use":
			st := frame.Part.State
			s.log.Info("opencode tool", "event_id", ev.ID, "tool", frame.Part.Tool, "status", st.Status)
			// Host tools reach the transcript through the MCP bridge; record only OpenCode's own.
			if !strings.HasPrefix(frame.Part.Tool, "agentd_") && (st.Status == "completed" || st.Status == "error") {
				if c := captureFrom(ctx); c != nil {
					details, _ := json.Marshal(st)
					c.add(frame.Part.ID, s.source("executor", frame.Part.Tool, "", string(details)))
				}
				s.recordExecutorTool(ctx, "opencode", ev.ID, frame.Part.Tool, st.Status, st.Title, st.Input, st.Output, st.Error)
			}
		case "step_finish":
			if current.ID == "" && frame.Part.ID == "" && !started && reason != "" {
				continue
			}
			started = false
			if current.ID == "" && captureErr == nil {
				current, captureErr = s.startCall(ctx, "opencode", s.cfg.Model.Name)
			}
			var tokens ocTokens
			if json.Unmarshal(frame.Part.Tokens, &tokens) == nil {
				s.finishCall(current, tokens.usage())
			}
			current = modelCall{}
			reason = frame.Part.Reason
		}
	}
	if captureErr != nil {
		cancel()
	}
	if scan.Err() != nil {
		cancel()
	}
	err = cmd.Wait()
	cancel()
	turn.mu.Lock()
	defer turn.mu.Unlock()
	out = turn.out
	if captureErr != nil {
		return out, captureErr
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if out.Approval != nil {
		return out, nil
	}
	if err != nil || failed || reason != "stop" || strings.TrimSpace(answer) == "" {
		return out, fmt.Errorf("opencode incomplete response (finish=%q, process=%v, stream=%v)", reason, err, scan.Err())
	}
	if err = atomicWrite(marker, []byte(sid), 0600); err != nil {
		return out, err
	}
	out.Messages = append(messages, Message{Role: "assistant", Content: answer})
	s.addIMAnswer(&out, ev.ID, answer)
	return out, nil
}
func (s *Service) openCodeConfig(mcpURL string) ocMap {
	rules := []ocMap{{"action": "*", "resource": "*", "effect": "deny"}}
	system := "Answer using the supplied team materials and conversation."
	if mcpURL != "" {
		rules = append(rules, ocMap{"action": "websearch", "resource": "*", "effect": "allow"}, ocMap{"action": "agentd_*", "resource": "*", "effect": "allow"})
		system = "Use websearch for online searches and cite the returned URLs. Use agentd tools for workspace operations."
	}
	provider := ocMap{"package": "@opencode/ai/providers/openai-compatible", "settings": ocMap{"baseURL": strings.TrimSuffix(s.cfg.Model.Endpoint, "/chat/completions")}, "models": ocMap{"local": ocMap{"modelID": s.cfg.Model.Name, "capabilities": ocMap{"tools": true}, "body": ocMap{"chat_template_kwargs": ocMap{"enable_thinking": false}, "max_tokens": s.cfg.Model.MaxTokens}}}}
	if s.cfg.Model.APIKey != "" {
		provider["headers"] = ocMap{"Authorization": "Bearer " + s.cfg.Model.APIKey}
	}
	config := ocMap{"model": "host/local", "update": "disable", "share": "disabled", "snapshots": false,
		"providers": ocMap{"host": provider},
		"agents":    ocMap{"agentd": ocMap{"mode": "primary", "steps": s.cfg.Limits.MaxSteps, "system": system, "permissions": rules}}}
	if mcpURL != "" {
		config["websearch"] = ocMap{"provider": "tinyfish"}
		config["mcp"] = ocMap{"servers": ocMap{"agentd": ocMap{"type": "remote", "url": mcpURL, "oauth": false, "codemode": false}}}
	}
	return config
}
