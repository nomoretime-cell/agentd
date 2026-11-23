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
	Scope         runScope
}

func timeNow() string { return time.Now().Format(time.RFC3339) }

// Critical actions write what every later run reads: memory consolidation now,
// skill drafting later. Code, not the prompt, binds them to the strong tier:
// they never fall back to the default model, they get only file tools, and
// they read only records that may leave this machine.
const (
	consolidationTimerID = "memory-consolidation"
	kindConsolidation    = "consolidation"
)

var (
	criticalKinds = map[string]bool{kindConsolidation: true}
	criticalTools = map[string]bool{"read_file": true, "search": true, "write_file": true, "edit_file": true}
)

type criticalKey struct{}

func isCritical(ctx context.Context) bool { v, _ := ctx.Value(criticalKey{}).(bool); return v }

// skippedError ends a critical run without retrying it. A critical action is
// periodic: the next occurrence starts from the same progress, while retries
// would dead-letter the event and stop the whole Inbox behind it.
type skippedError struct{ error }

func (e *skippedError) Unwrap() error { return e.error }

func (s *Service) execute(ctx context.Context, batch []Event) (out Outcome, err error) {
	if criticalKinds[batch[0].Payload.Kind] {
		defer func() {
			if err != nil {
				err = &skippedError{err}
			}
		}()
		return s.executeCritical(ctx, batch[0])
	}
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
	out.Scope = localScope
	if route != nil {
		out.Scope = s.imScope(route)
	}
	ctx = context.WithValue(ctx, imTargetKey{}, target)
	ctx = context.WithValue(ctx, scopeKey{}, out.Scope)
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
			// A local approval has a host receipt even when the model finishes
			// silently. Stage it with the outcome, once, at commit time.
			if target == "" && s.cfg.Targets[s.cfg.DefaultTarget].Kind == "local" && s.cfg.Permissions["send_message"] == "allow" {
				defer func() {
					if err == nil && out.Approval == nil && len(out.Notifications) == 0 {
						content := s.redact(fmt.Sprintf("Action %s: %s\n%s", a.Name, a.Status, a.Result))
						if len(content) > s.cfg.Limits.MaxOutputBytes {
							content = strings.ToValidUTF8(content[:s.cfg.Limits.MaxOutputBytes], "")
						}
						out.Notifications = append(out.Notifications, Notification{Key: "action-result:" + a.ID, Target: s.cfg.DefaultTarget, Content: content})
					}
				}()
			}
			fmt.Fprintf(&input, "Action %s is %s. Host receipt: %s\nResume the original request below. The receipt describes the action only. Complete the steps appropriate to this status. If a notification was requested, call send_message with the actual outcome before finishing. Do not repeat the action.\nOriginal context: %s\n", a.ID, a.Status, a.Result, a.Hint)
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
	return s.runModel(ctx, s.model, s.cfg.Limits.MaxSteps, 128<<10, batch[0], messages, input.String(), out)
}

// executeCritical runs a critical action on the strong tier with no chat
// history and no record of messages sent elsewhere.
func (s *Service) executeCritical(ctx context.Context, ev Event) (Outcome, error) {
	out := Outcome{Scope: maintenanceScope}
	if s.strong == nil {
		return out, errors.New("strong_model is not enabled; critical actions never run on the default model")
	}
	ctx = context.WithValue(ctx, scopeKey{}, out.Scope)
	ctx = context.WithValue(ctx, criticalKey{}, true)
	input := fmt.Sprintf("Event %d (%s): %s\n", ev.ID, ev.Payload.Kind, ev.Payload.Text)
	messages := []Message{{Role: "system", Content: s.systemPromptFor(false) + criticalGuide}, {Role: "user", Content: input}}
	sm := s.cfg.StrongModel
	if ev.Payload.Kind != kindConsolidation {
		return s.runModel(ctx, s.strong, sm.MaxSteps, sm.MaxContextBytes, ev, messages, input, out)
	}
	s.consolidating.Store(true)
	defer s.consolidating.Store(false)
	if err := s.prepareCandidate(); err != nil {
		return out, err
	}
	out, err := s.runModel(context.WithValue(ctx, candidateKey{}, true), s.strong, sm.MaxSteps, sm.MaxContextBytes, ev, messages, input, out)
	return s.finishConsolidation(ev, out, err)
}

const criticalGuide = "\nThis is a background maintenance run. Only file tools are available and nothing you write is sent to anyone. Transcripts show only records from the owner's private chats and local runs.\n"

func (s *Service) runModel(ctx context.Context, model Model, maxSteps, maxContext int, first Event, messages []Message, input string, out Outcome) (Outcome, error) {
	batch := []Event{first}
	ordinal := 0
	for step := 0; step < maxSteps; step++ {
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
		if total > maxContext {
			return out, errContextBudget
		}
		msg, e := model.Complete(ctx, messages, s.tools(ctx))
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
			result, pending, toolErr := s.tool(ctx, key, call, input, &out)
			if toolErr != nil {
				result = s.bounded("ERROR: " + toolErr.Error())
			}
			messages = append(messages, Message{Role: "tool", ToolCallID: call.ID, Content: result})
			if pending != nil {
				out.Approval = pending
				out.Messages = messages
				return out, nil
			}
		}
	}
	return out, errMaxSteps
}

