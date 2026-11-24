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

func workbenchCoverage(root string) (time.Time, error) {
	path := filepath.Join(root, "data/workbench-since")
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		now := time.Now().UTC()
		return now, atomicWrite(path, []byte(now.Format(time.RFC3339Nano)), 0600)
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, string(raw))
}

type callScopeKey struct{}
type callScope struct {
	Event   int64  `json:"event"`
	Attempt string `json:"attempt"`
	Purpose string `json:"purpose"`
}
type tokenUsage struct {
	Input  *int64 `json:"input"`
	Output *int64 `json:"output"`
	Total  *int64 `json:"total"`
}

func validTokenCount(n *int64) *int64 {
	if n == nil || *n < 0 || *n > 1<<50 {
		return nil
	}
	return n
}

func normalizedUsage(input, output, total *int64) tokenUsage {
	// Some providers omit the total. Standard prompt and completion counts
	// already include their cache and reasoning subcounts.
	u := tokenUsage{validTokenCount(input), validTokenCount(output), validTokenCount(total)}
	if u.Total == nil && u.Input != nil && u.Output != nil {
		n := *u.Input + *u.Output
		u.Total = &n
	}
	return u
}

type modelCall struct {
	callScope
	ID       string `json:"id"`
	At       int64  `json:"at"`
	Model    string `json:"model"`
	Executor string `json:"executor"`
	tokenUsage
}

// A receipt replaces the same call's placeholder. A new attempt gets a fresh ID.
func (s *Service) storeCall(ctx context.Context, c modelCall) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE inbox SET last_call_at=max(coalesce(last_call_at,0),?), model_calls=json_set(coalesce(model_calls,'{}'),?,json(?)) WHERE id=?`, c.At, "$."+c.ID, string(b), c.Event)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return sql.ErrNoRows
	}
	return err
}
func (s *Service) startCall(ctx context.Context, executor, model string) (modelCall, error) {
	sc, _ := ctx.Value(callScopeKey{}).(callScope)
	if sc.Event == 0 {
		return modelCall{}, nil
	}
	c := modelCall{callScope: sc, ID: randomID(), At: time.Now().UnixMilli(), Executor: executor, Model: s.redact(model)}
	return c, s.storeCall(ctx, c)
}
func (s *Service) finishCall(c modelCall, usage tokenUsage) {
	if c.ID == "" || (usage.Input == nil && usage.Output == nil && usage.Total == nil) {
		return
	}
	c.tokenUsage = usage
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.logErr("model usage receipt", s.storeCall(ctx, c))
}
func (s *Service) completeMeasured(ctx context.Context, m Model, messages []Message, tools []Tool) (Message, error) {
	name := s.cfg.Model.Name
	if isCritical(ctx) {
		name = s.cfg.StrongModel.Name
		if selected, ok := ctx.Value(dreamNameKey{}).(string); ok {
			name = selected
		}
	}
	c, err := s.startCall(ctx, "native", name)
	if err != nil {
		return Message{}, err
	}
	s.submitted(ctx, c.ID, messages)
	msg, err := m.Complete(ctx, messages, tools)
	s.finishCall(c, msg.usage)
	return msg, err
}

type formalReply struct {
	ID           string `json:"id"`
	Attempt      string `json:"attempt"`
	Conversation string `json:"conversation"`
	Body         string `json:"body"`
	DeliveryKey  string `json:"delivery_key,omitempty"`
}

func (s *Service) commitReplies(ctx context.Context, tx *sql.Tx, first Event, out Outcome) error {
	var replies []formalReply
	if out.ReplyTarget != "" {
		for _, n := range out.Notifications {
			if n.Target == out.ReplyTarget && (strings.HasPrefix(n.Key, "reply:") || strings.HasPrefix(n.Key, "message:")) {
				replies = append(replies, formalReply{n.Key, out.Attempt, out.Scope.Conversation, s.redact(n.Content), n.Key})
			}
		}
	} else if (first.Payload.Kind == "message" || first.Payload.WebChat != "" && first.Payload.Kind == "approval") && out.Approval == nil && len(out.Messages) > 0 {
		last := out.Messages[len(out.Messages)-1]
		if last.Role == "assistant" && strings.TrimSpace(last.Content) != "" {
			replies = append(replies, formalReply{fmt.Sprintf("local:%d", first.ID), out.Attempt, out.Scope.Conversation, s.redact(last.Content), ""})
		}
	}
	if len(replies) == 0 {
		return nil
	}
	b, err := json.Marshal(replies)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE inbox SET reply_at=?, replies=? WHERE id=? AND reply_at IS NULL", time.Now().UnixMilli(), string(b), first.ID)
	return err
}
