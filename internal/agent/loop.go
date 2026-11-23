package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Outcome struct {
	ReplyTarget   string
	Notifications []Notification
	Timers        []Timer
	Approval      *Action
	Messages      []Message
}

func timeNow() string { return time.Now().Format(time.RFC3339) }

func (s *Service) execute(ctx context.Context, batch []Event) (Outcome, error) {
	var out Outcome
	if out, ok, err := s.runBusiness(ctx, batch[0]); ok {
		return out, err
	}
	target, err := s.replyTarget(ctx, batch[0])
	if err != nil {
		return out, err
	}
	var route *IMRoute
	if target != "" {
		route, err = s.imRoute(ctx, s.read, target)
		if err != nil {
			return out, err
		}
	}
	out.ReplyTarget = target
	ctx = context.WithValue(ctx, imTargetKey{}, target)
	if route.group() {
		ctx = context.WithValue(ctx, sharedTurnKey{}, true)
	}
	var prompt string
	if route.team() {
		ctx = context.WithValue(ctx, teamTurnKey{}, true)
		prompt = s.teamPrompt()
	} else {
		prompt = s.systemPromptFor(target == "")
	}
	messages := []Message{{Role: "system", Content: prompt}}
	if target == "" {
		messages = append(messages, s.loadSession()...)
	} else {
		hist, err := s.chatHistory(ctx, route, batch[0].ID)
		if err != nil {
			return out, err
		}
		messages = append(messages, hist...)
		if !s.restrictTeamTools(ctx) {
			messages[0].Content += "\nThis is an IM task. Respond in the user language. Your final answer will be delivered automatically to the originating conversation. send_message also targets only that conversation. Do not send progress messages with send_message; use it only for the final result. Timers are unavailable on this channel. Never claim a tool operation succeeded without a successful receipt."
		}
	}
	var input strings.Builder
	for _, ev := range batch {
		fmt.Fprintf(&input, "Event %d (%s): %s\n", ev.ID, ev.Payload.Kind, ev.Payload.Text)
		if ev.Payload.Kind == "approval" {
			a, e := s.executeAction(ctx, ev.Payload.ActionID)
			if e != nil {
				return out, e
			}
			ctx = context.WithValue(ctx, resumedActionKey{}, a)
			fmt.Fprintf(&input, "Original context: %s\nAction %s is %s. Receipt: %s. Do not execute it again. Inform the user or continue from this receipt.\n", a.Hint, a.ID, a.Status, a.Result)
		}
		if ev.Payload.Kind == "action_request" {
			result, pending, e := s.namedOperation(ctx, fmt.Sprintf("ev:%d:0", ev.ID), ev.Payload.Name, ev.Payload.Arguments, ev.Payload.Text)
			if e != nil {
				return out, e
			}
			if pending != nil {
				out.Approval = pending
				return out, nil
			}
			input.WriteString("Action result: " + result + "\n")
		}
	}
	messages = append(messages, Message{Role: "user", Content: input.String()})
	if s.cfg.OpenCodeBinary != "" {
		return s.executeOpenCode(ctx, batch[0], route, messages, out)
	}
	ordinal := 0
	for step := 0; step < s.cfg.Limits.MaxSteps; step++ {
		if e := ctx.Err(); e != nil {
			return out, e
		}
		// Bound the full prompt, not only individual tool output. A new run can retry with operator intervention.
		total := 0
		for _, m := range messages {
			total += len(m.Content)
			for _, tc := range m.ToolCalls {
				total += len(tc.Function.Arguments)
			}
		}
		if total > 128<<10 {
			return out, errors.New("context budget exceeded")
		}
		msg, e := s.model.Complete(ctx, messages, s.turnTools(ctx))
		if e != nil {
			return out, e
		}
		messages = append(messages, msg)
		if len(msg.ToolCalls) == 0 {
			out.Messages = messages
			s.addIMAnswer(&out, batch[0].ID, msg.Content)
			return out, nil
		}
		if len(msg.ToolCalls) > 8 {
			return out, errors.New("too many tool calls in one model response")
		}
		for _, call := range msg.ToolCalls {
			if e = ctx.Err(); e != nil {
				return out, e
			}
			ordinal++
			key := fmt.Sprintf("ev:%d:%d", batch[0].ID, ordinal)
			result, pending, toolErr := s.tool(ctx, key, call, input.String(), &out)
			if toolErr != nil {
				result = "ERROR: " + s.bounded(toolErr.Error())
			}
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: s.bounded(result)})
			if pending != nil {
				out.Approval = pending
				out.Messages = messages
				return out, nil
			}
		}
	}
	return out, errors.New("maximum model steps reached")
}

// readPage encodes one read_file page. JSON escaping can multiply the page's
// size, so the page shrinks until the encoded result fits the tool result
// limit; the caller never receives cut-off JSON or a lost next_offset.
func (s *Service) readPage(fields map[string]any, page []byte, offset int) string {
	// Leave room for redaction, which bounded() applies before its cut.
	limit := s.cfg.Limits.MaxOutputBytes - 256
	for {
		fields["content"] = string(page)
		fields["next_offset"] = offset + len(page)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(fields)
		if res := strings.TrimSuffix(buf.String(), "\n"); len(res) <= limit || len(page) == 0 {
			return res
		}
		page = page[:len(page)/2]
		if i := bytes.LastIndexByte(page, '\n'); i >= 0 {
			page = page[:i+1]
		} else {
			page = trimPartialRune(page)
		}
	}
}

// trimPartialRune drops an incomplete UTF-8 sequence at the end of a page.
func trimPartialRune(page []byte) []byte {
	for k := 0; k < utf8.UTFMax && len(page) > 0 && !utf8.Valid(page); k++ {
		page = page[:len(page)-1]
	}
	return page
}

