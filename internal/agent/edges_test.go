package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestToolContracts(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	ctx := context.Background()
	out := Outcome{}
	invoke := func(name string, args any) (string, error) {
		b, _ := json.Marshal(args)
		v, _, e := s.tool(ctx, "ev:1:1", ToolCall{Function: FunctionCall{Name: name, Arguments: string(b)}}, "", &out)
		return v, e
	}
	if _, e := invoke("write_file", map[string]string{"path": "notes/tool.md", "content": "alpha", "expected_hash": "absent"}); e != nil {
		t.Fatal(e)
	}
	text, e := invoke("read_file", map[string]any{"path": "notes/tool.md", "offset": 0})
	if e != nil {
		t.Fatal(e)
	}
	var read map[string]any
	json.Unmarshal([]byte(text), &read)
	if _, e = invoke("edit_file", map[string]string{"path": "notes/tool.md", "old_text": "alpha", "new_text": "beta", "expected_hash": read["sha256"].(string)}); e != nil {
		t.Fatal(e)
	}
	b, e := s.workspaceRead("notes/tool.md")
	if e != nil || string(b) != "beta" {
		t.Fatal(string(b), e)
	}
	if _, e = invoke("set_timer", map[string]any{"id": "tool-timer", "prompt": "check", "active": true, "fire_at": time.Now().Add(time.Minute).Format(time.RFC3339)}); e != nil || len(out.Timers) != 1 {
		t.Fatal(e, out)
	}
	if count(t, s, "SELECT count(*) FROM timers") != 0 {
		t.Fatal("timer escaped staging")
	}
	if _, e = invoke("send_message", map[string]string{"target": "not-allowed", "content": "x"}); !errors.Is(e, ErrDenied) {
		t.Fatal("arbitrary recipient permitted", e)
	}
	if _, e = invoke("bash", map[string]string{"command": "echo no"}); !errors.Is(e, ErrDenied) {
		t.Fatal("shell enabled by default", e)
	}
	if e = decodeArgs(`{"path":"x"} {"path":"y"}`, &struct {
		Path string `json:"path"`
	}{}); e == nil {
		t.Fatal("trailing tool JSON accepted")
	}
}

func TestFailureLimitAndDiscardStagedNotifications(t *testing.T) {
	m := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		if m[len(m)-1].Role == "tool" {
			return Message{}, errors.New("model unavailable")
		}
		return call("send_message", `{"content":"must not leak before successful completion"}`), nil
	})
	s := openTest(t, testConfig(), m)
	s.Start(context.Background())
	s.Append(context.Background(), "fails", Payload{Kind: "message", Text: "test"})
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM inbox WHERE status='dead' AND retries=3") == 1 })
	if count(t, s, "SELECT count(*) FROM outbox") != 0 {
		t.Fatal("failed run sent staged output")
	}
}

func TestWebhookDeliveryRetriesAndIdempotencyKey(t *testing.T) {
	var attempts atomic.Int32
	var firstKey string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			t.Error("missing key")
		}
		if firstKey == "" {
			firstKey = key
		} else if firstKey != key {
			t.Error("key changed on retry")
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(429)
			return
		}
		w.WriteHeader(204)
	}))
	defer remote.Close()
	c := testConfig()
	c.Targets["hook"] = Target{Kind: "webhook", URL: remote.URL}
	s := openTest(t, c, finalModel())
	ctx := context.Background()
	e := transaction(ctx, s.db, func(tx *sql.Tx) error {
		return s.enqueueTx(ctx, tx, Notification{Key: "rate-test", Target: "hook", Content: "hello"})
	})
	if e != nil {
		t.Fatal(e)
	}
	if e = s.deliverOne(ctx); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM outbox WHERE status='pending' AND retries=1") != 1 {
		t.Fatal("429 not backed off")
	}
	s.db.Exec("UPDATE outbox SET deliver_at=0")
	if e = s.deliverOne(ctx); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'") != 1 || attempts.Load() != 2 {
		t.Fatal("delivery retry failed")
	}
}

