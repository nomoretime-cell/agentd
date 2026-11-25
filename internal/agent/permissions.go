package agent

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// FrozenRequest includes destination and bytes, not a mutable file path or secret.
type FrozenRequest struct {
	URL       string          `json:"url"`
	Method    string          `json:"method"`
	Arguments json.RawMessage `json:"arguments"`
}

type resumedActionKey struct{}

func (s *Service) namedOperation(ctx context.Context, key, name string, args json.RawMessage, hint string) (string, *Action, error) {
	op, ok := s.cfg.Actions[name]
	if !ok || op.Mode == "deny" {
		return "", nil, ErrDenied
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var obj map[string]any
	if e := json.Unmarshal(args, &obj); e != nil || obj == nil {
		return "", nil, errors.New("operation arguments must be an object")
	}
	args, _ = json.Marshal(obj)
	if e := checkSize(string(args), s.cfg.Limits.MaxInputBytes); e != nil {
		return "", nil, e
	}
	frozen := FrozenRequest{URL: op.URL, Method: op.Method, Arguments: args}
	b, _ := json.Marshal(frozen)
	if op.Method == "GET" {
		// Named GET queries accept no model-controlled URL, body or credentials.
		if len(obj) != 0 {
			return "", nil, errors.New("named GET accepts empty arguments only")
		}
		text, status, e := s.performHTTP(ctx, frozen, op.BearerToken, "")
		if e != nil {
			return "", nil, e
		}
		if status < 200 || status >= 300 {
			return "", nil, fmt.Errorf("read operation HTTP %d", status)
		}
		return text, nil, nil
	}
	// A regenerated model plan may move a write to another tool ordinal. Bind the
	// intent to source event + operation, not transient tool position. This small
	// implementation allows one argument set per named operation per input event.
	parts := strings.SplitN(key, ":", 3)
	if len(parts) != 3 {
		return "", nil, errors.New("missing stable source event")
	}
	key = parts[0] + ":" + parts[1] + ":action:" + name
	if resumed, ok := ctx.Value(resumedActionKey{}).(Action); ok && resumed.Name == name {
		key = resumed.Key
	}
	s.mu.Lock()
	if s.paused || ctx.Err() != nil {
		s.mu.Unlock()
		return "", nil, ErrPaused
	}
	existing, e := scanAction(s.db.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM actions WHERE dedup_key=?", key))
	if e == nil {
		s.mu.Unlock()
		if existing.Name != name || existing.Args != string(b) {
			return "", nil, fmt.Errorf("%w: action intent changed on replay", ErrConflict)
		}
		switch existing.Status {
		case "pending_approval":
			return "", &existing, nil
		case "approved":
			a, e := s.executeAction(ctx, existing.ID)
			return a.Result, nil, e
		default:
			return fmt.Sprintf("action_id=%s status=%s result=%s", existing.ID, existing.Status, existing.Result), nil, nil
		}
	}
	if !errors.Is(e, sql.ErrNoRows) {
		s.mu.Unlock()
		return "", nil, e
	}
	// An uncertain prior write in this source event must not be bypassed with a new tool step.
	prefix := parts[0] + ":" + parts[1] + ":"
	var blocked int
	e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM actions WHERE substr(dedup_key,1,?)=? AND status IN ('unknown','executing','pending_approval')", len(prefix), prefix).Scan(&blocked)
	if e != nil {
		s.mu.Unlock()
		return "", nil, e
	}
	if blocked > 0 {
		s.mu.Unlock()
		return "", nil, fmt.Errorf("%w: an earlier action requires resolution", ErrConflict)
	}
	// A new input or tool ordinal must not silently bypass the same unresolved remote intent.
	e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM actions WHERE tool_name=? AND tool_args=? AND status IN ('unknown','executing')", name, string(b)).Scan(&blocked)
	if e != nil || blocked > 0 {
		s.mu.Unlock()
		if e != nil {
			return "", nil, e
		}
		return "", nil, fmt.Errorf("%w: identical remote intent still has unknown outcome", ErrConflict)
	}
	a := Action{ID: stableID("action:" + key), Key: key, Name: name, Args: string(b), Hint: hint, Status: "pending_approval", ExpiresAt: time.Now().Add(time.Duration(s.cfg.Limits.ApprovalSeconds) * time.Second).UnixMilli()}
	if op.Mode == "ask" {
		s.mu.Unlock()
		return "", &a, nil
	} // Persist atomically with approval notice and input completion.
	a.Status = "approved"
	_, e = s.db.ExecContext(ctx, "INSERT INTO actions(id,dedup_key,tool_name,tool_args,resume_hint,status,expires_at) VALUES(?,?,?,?,?,?,?)", a.ID, a.Key, a.Name, a.Args, a.Hint, a.Status, a.ExpiresAt)
	s.mu.Unlock()
	if e != nil {
		return "", nil, e
	}
	a, e = s.executeAction(ctx, a.ID)
	return fmt.Sprintf("action_id=%s status=%s result=%s", a.ID, a.Status, a.Result), nil, e
}

func (s *Service) executeAction(ctx context.Context, id string) (Action, error) {
	s.mu.Lock()
	a, e := scanAction(s.db.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM actions WHERE id=?", id))
	if e != nil {
		s.mu.Unlock()
		return a, e
	}
	if a.Status != "approved" {
		s.mu.Unlock()
		return a, nil
	}
	if s.paused || ctx.Err() != nil {
		s.mu.Unlock()
		return a, ErrPaused
	}
	var frozen FrozenRequest
	if e = json.Unmarshal([]byte(a.Args), &frozen); e != nil {
		s.mu.Unlock()
		return a, e
	}
	op, ok := s.cfg.Actions[a.Name]
	if a.ExpiresAt <= time.Now().UnixMilli() || !ok || op.Mode == "deny" || op.URL != frozen.URL || op.Method != frozen.Method {
		a.Status = "rejected"
		a.Result = "approval expired or configured operation changed"
		_, e = s.db.ExecContext(ctx, "UPDATE actions SET status='rejected',result=? WHERE id=? AND status='approved'", a.Result, id)
		s.mu.Unlock()
		return a, e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE actions SET status='executing' WHERE id=? AND status='approved'", id)
	s.mu.Unlock()
	if e != nil {
		return a, e
	}
	// The committed dispatch is now in flight. Cancellation cannot guarantee remote rollback.
	text, code, callErr := s.performHTTP(ctx, frozen, op.BearerToken, "action-"+id)
	a.Status = "unknown"
	a.Result = "remote outcome unknown; operator reconciliation required"
	if callErr == nil {
		if code >= 200 && code < 300 {
			a.Status = "succeeded"
			a.Result = text
		} else if definiteRejection(code) {
			a.Status = "failed"
			a.Result = fmt.Sprintf("HTTP %d: %s", code, text)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	saveCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, e = s.db.ExecContext(saveCtx, "UPDATE actions SET status=?,result=? WHERE id=? AND status='executing'", a.Status, s.bounded(a.Result), id)
	return a, e
}
func (s *Service) performHTTP(ctx context.Context, f FrozenRequest, token, key string) (string, int, error) {
	var body io.Reader
	if f.Method != "GET" {
		body = bytes.NewReader(f.Arguments)
	}
	req, e := http.NewRequestWithContext(ctx, f.Method, f.URL, body)
	if e != nil {
		return "", 0, e
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	res, e := s.client.Do(req)
	if e != nil {
		return "", 0, e
	}
	defer res.Body.Close()
	data, e := io.ReadAll(io.LimitReader(res.Body, int64(s.cfg.Limits.MaxOutputBytes)+1))
	if e != nil {
		return "", res.StatusCode, e
	}
	return s.bounded(string(data)), res.StatusCode, nil
}
func definiteRejection(code int) bool {
	return code == 400 || code == 401 || code == 403 || code == 404 || code == 405 || code == 409 || code == 410 || code == 413 || code == 415 || code == 422 || code == 429
}

func (s *Service) Decide(ctx context.Context, id, decision string) error {
	if decision != "approve" && decision != "reject" {
		return errors.New("decision must be approve or reject")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	expired := false
	e := transaction(ctx, s.db, func(tx *sql.Tx) error {
		a, e := scanAction(tx.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM actions WHERE id=?", id))
		if e != nil {
			return e
		}
		want := "approved"
		if decision == "reject" {
			want = "rejected"
		}
		if a.Status != "pending_approval" {
			if a.Status == want {
				return nil
			}
			return ErrConflict
		}
		if a.ExpiresAt <= time.Now().UnixMilli() {
			// A late decision records the expiry the sweep would have recorded.
			expired = true
			return expireActionTx(ctx, tx, id)
		}
		if _, e = tx.ExecContext(ctx, "UPDATE actions SET status=?,result=? WHERE id=? AND status='pending_approval'", want, "operator decision: "+want, id); e != nil {
			return e
		}
		e = queueActionContinuation(ctx, tx, id, "decision", "Approval decision recorded.")
		return e
	})
	if e == nil {
		s.notify()
	}
	if e == nil && expired && decision == "approve" {
		return fmt.Errorf("%w: approval expired", ErrConflict)
	}
	return e
}

// expireActionTx rejects an action whose approval window has passed. The
// sweep and late decisions both use it, so an expiry reads the same everywhere.
func expireActionTx(ctx context.Context, tx *sql.Tx, id string) error {
	if _, e := tx.ExecContext(ctx, "UPDATE actions SET status='rejected',result='approval expired' WHERE id=? AND status IN ('pending_approval','approved')", id); e != nil {
		return e
	}
	return queueActionContinuation(ctx, tx, id, "expired", "Approval expired.")
}
func (s *Service) expireActions(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return transaction(ctx, s.db, func(tx *sql.Tx) error {
		rows, e := tx.QueryContext(ctx, "SELECT id FROM actions WHERE status IN ('pending_approval','approved') AND expires_at<=?", time.Now().UnixMilli())
		if e != nil {
			return e
		}
		var ids []string
		for rows.Next() {
			var id string
			if e = rows.Scan(&id); e != nil {
				rows.Close()
				return e
			}
			ids = append(ids, id)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, id := range ids {
			if e = expireActionTx(ctx, tx, id); e != nil {
				return e
			}
		}
		return nil
	})
}
func (s *Service) ReconcileAction(ctx context.Context, id, status, result string) error {
	if status != "succeeded" && status != "failed" {
		return errors.New("reconciliation must confirm succeeded or failed; no automatic resend")
	}
	if strings.TrimSpace(result) == "" {
		return errors.New("reconciliation evidence is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := transaction(ctx, s.db, func(tx *sql.Tx) error {
		res, e := tx.ExecContext(ctx, "UPDATE actions SET status=?,result=? WHERE id=? AND status='unknown'", status, s.bounded(result), id)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return ErrConflict
		}
		e = queueActionContinuation(ctx, tx, id, "reconciled", "Operator reconciled action.")
		return e
	})
	if e == nil {
		s.notify()
	}
	return e
}

// Copy the original browser destination inside the action transition transaction.
// This also works for chains of approvals, whose source may itself be a continuation.
func queueActionContinuation(ctx context.Context, tx *sql.Tx, id, reason, text string) error {
	var chat sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT i.payload->>'web_chat' FROM actions a JOIN inbox i ON i.id=cast(substr(a.dedup_key,4) AS integer) WHERE a.id=? AND a.dedup_key GLOB 'ev:[0-9]*:*'`, id).Scan(&chat)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, _, err = insertEvent(ctx, tx, "action:"+id+":"+reason, Payload{Kind: "approval", ActionID: id, Text: text, WebChat: chat.String}, time.Now().UnixMilli())
	return err
}
