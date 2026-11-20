package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}
type Model interface {
	Complete(context.Context, []Message, []Tool) (Message, error)
}
type HTTPModel struct {
	cfg    ModelConfig
	client *http.Client
}

func NewHTTPModel(cfg ModelConfig) *HTTPModel {
	return &HTTPModel{cfg: cfg, client: &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (m *HTTPModel) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	body := map[string]any{"model": m.cfg.Name, "messages": messages, "max_tokens": m.cfg.MaxTokens, "stream": false, "temperature": 0, "chat_template_kwargs": map[string]any{"enable_thinking": false}}
	if len(tools) > 0 {
		body["tools"], body["tool_choice"], body["parallel_tool_calls"] = tools, "auto", false
	}
	b, e := json.Marshal(body)
	if e != nil {
		return Message{}, e
	}
	req, e := http.NewRequestWithContext(ctx, http.MethodPost, m.cfg.Endpoint, bytes.NewReader(b))
	if e != nil {
		return Message{}, e
	}
	req.Header.Set("Content-Type", "application/json")
	if m.cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.cfg.APIKey)
	}
	res, e := m.client.Do(req)
	if e != nil {
		return Message{}, fmt.Errorf("model request: %w", e)
	}
	defer res.Body.Close()
	b, e = io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if e != nil {
		return Message{}, e
	}
	if res.StatusCode != 200 {
		return Message{}, fmt.Errorf("model HTTP %d", res.StatusCode)
	}
	var result struct {
		Choices []struct {
			Message Message `json:"message"`
			Finish  string  `json:"finish_reason"`
		} `json:"choices"`
	}
	if e = json.Unmarshal(b, &result); e != nil {
		return Message{}, e
	}
	if len(result.Choices) != 1 {
		return Message{}, errors.New("model returned no single choice")
	}
	if result.Choices[0].Finish == "length" {
		return Message{}, errors.New("model output limit reached")
	}
	msg := result.Choices[0].Message
	msg.Role = "assistant"
	if len(msg.ToolCalls) == 0 && strings.TrimSpace(msg.Content) == "" {
		return Message{}, errors.New("empty model response")
	}
	for i := range msg.ToolCalls {
		if msg.ToolCalls[i].ID == "" {
			msg.ToolCalls[i].ID = fmt.Sprintf("call_%d", i)
		}
		msg.ToolCalls[i].Type = "function"
	}
	return msg, nil
}
func toolDefinitions() []Tool {
	str := func(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }
	def := func(name, desc string, props map[string]any, required ...string) Tool {
		return Tool{Type: "function", Function: ToolFunction{Name: name, Description: desc, Parameters: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}}
	}
	return []Tool{
		def("read_file", "Read a workspace file, optionally by byte offset. Paths are relative to workspace.", map[string]any{"path": str("File path"), "offset": map[string]any{"type": "integer", "minimum": 0}}, "path"),
		def("write_file", "Replace MEMORY.md or a file under notes/ or scratch/. Existing files require expected_hash from read_file; new files use expected_hash=absent.", map[string]any{"path": str("File path"), "content": str("Full text"), "expected_hash": str("SHA-256 from read_file, or absent")}, "path", "content", "expected_hash"),
		def("edit_file", "Replace exactly one matching text in an allowed workspace file. Requires current expected_hash.", map[string]any{"path": str("File path"), "old_text": str("Exact unique old text"), "new_text": str("Replacement"), "expected_hash": str("Current SHA-256")}, "path", "old_text", "new_text", "expected_hash"),
		def("set_timer", "Create, update or cancel a durable timer. Cron uses five fields in configured timezone. One-shot timers require fire_at in RFC3339. active=false cancels.", map[string]any{"id": str("Stable timer name"), "cron_expr": str("Optional five-field cron"), "fire_at": str("Optional RFC3339 instant"), "prompt": str("Instructions for the later run"), "active": map[string]any{"type": "boolean"}}, "id", "prompt", "active"),
		def("send_message", "Queue a notification to a configured target. Use this to send the final answer; do not repeat the same notification. Does not mean delivered yet.", map[string]any{"target": str("Target name; omit for default"), "content": str("Concise notification")}, "content"),
		def("bash", "Either run command in an isolated, network-disabled scratch sandbox (if allowed), OR invoke a configured named HTTP operation using action and arguments. Never combine command and action. Action permissions are enforced by the host.", map[string]any{"command": str("Sandbox shell command"), "action": str("Configured operation name"), "arguments": map[string]any{"type": "object"}}),
	}
}