func TestHTTPControlsQueriesAndTimer(t *testing.T) {
	s := openTest(t, testConfig(), finalModel())
	server := httptest.NewServer(s.Handler())
	defer server.Close()
	post(t, server, s, "/v1/control", map[string]string{"command": "pause"}, 200)
	post(t, server, s, "/v1/timers", Timer{ID: "http-timer", Prompt: "hello", FireAt: time.Now().UnixMilli(), Active: true}, 200)
	if e := s.fireTimers(context.Background()); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM inbox") != 0 {
		t.Fatal("timer fired while paused")
	}
	for _, endpoint := range []string{"status", "inbox", "timers", "actions", "outbox"} {
		req, _ := http.NewRequest("GET", server.URL+"/v1/"+endpoint, nil)
		req.Header.Set("Authorization", "Bearer "+s.Token())
		res, e := server.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatal(endpoint, res.StatusCode)
		}
	}
	post(t, server, s, "/v1/control", map[string]string{"command": "resume"}, 200)
	if e := s.fireTimers(context.Background()); e != nil {
		t.Fatal(e)
	}
	if count(t, s, "SELECT count(*) FROM inbox") != 1 {
		t.Fatal("timer not resumed")
	}
	req, _ := http.NewRequest("GET", server.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+s.Token())
	req.Header.Set("Origin", "https://untrusted.invalid")
	res, e := server.Client().Do(req)
	if e != nil {
		t.Fatal(e)
	}
	res.Body.Close()
	if res.StatusCode != 403 {
		t.Fatal("browser cross-origin permitted")
	}
}

func TestConfigurationStrictness(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.yaml")
	os.WriteFile(p, []byte("listen: 127.0.0.1:9000\nmodel:\n  name: test\n"), 0600)
	if _, e := LoadConfig(p); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(p, []byte("typo_field: x\n"), 0600)
	if _, e := LoadConfig(p); e == nil {
		t.Fatal("unknown config accepted")
	}
	c := testConfig()
	c.Listen = "0.0.0.0:9000"
	if e := c.Validate(); e == nil {
		t.Fatal("public bind enabled without explicit secure proxy")
	}
}

func TestSandboxNetworkAndTimeout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS seatbelt test")
	}
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(204) }))
	defer remote.Close()
	c := testConfig()
	c.Limits.ToolSeconds = 1
	s := openTest(t, c, finalModel())
	_, e := s.runShell(context.Background(), "network", "/usr/bin/curl --max-time 2 "+strconv.Quote(remote.URL))
	if e == nil || requests.Load() != 0 {
		t.Fatal("sandbox network was not blocked", e)
	}
	start := time.Now()
	_, e = s.runShell(context.Background(), "timeout", "sleep 30")
	if e == nil || time.Since(start) > 4*time.Second {
		t.Fatal("sandbox timeout did not terminate process group", e)
	}
}

func TestActionPolicyChangedAfterApproval(t *testing.T) {
	c := testConfig()
	c.Actions["x"] = Operation{Mode: "ask", URL: "http://127.0.0.1:1/original", Method: "POST"}
	s := openTest(t, c, finalModel())
	ctx := context.Background()
	_, a, e := s.namedOperation(ctx, "ev:1:1", "x", json.RawMessage(`{}`), "test")
	if e != nil {
		t.Fatal(e)
	}
	s.db.Exec("INSERT INTO actions(id,dedup_key,tool_name,tool_args,resume_hint,status,expires_at) VALUES(?,?,?,?,?,'approved',?)", a.ID, a.Key, a.Name, a.Args, a.Hint, a.ExpiresAt)
	s.cfg.Actions["x"] = Operation{Mode: "ask", URL: "http://127.0.0.1:1/changed", Method: "POST"}
	got, e := s.executeAction(ctx, a.ID)
	if e != nil || got.Status != "rejected" || !strings.Contains(got.Result, "changed") {
		t.Fatal(got, e)
	}
}