func decodeArgs(raw string, v any) error {
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		return e
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra arguments")
	}
	return nil
}
func (s *Service) tool(ctx context.Context, key string, call ToolCall, hint string, out *Outcome) (result string, pending *Action, err error) {
	defer func() { s.recordTool(key, call, result, pending, err) }()
	if s.restrictTeamTools(ctx) {
		return "", nil, ErrDenied
	}
	name := call.Function.Name
	// Named HTTP operations use their own explicit policy, not shell permissions.
	if name != "bash" && s.cfg.Permissions[name] != "allow" {
		return "", nil, ErrDenied
	}
	if len(call.Function.Arguments) > s.cfg.Limits.MaxInputBytes {
		return "", nil, errors.New("tool arguments exceed limit")
	}
	switch name {
	case "read_file":
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		if strings.HasPrefix(path.Clean(a.Path), "sessions/") {
			// Transcripts hold the owner's private conversations.
			if isSharedTurn(ctx) {
				return "", nil, ErrDenied
			}
			page, size, e := s.transcriptRead(a.Path, int64(a.Offset), s.cfg.Limits.MaxOutputBytes/2)
			if e != nil {
				return "", nil, e
			}
			return s.readPage(map[string]any{"path": a.Path, "size": size}, page, a.Offset), nil, nil
		}
		b, e := s.workspaceRead(a.Path)
		if e != nil {
			return "", nil, e
		}
		if a.Offset < 0 || a.Offset > len(b) {
			return "", nil, errors.New("invalid offset")
		}
		page := b[a.Offset:min(len(b), a.Offset+s.cfg.Limits.MaxOutputBytes/2)]
		if a.Offset+len(page) < len(b) {
			page = trimPartialRune(page)
		}
		return s.readPage(map[string]any{"path": a.Path, "sha256": digest(b), "size": len(b)}, page, a.Offset), nil, nil
	case "write_file":
		var a struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Expected string `json:"expected_hash"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		e := s.workspaceWrite(a.Path, a.Content, a.Expected)
		return "file saved", nil, e
	case "edit_file":
		var a struct {
			Path     string `json:"path"`
			Old      string `json:"old_text"`
			New      string `json:"new_text"`
			Expected string `json:"expected_hash"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		b, e := s.workspaceRead(a.Path)
		if e != nil {
			return "", nil, e
		}
		if a.Old == "" || strings.Count(string(b), a.Old) != 1 {
			return "", nil, errors.New("old_text must match exactly once")
		}
		if digest(b) != a.Expected {
			return "", nil, ErrConflict
		}
		return "file edited", nil, s.workspaceWrite(a.Path, strings.Replace(string(b), a.Old, a.New, 1), a.Expected)
	case "set_timer":
		if target, _ := ctx.Value(imTargetKey{}).(string); target != "" {
			return "", nil, errors.New("Scheduled IM replies are unavailable. No timer was created.")
		}
		var a struct {
			ID     string `json:"id"`
			Cron   string `json:"cron_expr"`
			FireAt string `json:"fire_at"`
			Prompt string `json:"prompt"`
			Active bool   `json:"active"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		t := Timer{ID: a.ID, Cron: a.Cron, Prompt: a.Prompt, Active: a.Active}
		if a.FireAt != "" {
			dt, e := time.Parse(time.RFC3339, a.FireAt)
			if e != nil {
				return "", nil, e
			}
			t.FireAt = dt.UnixMilli()
		} else if !a.Active {
			t.FireAt = time.Now().UnixMilli()
		}
		t, e := s.normalizeTimer(t)
		if e != nil {
			return "", nil, e
		}
		if t.Active && t.FireAt < time.Now().Add(time.Second).UnixMilli() {
			return "", nil, errors.New("timer must be at least one second in the future")
		}
		out.Timers = append(out.Timers, t)
		return "timer staged for commit: " + t.ID, nil, nil
	case "send_message":
		var a struct {
			Target  string `json:"target"`
			Content string `json:"content"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		if a.Target == "" {
			a.Target = s.cfg.DefaultTarget
		}
		if target, _ := ctx.Value(imTargetKey{}).(string); target != "" {
			if a.Target != s.cfg.DefaultTarget && a.Target != "current" {
				return "", nil, ErrDenied
			}
			a.Target = target
		} else if _, ok := s.cfg.Targets[a.Target]; !ok {
			return "", nil, ErrDenied
		}
		if strings.TrimSpace(a.Content) == "" {
			return "", nil, errors.New("message is empty")
		}
		if e := checkSize(a.Content, s.cfg.Limits.MaxOutputBytes); e != nil {
			return "", nil, e
		}
		for _, n := range out.Notifications {
			if n.Target == a.Target && n.Content == a.Content {
				return "already queued in this run", nil, nil
			}
		}
		out.Notifications = append(out.Notifications, Notification{Key: "message:" + key, Target: a.Target, Content: a.Content, DeliverAt: time.Now().UnixMilli()})
		return "notification staged; delivery will occur after commit", nil, nil
	case "bash":
		var a struct {
			Command   string          `json:"command"`
			Action    string          `json:"action"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		if a.Action != "" && a.Command != "" {
			return "", nil, errors.New("choose command or action, not both")
		}
		if a.Action != "" {
			return s.namedOperation(ctx, key, a.Action, a.Arguments, hint)
		}
		if s.cfg.Permissions["bash"] != "allow" {
			return "", nil, ErrDenied
		}
		if a.Command == "" {
			return "", nil, errors.New("command is required")
		}
		text, e := s.runShell(ctx, key, a.Command)
		return text, nil, e
	default:
		return "", nil, fmt.Errorf("unknown tool %s", strconv.Quote(name))
	}
}
