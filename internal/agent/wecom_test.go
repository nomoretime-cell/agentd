package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func wcConfig() Config {
	c := dtConfig() // Both credentials can be stored; only WeCom may connect.
	c.IM.Provider = "wecom"
	c.WeCom = WeComConfig{BotID: "bot", Secret: "secret-wc", Owner: "owner", AllowedGroups: []string{"room"}, GroupLeads: map[string]string{"room": "lead"}, TeamKnowledge: "团队代号海松"}
	return c
}
func wcMessage(id, chat, sender, text string) map[string]any {
	kind := "group"
	if chat == "" {
		kind = "single"
	}
	return map[string]any{"msgid": id, "aibotid": "bot", "chatid": chat, "chattype": kind, "from": map[string]string{"userid": sender}, "msgtype": "text", "text": map[string]string{"content": text}}
}

type postedWeCom struct {
	Chat     string `json:"chatid"`
	Markdown struct {
		Content string `json:"content"`
	} `json:"markdown"`
}
type fakeWeCom struct {
	ready  chan *websocket.Conn
	posted chan postedWeCom
	mu     sync.Mutex
}

func (w *fakeWeCom) write(c *websocket.Conn, v any) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return c.WriteJSON(v)
}
func (w *fakeWeCom) serve(rw http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{}
	c, err := up.Upgrade(rw, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	var subscribed bool
	for {
		var f wecomFrame
		if c.ReadJSON(&f) != nil {
			return
		}
		code := 0
		switch f.Cmd {
		case "aibot_subscribe":
			var auth map[string]string
			if json.Unmarshal(f.Body, &auth) != nil || auth["bot_id"] != "bot" || auth["secret"] != "secret-wc" {
				code = 1
			}
		case "aibot_send_msg":
			var msg postedWeCom
			if !subscribed || json.Unmarshal(f.Body, &msg) != nil {
				return
			}
			w.posted <- msg
		case "ping":
		default:
			return
		}
		if w.write(c, map[string]any{"headers": f.Headers, "errcode": code}) != nil {
			return
		}
		if f.Cmd == "aibot_subscribe" && code == 0 {
			subscribed = true
			w.ready <- c
		}
	}
}
func (w *fakeWeCom) next(t *testing.T) postedWeCom {
	t.Helper()
	select {
	case p := <-w.posted:
		return p
	case <-time.After(4 * time.Second):
		t.Fatal("no WeCom send")
		return postedWeCom{}
	}
}

func TestWeComTeamEndToEnd(t *testing.T) {
	for _, all := range []bool{false, true} {
		t.Run(fmt.Sprintf("allow_all_groups=%t", all), func(t *testing.T) { testWeComTeam(t, all) })
	}
}

func testWeComTeam(t *testing.T, all bool) {
	w := fakeWeCom{ready: make(chan *websocket.Conn, 1), posted: make(chan postedWeCom, 16)}
	server := httptest.NewServer(http.HandlerFunc(w.serve))
	defer server.Close()
	c := wcConfig()
	c.WeCom.ProgressMessages = true
	c.WeCom.AllowAllGroups = all
	c.WeCom.GroupOwnerTools = all
	if all {
		c.WeCom.AllowedGroups = nil
	}
	if err := c.WeCom.validate(); err != nil {
		t.Fatal(err)
	}
	c.NewsFeeds = []NewsFeed{{Name: "TestFeed", Language: "zh", URL: "https://feed.example.test/rss"}}
	release := make(chan struct{})
	seen := make(chan string, 8)
	s := openTest(t, c, modelFunc(func(ctx context.Context, m []Message, tools []Tool) (Message, error) {
		prompt := m[0].Content
		for _, v := range m[1:] {
			prompt += "\n" + v.Content
		}
		if isTeamTurn(ctx) {
			if (len(tools) > 0) != c.WeCom.GroupOwnerTools || strings.Contains(prompt, "PRIVATE_MARKER") || !strings.Contains(prompt, "团队代号海松") {
				t.Error("team context/permissions")
			}
		} else if !strings.Contains(prompt, "PRIVATE_MARKER") || len(tools) == 0 {
			t.Error("owner context/permissions")
		}
		seen <- prompt
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-release:
		}
		return Message{Role: "assistant", Content: "测试结果 <@all>"}, nil
	}))
	if err := s.workspaceWrite("MEMORY.md", "PRIVATE_MARKER", digest([]byte("# Memory\n\nUser preferences and useful facts.\n"))); err != nil {
		t.Fatal(err)
	}
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != c.NewsFeeds[0].URL {
			return nil, errors.New("unexpected outbound URL")
		}
		body := fmt.Sprintf("<rss><channel><item><title>团队新闻</title><link>https://example.test/news</link><pubDate>%s</pubDate></item></channel></rss>", time.Now().UTC().Format(time.RFC1123Z))
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	s.wecomURL = strings.Replace(server.URL, "http", "ws", 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.Start(ctx)
	var conn *websocket.Conn
	select {
	case conn = <-w.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("subscription missing")
	}
	send := func(body map[string]any) {
		t.Helper()
		if err := w.write(conn, map[string]any{"cmd": "aibot_msg_callback", "headers": map[string]string{"req_id": randomID()}, "body": body}); err != nil {
			t.Fatal(err)
		}
	}
	first := wcMessage("one", "room", "amy", "问题一")
	first["quote"] = map[string]any{"msgtype": "text", "text": map[string]string{"content": "引用内容"}}
	if !all {
		send(wcMessage("ignored", "other", "amy", "wrong group"))
	}
	send(wcMessage("ignored-dm", "", "amy", "not owner"))
	send(first)
	send(first)
	if p := w.next(t); p.Chat != "room" || !slices.Contains(imProgressPhrases[:], p.Markdown.Content) {
		t.Fatal("progress", p)
	}
	close(release)
	final := func(lead, chat string) {
		t.Helper()
		for {
			p := w.next(t)
			if p.Chat != chat {
				t.Fatal("wrong chat", p.Chat)
			}
			if strings.Contains(p.Markdown.Content, "测试结果") {
				if strings.Contains(p.Markdown.Content, "<@all>") || (lead != "" && !strings.Contains(p.Markdown.Content, "<@"+lead+">")) {
					t.Fatal("mention", p)
				}
				return
			}
		}
	}
	final("lead", "room")
	waitFor(t, time.Second, func() bool {
		var n int
		s.read.QueryRow("SELECT count(*) FROM outbox WHERE status='sent' AND dedup_key LIKE 'reply:%'").Scan(&n)
		return n == 1
	})
	if prompt := <-seen; !strings.Contains(prompt, "引用内容") {
		t.Fatal("missing quote")
	}
	send(wcMessage("two", "room", "amy", "追问"))
	final("lead", "room")
	if prompt := <-seen; !strings.Contains(prompt, "引用内容") || !strings.Contains(prompt, "测试结果") {
		t.Fatal("lost follow-up history")
	}
	send(wcMessage("three", "room", "owner", "/提醒 bob 独立的问题"))
	final("bob", "room")
	if prompt := <-seen; strings.Contains(prompt, "引用内容") {
		t.Fatal("different member received history")
	}
	send(wcMessage("four", "", "owner", "私人消息"))
	final("", "owner")
	<-seen
	var n int
	want := 4
	if all {
		send(wcMessage("five", "new-room", "new-member", "新群提问"))
		final("", "new-room")
		<-seen
		want++
	}
	if err := s.db.QueryRow("SELECT count(*) FROM inbox").Scan(&n); err != nil || n != want {
		t.Fatalf("inbox count %d err %v", n, err)
	}
	api := httptest.NewServer(s.Handler())
	defer api.Close()
	req, _ := http.NewRequest("GET", api.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer "+s.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status struct {
		WeCom, DingTalk struct{ Enabled, Connected bool }
	}
	if json.NewDecoder(resp.Body).Decode(&status) != nil || !status.WeCom.Connected || status.DingTalk.Enabled {
		t.Fatal(status)
	}
	// The deployment smoke endpoint must send to the selected owner's DM.
	for range 2 {
		req, _ := http.NewRequest("POST", api.URL+"/v1/news/test", strings.NewReader(`{"source":"TestFeed","key":"release-1"}`))
		req.Header.Set("Authorization", "Bearer "+s.token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatal(resp.Status)
		}
	}
	p := w.next(t)
	if p.Chat != "owner" || !strings.Contains(p.Markdown.Content, "【新闻推送测试】") || !strings.Contains(p.Markdown.Content, "团队新闻") {
		t.Fatal(p)
	}
	waitFor(t, time.Second, func() bool {
		var count int
		s.read.QueryRow("SELECT COUNT(*) FROM outbox WHERE dedup_key LIKE 'ai-news:test:%' AND status='sent'").Scan(&count)
		return count == 1
	})

}

func TestLiveWeComTeamOpenCode(t *testing.T) {
	if os.Getenv("AGENTD_WECOM_OC_LIVE") != "1" {
		t.Skip("requires local model and OpenCode")
	}
	project, _ := filepath.Abs("../..")
	cfg, err := LoadConfig(filepath.Join(project, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.IM.Provider = "wecom"
	cfg.WeCom = wcConfig().WeCom
	cfg.WeCom.GroupOwnerTools = true
	cfg.OpenCodeBinary = filepath.Join(project, ".tools/opencode")
	cfg.Limits.DebounceMS = 0
	s := openTest(t, cfg, nil)
	var logs bytes.Buffer
	s.log = slog.New(slog.NewJSONHandler(&logs, nil))
	for i, q := range []string{"本轮记住我的暗号青梅708。请只回复已记录。", "上一轮我给你的暗号是什么？团队代号又是什么？请只列这两项。", "请使用 agentd_write_file 创建 notes/team-check.md，expected_hash 为 absent，正文只写 apricot-614。", "请使用 websearch 搜索 Go programming language official，给出官网来源链接。"} {
		ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		raw, _ := json.Marshal(wcMessage(q, "room", "member", q))
		if err = s.acceptWeCom(ctx, raw); err != nil {
			cancel()
			t.Fatal(err)
		}
		batch, err := s.claim(ctx)
		if err != nil || len(batch) != 1 {
			cancel()
			t.Fatal(err)
		}
		out, err := s.execute(ctx, batch)
		if err == nil {
			err = s.commitBatch(ctx, batch, out)
		}
		cancel()
		if err != nil || len(out.Notifications) != 1 {
			t.Fatal(i, err)
		}
		if i == 1 && !(strings.Contains(out.Notifications[0].Content, "青梅708") && strings.Contains(out.Notifications[0].Content, "海松")) {
			t.Fatal(out.Notifications[0].Content)
		}
		if i == 2 {
			b, err := s.workspaceRead("notes/team-check.md")
			if err != nil || !strings.Contains(string(b), "apricot-614") {
				t.Fatal("group write missing", err)
			}
		}
		if i == 3 && !(strings.Contains(out.Notifications[0].Content, "go.dev") && strings.Contains(logs.String(), `"tool":"websearch","status":"completed"`)) {
			t.Fatal("group search missing", out.Notifications[0].Content)
		}
	}
}

func TestWeComTeamToolBoundaryAndCutover(t *testing.T) {
	s := openTest(t, wcConfig(), finalModel())
	ctx := context.WithValue(context.Background(), teamTurnKey{}, true)
	for _, name := range []string{"read_file", "bash", "write_file", "edit_file", "send_message", "set_timer"} {
		_, _, err := s.tool(ctx, "event", ToolCall{Function: FunctionCall{Name: name, Arguments: "{}"}}, "", &Outcome{})
		if !errors.Is(err, ErrDenied) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at) VALUES('old',?,0)", `{"kind":"dingtalk","text":"old"}`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES('old','dingtalk:1','old',0)"); err != nil {
		t.Fatal(err)
	}
	batch, err := s.claim(ctx)
	if err != nil || len(batch) != 0 {
		t.Fatal(batch, err)
	}
	if err = s.deliverOne(ctx); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = s.read.QueryRow("SELECT status FROM outbox WHERE dedup_key='old'").Scan(&status); err != nil || status != "pending" {
		t.Fatal(status, err)
	}
	if _, err = s.imRoute(ctx, s.read, "dingtalk:1"); !errors.Is(err, ErrDenied) {
		t.Fatal("inactive route accepted", err)
	}
}

func TestWeComUnacknowledgedSendIsUnknown(t *testing.T) {
	up := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var f wecomFrame
		if conn.ReadJSON(&f) != nil || conn.WriteJSON(map[string]any{"headers": f.Headers, "errcode": 0}) != nil {
			return
		}
		// Simulate a platform that consumed the notification but lost its reply.
		_ = conn.ReadJSON(&f)
	}))
	defer server.Close()
	s := openTest(t, wcConfig(), finalModel())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); s.serveWeCom(ctx, conn) }()
	defer func() { cancel(); <-stopped }()
	waitFor(t, time.Second, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return s.wecomConn != nil })
	_, err = s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at) VALUES('ai-news:test:drop',?,'test',0)", s.newsTarget())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.deliverOne(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var status string
	var retries int
	if err = s.read.QueryRow("SELECT status,retries FROM outbox").Scan(&status, &retries); err != nil || status != "unknown" || retries != 0 {
		t.Fatal(status, retries, err)
	}
}