var (
	errContextBudget = errors.New("context budget exceeded")
	errMaxSteps      = errors.New("maximum model steps reached")
)

// Maintenance only sees memory and its reference material. Drafts remain private to maintenance.
func fileAccess(ctx context.Context, p string) error {
	p = path.Clean(p)
	if isCritical(ctx) {
		if isMemoryPath(p) || p == "IDENTITY.md" || strings.HasPrefix(p, "skills/") || strings.HasPrefix(p, "knowledge/") {
			return nil
		}
		return ErrDenied
	}
	if strings.HasPrefix(p, draftsPrefix) {
		return ErrDenied
	}
	return nil
}

// tools offers what this run may call; critical runs get only file tools.
func (s *Service) tools(ctx context.Context) []Tool {
	all := s.turnTools(ctx)
	if !isCritical(ctx) {
		return all
	}
	var some []Tool
	for _, t := range all {
		if criticalTools[t.Function.Name] {
			some = append(some, t)
		}
	}
	return some
}

// readPage fits already-masked text and its metadata into one JSON result.
// The cursor advances in original bytes; a nonempty file must make progress.
func (s *Service) readPage(fields map[string]any, page []byte, offset, size int) (string, error) {
	for {
		fields["content"] = string(page)
		fields["next_offset"] = offset + len(page)
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(fields)
		res := strings.TrimSuffix(buf.String(), "\n")
		if len(res) <= s.cfg.Limits.MaxOutputBytes {
			if len(page) == 0 && offset < size {
				break
			}
			return res, nil
		}
		_, first := utf8.DecodeRune(page)
		if len(page) <= first {
			break
		}
		page = page[:max(first, len(page)/2)]
		if i := bytes.LastIndexByte(page, '\n'); i >= 0 {
			page = page[:i+1]
		} else {
			page = trimPartialRune(page)
		}
	}
	return "", fmt.Errorf("read_file cannot fit any content at offset %d within limits.max_output_bytes=%d; raise that limit", offset, s.cfg.Limits.MaxOutputBytes)
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
	defer func() {
		// Both executors receive the same bounded tool result. File pages
		// already fit and carry structured metadata that must stay intact.
		if err == nil && call.Function.Name != "read_file" {
			result = s.bounded(result)
		}
		s.recordTool(ctx, key, call, result, pending, err)
	}()
	if s.restrictTeamTools(ctx) {
		return "", nil, ErrDenied
	}
	name := call.Function.Name
	if isCritical(ctx) && !criticalTools[name] {
		return "", nil, ErrDenied
	}
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
			if isCritical(ctx) {
				res, e := s.readEgressTranscript(a.Path, a.Offset)
				return res, nil, e
			}
			page, size, e := s.transcriptRead(a.Path, int64(a.Offset), s.cfg.Limits.MaxOutputBytes/2)
			if e != nil {
				return "", nil, e
			}
			res, e := s.readPage(map[string]any{"path": s.redact(a.Path), "size": size}, []byte(s.maskedFile(string(page))), a.Offset, int(size))
			return res, nil, e
		}
		if e := fileAccess(ctx, a.Path); e != nil {
			return "", nil, e
		}
		b, e := s.workspaceReadIn(s.memoryHome(ctx), a.Path)
		if e != nil {
			return "", nil, e
		}
		if a.Offset < 0 || a.Offset > len(b) {
			return "", nil, errors.New("invalid offset")
		}
		view := s.maskedFile(string(b))
		page := []byte(view[a.Offset:min(len(view), a.Offset+s.cfg.Limits.MaxOutputBytes/2)])
		if a.Offset+len(page) < len(b) {
			page = trimPartialRune(page)
		}
		res, e := s.readPage(map[string]any{"path": s.redact(a.Path), "sha256": digest(b), "size": len(b)}, page, a.Offset, len(b))
		return res, nil, e
	case "search":
		var a struct {
			Query string `json:"query"`
			Path  string `json:"path"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		res, e := s.search(ctx, a.Query, a.Path)
		return res, nil, e
	case "write_file":
		var a struct {
			Path     string `json:"path"`
			Content  string `json:"content"`
			Expected string `json:"expected_hash"`
		}
		if e := decodeArgs(call.Function.Arguments, &a); e != nil {
			return "", nil, e
		}
		if e := fileAccess(ctx, a.Path); e != nil {
			return "", nil, e
		}
		e := s.workspaceWriteIn(s.memoryHome(ctx), a.Path, a.Content, a.Expected)
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
		if e := fileAccess(ctx, a.Path); e != nil {
			return "", nil, e
		}
		b, e := s.workspaceReadIn(s.memoryHome(ctx), a.Path)
		if e != nil {
			return "", nil, e
		}
		if a.Old == "" || strings.Count(string(b), a.Old) != 1 {
			return "", nil, errors.New("old_text must match exactly once")
		}
		if digest(b) != a.Expected {
			return "", nil, ErrConflict
		}
		return "file edited", nil, s.workspaceWriteIn(s.memoryHome(ctx), a.Path, strings.Replace(string(b), a.Old, a.New, 1), a.Expected)
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
