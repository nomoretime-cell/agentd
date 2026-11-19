package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func dtConfig() Config {
	c := testConfig()
	c.DingTalk = DingTalkConfig{Enabled: true, ClientID: "test-app", ClientSecret: "test-client-secret", AllowedSender: "owner"}
	return c
}
func dtMessage() dingTalkMessage {
	m := dingTalkMessage{MsgID: "message-1", MsgType: "text", ConversationID: "direct-owner", ConversationType: "1", Sender: "owner", Webhook: "https://oapi.dingtalk.com/robot/sendBySession?session=secret-session", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	m.Text.Content = "请完成测试任务并回复结果"
	return m
}
func dtFrame(m dingTalkMessage) dingTalkFrame {
	b, _ := json.Marshal(m)
	return dingTalkFrame{Type: "CALLBACK", Headers: map[string]string{"topic": dingTalkTopic, "messageId": "frame-" + m.MsgID}, Data: string(b)}
}

// Wire-level loopback WebSocket peer. No public socket or DingTalk credential is used.
func openDTStream(t *testing.T, s *Service) *websocket.Conn {
	t.Helper()
	peers := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, e := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if e != nil {
			return
		}
		peers <- c
	}))
	t.Cleanup(server.Close)
	conn, _, e := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if e != nil {
		t.Fatal(e)
	}
	peer := <-peers
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = s.serveDingTalk(ctx, conn) }()
	t.Cleanup(func() {
		cancel()
		peer.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("Stream did not stop on cancellation")
		}
	})
	return peer
}
func sendDT(t *testing.T, s *Service, peer *websocket.Conn, m dingTalkMessage) {
	t.Helper()
	if e := peer.WriteJSON(dtFrame(m)); e != nil {
		t.Fatal(e)
	}
	peer.SetReadDeadline(time.Now().Add(3 * time.Second))
	var ack dingTalkACK
	if e := peer.ReadJSON(&ack); e != nil {
		t.Fatal(e)
	}
	if ack.Code != 200 || ack.Headers["messageId"] != "frame-"+m.MsgID {
		t.Fatalf("bad ack: %+v", ack)
	}
	// The service must commit before sending its ACK.
	if count(t, s, "SELECT count(*) FROM inbox") != 1 {
		t.Fatal("ACK before durable insert, or duplicate task")
	}
}
func startDTWorkers(s *Service) {
	// Exercise the real workers without dialing the public DingTalk gateway.
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for _, fn := range []func(context.Context){s.workerLoop, s.timerLoop, s.outboxLoop} {
		s.wg.Add(1)
		go func(f func(context.Context)) { defer s.wg.Done(); f(ctx) }(fn)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func routeDTToLocal(s *Service, receiver *httptest.Server) {
	base := receiver.Client().Transport
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "oapi.dingtalk.com" {
			return nil, errors.New("test forbids public network")
		}
		cloned := r.Clone(r.Context())
		u := *r.URL
		target, _ := url.Parse(receiver.URL)
		u.Scheme = target.Scheme
		u.Host = target.Host
		cloned.URL = &u
		return base.RoundTrip(cloned)
	})
}
func TestDingTalkStreamTaskAndResult(t *testing.T) {
	var modelCalls, delivered atomic.Int32
	model := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		all, _ := json.Marshal(m)
		if strings.Contains(string(all), "secret-session") || strings.Contains(string(all), "session_webhook") {
			t.Error("credential exposed to model")
		}
		switch modelCalls.Add(1) {
		case 1:
			return call("write_file", `{"path":"notes/dingtalk-test.md","content":"DT_EXECUTED","expected_hash":"absent"}`), nil
		default:
			return Message{Role: "assistant", Content: "DT_EXECUTED：文件已保存"}, nil
		}
	})
	c := dtConfig()
	c.Quiet = QuietHours{Start: "00:00", End: "23:59"}
	s := openTest(t, c, model)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if e := json.NewDecoder(r.Body).Decode(&b); e != nil || b.MsgType != "text" || !strings.Contains(b.Text.Content, "DT_EXECUTED") {
			t.Errorf("incorrect DingTalk payload: %+v %v", b, e)
		}
		if r.URL.Query().Get("session") != "secret-session" {
			t.Error("wrong destination")
		}
		delivered.Add(1)
		fmt.Fprint(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	defer receiver.Close()
	routeDTToLocal(s, receiver)
	peer := openDTStream(t, s)
	m := dtMessage()
	sendDT(t, s, peer, m)
	sendDT(t, s, peer, m)
	startDTWorkers(s)
	waitFor(t, 4*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'") == 1 })
	b, e := s.workspaceRead("notes/dingtalk-test.md")
	if e != nil || string(b) != "DT_EXECUTED" {
		t.Fatalf("task did not execute: %q %v", b, e)
	}
	sendDT(t, s, peer, m)
	if delivered.Load() != 1 || modelCalls.Load() != 2 {
		t.Fatalf("duplicate execution or delivery: %d/%d", modelCalls.Load(), delivered.Load())
	}
	if count(t, s, "SELECT count(*) FROM outbox WHERE target='dingtalk:1'") != 1 {
		t.Fatal("reply not bound to source")
	}
	rows, e := s.list(context.Background(), "inbox")
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(rows)
	if strings.Contains(string(raw), "secret-session") {
		t.Fatal("webhook leaked through status API")
	}
	if fileExists(filepath.Join(s.root, "data/session.jsonl")) {
		t.Fatal("DingTalk chat polluted global session")
	}
}
func TestDingTalkPolicyAndMalformedInput(t *testing.T) {
	cases := []struct {
		name string
		edit func(*dingTalkMessage)
	}{
		{"other sender", func(m *dingTalkMessage) { m.Sender = "stranger" }},
		{"unlisted group", func(m *dingTalkMessage) { m.ConversationType = "2"; m.Mentioned = true }},
		{"unsupported type", func(m *dingTalkMessage) { m.MsgType = "picture" }},
		{"expired webhook", func(m *dingTalkMessage) { m.ExpiresAt = 1 }},
		{"SSRF", func(m *dingTalkMessage) { m.Webhook = "http://127.0.0.1:8080/" }},
		{"foreign webhook", func(m *dingTalkMessage) {
			m.Webhook = "https://oapi.dingtalk.com.attacker.example/robot/sendBySession?session=x"
		}},
		{"blank text", func(m *dingTalkMessage) { m.Text.Content = " " }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t, dtConfig(), finalModel())
			m := dtMessage()
			tc.edit(&m)
			ack := s.acceptDingTalk(context.Background(), dtFrame(m))
			if ack.Code != 200 || count(t, s, "SELECT count(*) FROM inbox") != 0 {
				t.Fatal("unsafe input accepted")
			}
		})
	}
	t.Run("group mention and allowlist", func(t *testing.T) {
		c := dtConfig()
		c.DingTalk.AllowedConversations = []string{"test-group"}
		s := openTest(t, c, finalModel())
		m := dtMessage()
		m.ConversationID = "test-group"
		m.ConversationType = "2"
		s.acceptDingTalk(context.Background(), dtFrame(m))
		if count(t, s, "SELECT count(*) FROM inbox") != 0 {
			t.Fatal("unmentioned group accepted")
		}
		m.Mentioned = true
		s.acceptDingTalk(context.Background(), dtFrame(m))
		if count(t, s, "SELECT count(*) FROM inbox") != 1 {
			t.Fatal("allowed group rejected")
		}
	})
	t.Run("discovery", func(t *testing.T) {
		c := dtConfig()
		c.DingTalk.DiscoveryOnly = true
		s := openTest(t, c, finalModel())
		s.acceptDingTalk(context.Background(), dtFrame(dtMessage()))
		if count(t, s, "SELECT count(*) FROM inbox") != 0 {
			t.Fatal("discovery executed task")
		}
	})
}
func TestDingTalkDeliveryReceipts(t *testing.T) {
	for _, tc := range []struct {
		name       string
		code       int
		body, want string
	}{
		{"success", 200, `{"errcode":0}`, "sent"},
		{"business error", 200, `{"errcode":40035,"errmsg":"invalid parameter"}`, "failed"},
		{"missing receipt", 200, `{}`, "unknown"},
		{"malformed", 200, `not JSON`, "unknown"},
		{"server failure", 503, `{}`, "unknown"},
		{"forbidden", 403, `{}`, "failed"},
		{"rate limit", 429, `{}`, "pending"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTest(t, dtConfig(), finalModel())
			s.acceptDingTalk(context.Background(), dtFrame(dtMessage()))
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.code); fmt.Fprint(w, tc.body) }))
			defer receiver.Close()
			routeDTToLocal(s, receiver)
			_, e := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES('test','dingtalk:1','result',0)")
			if e != nil {
				t.Fatal(e)
			}
			if e = s.deliverOne(context.Background()); e != nil {
				t.Fatal(e)
			}
			var status string
			s.read.QueryRow("SELECT status FROM outbox").Scan(&status)
			if status != tc.want {
				t.Fatalf("got %s want %s", status, tc.want)
			}
		})
	}
}
func TestDingTalkPersistenceFailureAndPing(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ack := s.acceptDingTalk(ctx, dtFrame(dtMessage()))
	if ack.Code != 500 || count(t, s, "SELECT count(*) FROM inbox") != 0 {
		t.Fatal("ACK on failed commit")
	}
	peer := openDTStream(t, s)
	ping := dingTalkFrame{Type: "SYSTEM", Headers: map[string]string{"topic": "ping", "messageId": "ping-1"}, Data: `{"opaque":"value"}`}
	if e := peer.WriteJSON(ping); e != nil {
		t.Fatal(e)
	}
	peer.SetReadDeadline(time.Now().Add(time.Second))
	if e := peer.ReadJSON(&ack); e != nil {
		t.Fatal(e)
	}
	if ack.Code != 200 || ack.Data != ping.Data || ack.Headers["messageId"] != "ping-1" {
		t.Fatalf("invalid pong %+v", ack)
	}
}
func TestDingTalkRouteAndToolRestrictions(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	m := dtMessage()
	s.acceptDingTalk(context.Background(), dtFrame(m))
	// Redelivery can refresh expiry, but cannot overwrite a previously accepted goal.
	m.Text.Content = "changed"
	s.acceptDingTalk(context.Background(), dtFrame(m))
	var raw string
	s.read.QueryRow("SELECT payload FROM inbox").Scan(&raw)
	if strings.Contains(raw, "changed") {
		t.Fatal("duplicate overwrote task")
	}
	m = dtMessage()
	m.ExpiresAt += 10000
	m.Webhook = "https://oapi.dingtalk.com/robot/sendBySession?session=refreshed"
	s.acceptDingTalk(context.Background(), dtFrame(m))
	r, e := s.dingTalkRoute(context.Background(), s.read, "dingtalk:1")
	if e != nil || r.Webhook != m.Webhook {
		t.Fatal("webhook refresh failed", e)
	}
	ctx := context.WithValue(context.Background(), dingTalkTargetKey{}, "dingtalk:1")
	out := Outcome{}
	_, _, e = s.tool(ctx, "test", call("send_message", `{"target":"another-user","content":"leak"}`).ToolCalls[0], "", &out)
	if !errors.Is(e, ErrDenied) {
		t.Fatal("cross destination permitted")
	}
	_, _, e = s.tool(ctx, "test", call("send_message", `{"content":"result"}`).ToolCalls[0], "", &out)
	if e != nil || len(out.Notifications) != 1 || out.Notifications[0].Target != "dingtalk:1" {
		t.Fatal("source routing failed", e)
	}
	_, _, e = s.tool(ctx, "test", call("set_timer", `{}`).ToolCalls[0], "", &out)
	if e == nil || len(out.Timers) != 0 {
		t.Fatal("unsupported channel timer accepted")
	}
}
func TestDingTalkNoResendUnknownOrExpired(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			s := openTest(t, dtConfig(), finalModel())
			s.acceptDingTalk(context.Background(), dtFrame(dtMessage()))
			var calls atomic.Int32
			s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, io.ErrUnexpectedEOF })
			if expired {
				_, e := s.db.Exec("UPDATE inbox SET payload=json_set(payload,'$.dingtalk.expires_at',1)")
				if e != nil {
					t.Fatal(e)
				}
			}
			_, e := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES('test','dingtalk:1','result',0)")
			if e != nil {
				t.Fatal(e)
			}
			for i := 0; i < 3; i++ {
				if e = s.deliverOne(context.Background()); e != nil {
					t.Fatal(e)
				}
			}
			want := "unknown"
			n := int32(1)
			if expired {
				want = "failed"
				n = 0
			}
			if count(t, s, "SELECT count(*) FROM outbox WHERE status=?", want) != 1 || calls.Load() != n {
				t.Fatal("unsafe resend or expired send")
			}
		})
	}
}
func TestDingTalkLiveLocalModel(t *testing.T) {
	if os.Getenv("AGENTD_LIVE") != "1" {
		t.Skip("set AGENTD_LIVE=1")
	}
	project, e := filepath.Abs("../..")
	if e != nil {
		t.Fatal(e)
	}
	c, e := LoadConfig(filepath.Join(project, "config.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	c.DingTalk = dtConfig().DingTalk
	c.Limits.DebounceMS = 10
	c.Limits.PollMS = 20
	c.DeliveryIntervalMS = 0
	root := filepath.Join(project, ".cache/dingtalk-e2e", time.Now().UTC().Format("20060102T150405.000000000"))
	model := NewHTTPModel(c.Model)
	model.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		resp, err := http.DefaultTransport.RoundTrip(r)
		if err == nil && resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			t.Logf("local model failure: HTTP %d: %s", resp.StatusCode, b)
			resp.Body = io.NopCloser(strings.NewReader(string(b)))
		}
		return resp, err
	})
	s, e := Open(root, c, model)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	received := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct {
			MsgType string `json:"msgtype"`
			Text    struct {
				Content string `json:"content"`
			} `json:"text"`
		}
		if e := json.NewDecoder(r.Body).Decode(&b); e != nil || b.MsgType != "text" {
			t.Error("invalid reply", e)
		}
		received <- b.Text.Content
		fmt.Fprint(w, `{"errcode":0,"errmsg":"ok"}`)
	}))
	t.Cleanup(receiver.Close)
	routeDTToLocal(s, receiver)
	peer := openDTStream(t, s)
	m := dtMessage()
	m.Text.Content = "请立即调用 write_file，将 notes/dingtalk-live.md 创建为内容 DT_LIVE_EXECUTED_20261117，expected_hash 为 absent。完成后用中文回复结果，并包含 DT_LIVE_EXECUTED_20261117。不要使用 bash。"
	sendDT(t, s, peer, m)
	sendDT(t, s, peer, m)
	startDTWorkers(s)
	liveWait(t, s, 150*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'") == 1 })
	b, e := s.workspaceRead("notes/dingtalk-live.md")
	if e != nil || !strings.Contains(string(b), "DT_LIVE_EXECUTED_20261117") {
		t.Fatalf("model did not execute file task: %q %v", b, e)
	}
	var reply string
	select {
	case reply = <-received:
	case <-time.After(time.Second):
		t.Fatal("missing webhook delivery")
	}
	if !strings.Contains(reply, "DT_LIVE_EXECUTED_20261117") {
		t.Fatal("incorrect result:", reply)
	}
	report := map[string]any{"passed": true, "real_dingtalk_cloud": false, "real_model": true, "model": c.Model.Name, "completed_at": time.Now().Format(time.RFC3339), "input_transport": "loopback websocket with DingTalk frames", "output_transport": "DingTalk text webhook protocol over loopback HTTP", "inbox_done": count(t, s, "SELECT count(*) FROM inbox WHERE status='done'"), "outbox_sent": count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'"), "reply": reply, "artifact": "workspace/notes/dingtalk-live.md"}
	data, _ := json.MarshalIndent(report, "", "  ")
	if e = atomicWrite(filepath.Join(root, "report.json"), data, 0600); e != nil {
		t.Fatal(e)
	}
	t.Log("real-model / simulated DingTalk transport report:", filepath.Join(root, "report.json"))
}

func TestDingTalkApprovalContinuation(t *testing.T) {
	for _, decision := range []string{"approve", "reject"} {
		t.Run(decision, func(t *testing.T) {
			var writes atomic.Int32
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { writes.Add(1); fmt.Fprint(w, `{"receipt":"done"}`) }))
			defer endpoint.Close()
			c := dtConfig()
			c.Actions = map[string]Operation{"test_write": {Mode: "ask", URL: endpoint.URL, Method: "POST"}}
			model := modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
				if strings.Contains(m[len(m)-1].Content, "Original context:") {
					return Message{Role: "assistant", Content: "APPROVAL_RESULT"}, nil
				}
				return call("bash", `{"action":"test_write","arguments":{"value":1}}`), nil
			})
			s := openTest(t, c, model)
			s.acceptDingTalk(context.Background(), dtFrame(dtMessage()))
			s.mu.Lock()
			batch, e := s.claim(context.Background())
			s.mu.Unlock()
			// Initial debounce may not yet have expired.
			if e != nil {
				t.Fatal(e)
			}
			if len(batch) == 0 {
				time.Sleep(30 * time.Millisecond)
				s.mu.Lock()
				batch, e = s.claim(context.Background())
				s.mu.Unlock()
			}
			if e != nil || len(batch) != 1 {
				t.Fatalf("claim %v %v", batch, e)
			}
			out, e := s.execute(context.Background(), batch)
			if e != nil || out.Approval == nil {
				t.Fatal("expected pending approval", e)
			}
			if e = s.commitBatch(context.Background(), batch, out); e != nil {
				t.Fatal(e)
			}
			if writes.Load() != 0 || count(t, s, "SELECT count(*) FROM outbox WHERE target='dingtalk:1'") != 1 {
				t.Fatal("approval bypassed or notice misrouted")
			}
			if e = s.Decide(context.Background(), out.Approval.ID, decision); e != nil {
				t.Fatal(e)
			}
			s.mu.Lock()
			batch, e = s.claim(context.Background())
			s.mu.Unlock()
			if e != nil || len(batch) != 1 {
				t.Fatal("approval not resumed", e)
			}
			out, e = s.execute(context.Background(), batch)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.commitBatch(context.Background(), batch, out); e != nil {
				t.Fatal(e)
			}
			want := int32(0)
			if decision == "approve" {
				want = 1
			}
			if writes.Load() != want || count(t, s, "SELECT count(*) FROM outbox WHERE target='dingtalk:1' AND content='APPROVAL_RESULT'") != 1 {
				t.Fatal("continuation result misrouted or wrong action count")
			}
		})
	}
}
func TestDingTalkRestartAndExplicitSend(t *testing.T) {
	root := t.TempDir()
	c := dtConfig()
	s, e := Open(root, c, notifyModel())
	if e != nil {
		t.Fatal(e)
	}
	m := dtMessage()
	s.acceptDingTalk(context.Background(), dtFrame(m))
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, c, notifyModel())
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	s.acceptDingTalk(context.Background(), dtFrame(m))
	if count(t, s, "SELECT count(*) FROM inbox") != 1 {
		t.Fatal("lost dedup across restart")
	}
	var calls atomic.Int32
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); fmt.Fprint(w, `{"errcode":0}`) }))
	defer receiver.Close()
	routeDTToLocal(s, receiver)
	startDTWorkers(s)
	waitFor(t, 3*time.Second, func() bool { return count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'") == 1 })
	if count(t, s, "SELECT count(*) FROM outbox") != 1 || calls.Load() != 1 {
		t.Fatal("explicit send plus final reply duplicated")
	}
}
func TestDingTalkExecutionFailureNotice(t *testing.T) {
	s := openTest(t, dtConfig(), modelFunc(func(context.Context, []Message, []Tool) (Message, error) {
		return Message{}, errors.New("model unavailable")
	}))
	s.acceptDingTalk(context.Background(), dtFrame(dtMessage()))
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"errcode":0}`) }))
	defer receiver.Close()
	routeDTToLocal(s, receiver)
	startDTWorkers(s)
	waitFor(t, 3*time.Second, func() bool {
		return count(t, s, "SELECT count(*) FROM outbox WHERE status='sent' AND dedup_key='failure:1'") == 1
	})
	if count(t, s, "SELECT count(*) FROM inbox WHERE status='dead' AND retries=3") != 1 {
		t.Fatal("missing dead event")
	}
}
func TestDingTalkEndpointValidation(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	for _, endpoint := range []string{"ws://127.0.0.1/socket", "wss://attacker.invalid/socket", "wss://wss-open-connection.dingtalk.com:8000/socket"} {
		s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != "https://api.dingtalk.com/v1.0/gateway/connections/open" {
				t.Error("wrong endpoint API")
			}
			var body map[string]any
			if json.NewDecoder(r.Body).Decode(&body) != nil || body["clientId"] != "test-app" || body["clientSecret"] != "test-client-secret" {
				t.Error("wrong auth body")
			}
			b, _ := json.Marshal(map[string]string{"endpoint": endpoint, "ticket": "secret-ticket"})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(b)))}, nil
		})
		conn, e := s.dingTalkConnect(context.Background())
		if e == nil || conn != nil || strings.Contains(e.Error(), "secret-ticket") {
			t.Fatal("untrusted endpoint accepted or ticket exposed")
		}
	}
}

func TestDingTalkProgressBeforeCompletion(t *testing.T) {
	cfg := dtConfig()
	cfg.DingTalk.ProgressMessages = true
	cfg.DeliveryIntervalMS = 0
	finish := make(chan struct{})
	model := modelFunc(func(ctx context.Context, _ []Message, _ []Tool) (Message, error) {
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-finish:
			return Message{Role: "assistant", Content: "最终结果"}, nil
		}
	})
	s := openTest(t, cfg, model)
	delivered := make(chan string, 4)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct{ Text struct{ Content string } }
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		delivered <- payload.Text.Content
		fmt.Fprint(w, `{"errcode":0}`)
	}))
	defer receiver.Close()
	routeDTToLocal(s, receiver)
	peer := openDTStream(t, s)
	m := dtMessage()
	sendDT(t, s, peer, m)
	sendDT(t, s, peer, m)
	startDTWorkers(s)
	expect := func(want string) {
		t.Helper()
		select {
		case got := <-delivered:
			if got != want {
				t.Fatalf("got %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("missing delivery", want)
		}
	}
	select {
	case got := <-delivered:
		if !slices.Contains(dingTalkProgressPhrases[:], got) {
			t.Fatalf("unexpected progress message: %q", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing progress message")
	}
	if count(t, s, "SELECT count(*) FROM inbox WHERE status='running'") != 1 {
		t.Fatal("expected running event")
	}
	close(finish)
	expect("最终结果")
	waitFor(t, time.Second, func() bool { return count(t, s, "SELECT count(*) FROM outbox WHERE status='sent'") == 2 })
	// Simulate an outstanding 429 retry reaching the dispatcher after the final reply.
	_, err := s.db.Exec("UPDATE outbox SET status='pending',deliver_at=0 WHERE dedup_key='progress:1'")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool { return count(t, s, "SELECT count(*) FROM outbox WHERE dedup_key='progress:1'") == 0 })
	if len(delivered) != 0 {
		t.Fatal("stale progress was delivered")
	}
}

func TestDingTalkOwnerInNewGroup(t *testing.T) {
	cfg := dtConfig()
	cfg.DingTalk.AllowAllGroups = true
	cfg.DingTalk.AllowedConversations = []string{"owner-private-chat"}
	s := openTest(t, cfg, finalModel())
	m := dtMessage()
	m.ConversationID = "new-group"
	ctx := context.Background()
	// A new chat ID is eligible only for group callbacks from the owner with a mention.
	for _, tc := range []struct {
		kind, sender string
		mention      bool
	}{
		{"1", "owner", true}, {"2", "stranger", true}, {"2", "owner", false},
	} {
		m.ConversationType, m.Sender, m.Mentioned = tc.kind, tc.sender, tc.mention
		s.acceptDingTalk(ctx, dtFrame(m))
		if count(t, s, "SELECT count(*) FROM inbox") != 0 {
			t.Fatal("unexpected admission", tc)
		}
	}
	m.ConversationType, m.Sender, m.Mentioned = "2", "owner", true
	s.acceptDingTalk(ctx, dtFrame(m))
	route, err := s.dingTalkRoute(ctx, s.read, "dingtalk:1")
	if err != nil || route.ConversationID != "new-group" {
		t.Fatalf("new group not routable: %v", err)
	}
}

func TestScheduledOwnerNewsReceipt(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	var authCalls, sendCalls int
	var limited bool
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body string
		switch r.URL.Path {
		case "/v1.0/oauth2/accessToken":
			authCalls++
			body = `{"accessToken":"fake-test-token","expireIn":7200}`
		case "/v1.0/robot/oToMessages/batchSend":
			sendCalls++
			var req struct {
				RobotCode        string
				UserIds          []string
				MsgKey, MsgParam string
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RobotCode != s.cfg.DingTalk.ClientID || len(req.UserIds) != 1 || req.UserIds[0] != s.cfg.DingTalk.AllowedSender || req.MsgKey != "sampleText" || r.Header.Get("x-acs-dingtalk-access-token") != "fake-test-token" {
				t.Fatal("incorrect proactive recipient or credentials")
			}
			body = `{"processQueryKey":"receipt"}`
			if limited {
				body = `{"flowControlledStaffIdList":["owner"]}`
			}
		default:
			t.Fatal("unexpected endpoint")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	n := Notification{Target: s.newsTarget(), Content: "AI news"}
	if got := s.deliverOwnerNews(context.Background(), n); got != "sent" {
		t.Fatal(got)
	}
	limited = true
	if got := s.deliverOwnerNews(context.Background(), n); got != "pending" {
		t.Fatal(got)
	}
	n.Target = "group"
	if s.deliverOwnerNews(context.Background(), n) != "failed" || authCalls != 1 || sendCalls != 2 {
		t.Fatal("recipient restriction or token cache failed")
	}
}

func TestDingTalkQuotedMessageReachesModel(t *testing.T) {
	cfg := dtConfig()
	cfg.Limits.DebounceMS = 0
	s := openTest(t, cfg, modelFunc(func(_ context.Context, m []Message, _ []Tool) (Message, error) {
		input := m[len(m)-1].Content
		for _, want := range []string{"介绍第三条", "第三条：引用测试文章"} {
			if !strings.Contains(input, want) {
				t.Fatalf("missing %q in model input", want)
			}
		}
		if strings.Contains(input, "private-download-code") {
			t.Fatal("raw quoted metadata leaked")
		}
		return Message{Role: "assistant", Content: "已读引用"}, nil
	}))
	f := dtFrame(dtMessage())
	var data map[string]any
	if err := json.Unmarshal([]byte(f.Data), &data); err != nil {
		t.Fatal(err)
	}
	data["text"] = map[string]any{"content": "介绍第三条", "isReplyMsg": true, "repliedMsg": map[string]any{"msgType": "text", "content": map[string]string{"text": "第三条：引用测试文章", "downloadCode": "private-download-code"}}}
	raw, _ := json.Marshal(data)
	f.Data = string(raw)
	ctx := context.Background()
	if s.acceptDingTalk(ctx, f).Code != 200 {
		t.Fatal("callback rejected")
	}
	batch, e := s.claim(ctx)
	if e != nil || len(batch) != 1 {
		t.Fatal("claim failed", e)
	}
	if _, e = s.execute(ctx, batch); e != nil {
		t.Fatal(e)
	}
}
