package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in: real inference against the user's local OpenAI-compatible model server.
// All side effects are contained in a fresh project-local root and a loopback receiver.
func TestLiveFivePrimitives(t *testing.T) {
	if os.Getenv("AGENTD_LIVE") != "1" {
		t.Skip("set AGENTD_LIVE=1 to exercise the local model")
	}
	project, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	cfg, e := LoadConfig(filepath.Join(project, "config.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	cfg.Quiet = QuietHours{}
	cfg.DeliveryIntervalMS = 0
	cfg.Limits.DebounceMS = 20
	cfg.Limits.PollMS = 20
	cfg.Limits.MaxHistoryMessages = 8
	cfg.Limits.RunSeconds = 180
	cfg.Model.TimeoutSeconds = 120
	root := filepath.Join(project, ".cache/live-e2e", time.Now().UTC().Format("20060102T150405.000000000"))
	var remoteCalls atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") == "" {
			t.Error("live action missing idempotency key")
		}
		remoteCalls.Add(1)
		writeJSON(w, 200, map[string]string{"receipt": "E2E_APPROVED_RECEIPT"})
	}))
	defer receiver.Close()
	cfg.Actions = map[string]Operation{"e2e_write": {Mode: "ask", Method: "POST", URL: receiver.URL}}
	s, e := Open(root, cfg, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	s.Start(context.Background())
	t.Log("live model:", cfg.Model.Name, "endpoint:", cfg.Model.Endpoint, "artifacts:", root)
	marker := "MEMORY_" + stableID(root)[:10]
	post(t, api, s, "/v1/events", map[string]string{"key": "live-memory-write", "text": fmt.Sprintf("Use write_file now to create notes/e2e.md with exact content %s and expected_hash absent. Then call send_message with content E2E_MEMORY_SAVED. Do not use bash. Stop after that.", marker)}, 200)
	liveWait(t, s, 120*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM outbox WHERE content LIKE '%E2E_MEMORY_SAVED%' AND status='sent'") > 0
	})
	b, e := s.workspaceRead("notes/e2e.md")
	if e != nil || strings.TrimSpace(string(b)) != marker {
		t.Fatalf("memory not written: %q %v", b, e)
	}
	t.Log("PASS: HTTP inbox -> real model tools -> writable memory -> outbox -> local delivery")

	post(t, api, s, "/v1/events", map[string]string{"key": "live-memory-read", "text": "Use read_file to read notes/e2e.md. Then call send_message with content E2E_MEMORY_READ followed by the exact file content. Stop after sending."}, 200)
	liveWait(t, s, 120*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM outbox WHERE content LIKE ? AND content LIKE '%E2E_MEMORY_READ%' AND status='sent'", "%"+marker+"%") > 0
	})
	t.Log("PASS: next run retrieves persistent memory")

	post(t, api, s, "/v1/timers", Timer{ID: "live-once", FireAt: time.Now().Add(200 * time.Millisecond).UnixMilli(), Prompt: "This timer has fired. Call send_message exactly once with content E2E_TIMER_FIRED, then finish.", Active: true}, 200)
	liveWait(t, s, 120*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM outbox WHERE content LIKE '%E2E_TIMER_FIRED%' AND status='sent'") > 0
	})
	if count(t, s, "SELECT count(*) FROM timers WHERE id='live-once' AND active=0") != 1 {
		t.Fatal("timer not consumed")
	}
	t.Log("PASS: durable timer -> inbox -> real model -> outbox")

	post(t, api, s, "/v1/action-requests", map[string]any{"key": "live-approval", "name": "e2e_write", "arguments": map[string]string{"title": "safe loopback test"}, "text": "After the host approves and executes this operation, call send_message with content E2E_APPROVED_RECEIPT. Do not invoke the action again."}, 200)
	liveWait(t, s, 10*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM actions WHERE status='pending_approval'") == 1 })
	if remoteCalls.Load() != 0 {
		t.Fatal("operation ran before approval")
	}
	var actionID string
	if e = s.read.QueryRow("SELECT id FROM actions WHERE status='pending_approval'").Scan(&actionID); e != nil {
		t.Fatal(e)
	}
	post(t, api, s, "/v1/actions/"+actionID+"/decision", map[string]string{"decision": "approve"}, 200)
	liveWait(t, s, 120*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM outbox WHERE content LIKE '%E2E_APPROVED_RECEIPT%' AND status='sent'") > 0
	})
	if remoteCalls.Load() != 1 {
		t.Fatalf("remote write count %d", remoteCalls.Load())
	}
	t.Log("PASS: permission ask -> approval notice -> operator approval -> one remote write -> real model continuation")
	liveWait(t, s, 10*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM inbox WHERE status IN ('pending','running','dead')") == 0
	})
	report := map[string]any{"passed": true, "model": cfg.Model.Name, "endpoint": cfg.Model.Endpoint, "completed_at": time.Now().Format(time.RFC3339), "root": root, "remote_action_calls": remoteCalls.Load(), "inbox_done": count(t, s, "SELECT count(*) FROM inbox WHERE status='done'"), "notifications_sent": count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'"), "flows": []string{"inbox-memory-outbox", "memory-across-runs", "timer-inbox-outbox", "approval-action-resume"}}
	data, _ := json.MarshalIndent(report, "", "  ")
	if e = atomicWrite(filepath.Join(root, "report.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	t.Log("report:", filepath.Join(root, "report.json"))
}
func liveWait(t *testing.T, s *Service, d time.Duration, condition func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if condition() {
			return
		}
		if count(t, s, "SELECT count(*) FROM inbox WHERE status='dead'") > 0 {
			for _, table := range []string{"inbox", "actions", "outbox"} {
				v, _ := s.list(context.Background(), table)
				t.Log(table, v)
			}
			t.Fatal("live workflow reached dead letter")
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, table := range []string{"inbox", "actions", "outbox"} {
		v, _ := s.list(context.Background(), table)
		t.Log(table, v)
	}
	t.Fatal("live workflow timed out")
}
