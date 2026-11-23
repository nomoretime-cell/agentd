package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLiveOpenCodeChat(t *testing.T) {
	if os.Getenv("AGENTD_OC_LIVE") != "1" {
		t.Skip("requires OpenCode and local model")
	}
	project, _ := filepath.Abs("../..")
	c, e := LoadConfig(filepath.Join(project, "config.yaml"))
	if e != nil {
		t.Fatal(e)
	}
	c.DingTalk = dtConfig().DingTalk
	c.OpenCodeBinary = filepath.Join(project, ".tools/opencode")
	c.Limits.DebounceMS = 0
	root := t.TempDir()
	prompts := []string{"请用 agentd_write_file 创建 notes/check.md，expected_hash=absent，文件内容为 bluebird-915。记住代号青柠。完成后只回复已记录。", "上一轮的代号是什么？请实际使用 websearch 搜索 Go programming language official，并给出官网链接和代号。"}
	for i, p := range prompts {
		func() {
			s, e := Open(root, c, nil)
			if e != nil {
				t.Fatal(e)
			}
			defer s.Close()
			var logs bytes.Buffer
			s.log = slog.New(slog.NewJSONHandler(&logs, nil))
			m := dtMessage()
			m.MsgID = p
			m.Text.Content = p
			s.acceptDingTalk(context.Background(), dtFrame(m))
			batch, e := s.claim(context.Background())
			if e != nil || len(batch) != 1 {
				t.Fatalf("claim: %v %v", batch, e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
			defer cancel()
			out, e := s.execute(ctx, batch)
			if e != nil {
				t.Fatal(e)
			}
			if e = s.commitBatch(ctx, batch, out); e != nil {
				t.Fatal(e)
			}
			if len(out.Notifications) == 0 {
				t.Fatal("missing reply")
			}
			reply := out.Notifications[0].Content
			t.Log("reply", reply)
			if i == 0 {
				b, e := s.workspaceRead("notes/check.md")
				if e != nil || !strings.Contains(string(b), "bluebird-915") {
					t.Fatal("missing tool effect", e)
				}
			} else if !strings.Contains(reply, "青柠") || !strings.Contains(reply, "http") {
				t.Fatal("lost context or missing link")
			} else {
				found := false
				for _, line := range strings.Split(logs.String(), "\n") {
					var entry map[string]any
					if json.Unmarshal([]byte(line), &entry) == nil && entry["tool"] == "websearch" && entry["status"] == "completed" {
						found = true
					}
				}
				if !found {
					t.Fatal("no successful websearch in OpenCode trace")
				}
			}
		}()
	}
}

func TestChatHistoryExistingConversations(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	ctx := context.Background()
	for _, m := range []struct{ conversation, text string }{{"room1", "first"}, {"room2", "private"}, {"room1", "second"}} {
		payload, _ := json.Marshal(Payload{Kind: "dingtalk", Text: m.text, imRoutes: imRoutes{DingTalk: &IMRoute{ConversationID: m.conversation, Sender: "owner", Webhook: "SECRET"}}})
		result, err := s.db.Exec("INSERT INTO inbox(dedup_key,payload,available_at,status) VALUES(?,?,0,'done')", m.text, string(payload))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		_, err = s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES(?,?,?,0,'sent')", m.text, dingTalkTarget(id), "reply "+m.text)
		if err != nil {
			t.Fatal(err)
		}
	}
	messages, err := s.chatHistory(ctx, &IMRoute{ConversationID: "room1", Sender: "owner"}, 3)
	if err != nil || len(messages) != 2 || messages[0].Content != "first" || messages[1].Content != "reply first" {
		t.Fatalf("unexpected history: %+v %v", messages, err)
	}
}

func TestOpenCodeReadFileReturnsMaskedPage(t *testing.T) {
	c := testConfig()
	c.Model.APIKey = `sample-"quoted"`
	s := openTest(t, c, finalModel())
	note := "before " + c.Model.APIKey + " after"
	if err := s.workspaceWrite("notes/check.md", note, "absent"); err != nil {
		t.Fatal(err)
	}
	turn := ocTurn{s: s, ctx: context.Background()}
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"path":"notes/check.md"}}}`))
	response := httptest.NewRecorder()
	turn.mcp(response, req)
	var frame struct {
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(response.Body.Bytes(), &frame); err != nil || response.Code != 200 || frame.Result.IsError || len(frame.Result.Content) != 1 {
		t.Fatal("invalid MCP result", err)
	}
	var page struct {
		Content    string
		NextOffset int `json:"next_offset"`
		SHA256     string
	}
	if err := json.Unmarshal([]byte(frame.Result.Content[0].Text), &page); err != nil {
		t.Fatal(err)
	}
	if page.Content != "before "+strings.Repeat("*", len(c.Model.APIKey))+" after" || page.NextOffset != len(note) || page.SHA256 != digest([]byte(note)) {
		t.Fatal("file masking changed surrounding text, cursor, or source hash")
	}
}