func TestActionReplayWithChangedToolOrdinal(t *testing.T) {
	var calls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(204) }))
	defer remote.Close()
	c := testConfig()
	c.Actions["write"] = Operation{Mode: "allow", URL: remote.URL, Method: "POST"}
	s := openTest(t, c, finalModel())
	ctx := context.Background()
	for _, key := range []string{"ev:1:2", "ev:1:5"} {
		if _, _, e := s.namedOperation(ctx, key, "write", json.RawMessage(`{"value":1}`), ""); e != nil {
			t.Fatal(e)
		}
	}
	if calls.Load() != 1 {
		t.Fatal("regenerated tool plan duplicated a completed write")
	}
	if _, _, e := s.namedOperation(ctx, "ev:1:6", "write", json.RawMessage(`{"value":2}`), ""); !errors.Is(e, ErrConflict) {
		t.Fatal("changed intent accepted", e)
	}
}

// Stored Inbox payloads are compared byte for byte on redelivery, so their
// encoding must not change when Payload is reorganised.
func TestPayloadEncodingIsStable(t *testing.T) {
	for _, c := range []struct {
		p    Payload
		want string
	}{
		{Payload{Kind: "dingtalk", Text: "hi", imRoutes: imRoutes{DingTalk: &IMRoute{ConversationID: "c", ConversationType: "1", Sender: "u"}}},
			`{"dingtalk":{"conversation_id":"c","conversation_type":"1","sender":"u","mentioned":false,"session_webhook":"","expires_at":0},"kind":"dingtalk","text":"hi"}`},
		{Payload{ownerScope: ownerScope{NewsTarget: "wecom-owner:x"}, Kind: "ai_news", imRoutes: imRoutes{WeCom: &IMRoute{BotID: "b"}}},
			`{"news_target":"wecom-owner:x","wecom":{"bot_id":"b","conversation_id":"","conversation_type":"","sender":"","mentioned":false,"session_webhook":"","expires_at":0},"kind":"ai_news"}`},
	} {
		b, err := json.Marshal(c.p)
		if err != nil || string(b) != c.want {
			t.Fatalf("payload encoding changed:\n got %s\nwant %s", b, c.want)
		}
	}
}

// Work recorded for the IM provider that is not selected stays queued until it
// is selected again; work for the selected provider runs.
func TestClaimParksOtherProviderWork(t *testing.T) {
	wc := wcConfig().WeCom
	wecomOwner := "wecom-owner:" + stableID(wc.BotID) + ":" + wc.Owner
	cases := []struct {
		name               string
		payload            string
		fromDingTalkAction bool
		dingtalk, wecom    int
	}{
		{"dingtalk message", `{"kind":"dingtalk","text":"x"}`, false, 1, 0},
		{"wecom message", `{"kind":"wecom","text":"x"}`, false, 0, 1},
		{"legacy unscoped news", `{"kind":"ai_news","text":"x"}`, false, 1, 0},
		{"wecom scoped news", `{"news_target":"` + wecomOwner + `","kind":"ai_news","text":"x"}`, false, 0, 1},
		{"approval from dingtalk event", `{"kind":"approval","action_id":"a1"}`, true, 1, 0},
		{"local message", `{"kind":"message","text":"x"}`, false, 1, 1},
	}
	for _, c := range cases {
		for provider, want := range map[string]int{"dingtalk": c.dingtalk, "wecom": c.wecom} {
			cfg := dtConfig()
			if provider == "wecom" {
				cfg = wcConfig()
			}
			s := openTest(t, cfg, finalModel())
			if c.fromDingTalkAction {
				res, err := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at,status) VALUES('origin',?,0,'done')", `{"kind":"dingtalk","text":"o"}`)
				if err != nil {
					t.Fatal(err)
				}
				origin, _ := res.LastInsertId()
				if _, err = s.db.Exec("INSERT INTO actions(id,dedup_key,tool_name,tool_args,status,expires_at) VALUES('a1',?,'x','{}','approved',0)", "ev:"+strconv.FormatInt(origin, 10)+":1"); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at) VALUES('case',?,0)", c.payload); err != nil {
				t.Fatal(err)
			}
			batch, err := s.claim(context.Background())
			if err != nil || len(batch) != want {
				t.Errorf("%s under %s: claimed %d, want %d (%v)", c.name, provider, len(batch), want, err)
			}
		}
	}
}
