package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestIncrementalNewsMigratesExistingDigest(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	ctx := context.Background()
	at := time.Now().Truncate(time.Second)
	old := "https://example.com/update"
	prior := "AI 资讯 · 11-20 00:24\n\n来源｜旧标题\n11-20 00:19 · " + old + "?utm_source=a\n"
	_, e := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('ai-news:10',?,?,?,'sent')", s.ownerTarget(), prior, at.UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	items := []newsItem{{"旧文换标题", old + "#summary", at.Add(time.Minute), "来源", false, false}, {"以前未上榜", "https://example.com/archive", at.Add(-time.Hour), "来源", false, false}}
	digest := func() Outcome {
		t.Helper()
		out, e := s.newsDigest(ctx, Event{ID: 11}, items)
		if e != nil {
			t.Fatal(e)
		}
		return out
	}
	if len(digest().Notifications) != 0 {
		t.Fatal("unchanged cycle queued a message")
	}
	items = append(items, newsItem{"新文章", "https://example.com/new", at.Add(time.Hour), "来源", false, false})
	out := digest()
	if len(out.Notifications) != 1 || !strings.Contains(out.Notifications[0].Content, "新文章") || strings.Contains(out.Notifications[0].Content, old) {
		t.Fatal(out)
	}
	_, e = s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('ai-news:11',?,?,?,'unknown')", s.ownerTarget(), out.Notifications[0].Content, at.Add(time.Hour).UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	if len(digest().Notifications) != 0 {
		t.Fatal("possibly delivered article queued again")
	}
}

func TestNewsDigestAllItemsAcrossParts(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	at := time.Now().Add(time.Hour)
	items := []newsItem{}
	for i := 0; i < 15; i++ {
		items = append(items, newsItem{strings.Repeat("新闻", 120), fmt.Sprintf("https://example.com/article/%02d", i), at, "来源", false, false})
	}
	items = append(items, newsItem{"超长网址", "https://example.com/" + strings.Repeat("a", 4050), at, "来源", false, false})
	digest := func(id int64) Outcome {
		out, err := s.newsDigest(context.Background(), Event{ID: id}, items)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	parts := digest(12).Notifications
	if len(parts) < 3 {
		t.Fatalf("expected multiple parts, got %d", len(parts))
	}
	var all strings.Builder
	for _, n := range parts {
		if len(n.Content) > imTextLimit || !utf8.ValidString(n.Content) {
			t.Fatal("invalid message part")
		}
		all.WriteString(n.Content)
		_, err := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES(?,?,?,?,'sent')", n.Key, n.Target, n.Content, time.Now().UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range items {
		if i.Title == "超长网址" {
			continue
		}
		if strings.Count(all.String(), i.Link+"\n") != 1 {
			t.Fatal("missing or duplicated article", i.Link)
		}
	}
	if !strings.Contains(all.String(), "超长网址") || len(digest(13).Notifications) != 0 {
		t.Fatal("incomplete digest or duplicate on next poll")
	}
}

func TestNewsTranslateOnlyFreshAndRetryMissing(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	cfg := dtConfig()
	calls := 0
	model := modelFunc(func(_ context.Context, messages []Message, tools []Tool) (Message, error) {
		calls++
		if len(tools) != 0 {
			t.Fatal("unexpected tools")
		}
		var titles map[string]string
		if err := json.Unmarshal([]byte(messages[1].Content), &titles); err != nil {
			t.Fatal(err)
		}
		out := make(map[string]string)
		for key, title := range titles {
			switch title {
			case "A model update":
				if calls > 1 {
					t.Fatal("translated an already queued article")
				}
				out[key] = "模型更新"
			case "Un modèle ouvert":
				if calls > 1 {
					out[key] = "一个开放模型"
				}
			default:
				t.Fatalf("unexpected translation input %q", title)
			}
		}
		data, _ := json.Marshal(out)
		return Message{Content: string(data)}, nil
	})
	s := openTest(t, cfg, model)
	items := []newsItem{
		{"中文新闻", "https://example.org/zh", now, "中文", false, false},
		{"A model update", "https://example.org/en", now, "英语", true, false},
		{"Un modèle ouvert", "https://example.org/fr", now, "法语", true, false},
		{"Old news", "https://example.org/old", now.Add(-24 * time.Hour), "英语", true, false},
	}
	for id := 1; id <= 3; id++ {
		out, err := s.newsDigest(ctx, Event{ID: int64(id)}, items)
		if err != nil {
			t.Fatal(err)
		}
		if id == 3 {
			if len(out.Notifications) != 0 || calls != 2 {
				t.Fatal("unchanged poll was not silent")
			}
			break
		}
		if len(out.Notifications) != 1 {
			t.Fatal(out)
		}
		n := out.Notifications[0]
		if id == 1 && (!strings.Contains(n.Content, "中文新闻") || !strings.Contains(n.Content, "模型更新") || strings.Contains(n.Content, "example.org/fr")) {
			t.Fatal(n.Content)
		}
		if id == 2 && (!strings.Contains(n.Content, "一个开放模型") || strings.Contains(n.Content, "example.org/en")) {
			t.Fatal(n.Content)
		}
		if _, err := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES(?,?,?,?,'sent')", n.Key, n.Target, n.Content, now.Add(time.Minute).UnixMilli()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLiveNewsSourcesAndTranslation(t *testing.T) {
	if os.Getenv("AGENTD_NEWS_LIVE") != "1" {
		t.Skip("requires RSS access and local model")
	}
	cfg, err := LoadConfig("../../config.local.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg.OpenCodeBinary = ""
	s := openTest(t, cfg, NewHTTPModel(cfg.Model))
	var sample []newsItem
	for _, feed := range cfg.NewsFeeds {
		items, err := s.readNews(context.Background(), feed)
		if err != nil || len(items) == 0 {
			t.Errorf("%s: %d usable articles, error %v", feed.Name, len(items), err)
			continue
		}
		t.Logf("%s: %d usable articles", feed.Name, len(items))
		if feed.URL == anthropicNewsroom || feed.Name == "TechCrunch AI" || feed.Language == "fr" {
			sample = append(sample, items[0])
		}
	}
	if len(sample) < 2 {
		t.Fatal("missing multilingual sample")
	}
	err = s.translateHeadlines(context.Background(), sample)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range sample {
		if i.Translate || !chineseHeadline(i.Title) {
			t.Fatalf("translation missing for %s", i.Source)
		}
		t.Logf("%s: %s", i.Source, i.Title)
	}
}

func TestAnthropicNewsroomListing(t *testing.T) {
	const row = `<ul><li data-listing-row="1"><a href="/news/announcements">Announcements</a>
	<a href="/claude-release"><h3>Claude &amp; <em>tools</em></h3></a><time>May 5, 2025</time>
	<a href="/claude-release"><h3>Expanded preview</h3></a></li>
	<li data-listing-row="2"><a href="//example.org/news"><h3>External</h3></a><time>May 5, 2025</time></li></ul>`
	items, err := readAnthropicNews(strings.NewReader(row), "Anthropic")
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	item := items[0]
	if item.Title != "Claude & tools" || item.Link != "https://www.anthropic.com/claude-release" || item.Date.Format(time.DateOnly) != "2025-05-05" || !item.DateOnly || !item.Translate {
		t.Fatal(item)
	}
	if _, err := readAnthropicNews(strings.NewReader("<html>Unavailable</html>"), "Anthropic"); err == nil {
		t.Fatal("empty listing accepted")
	}
}

func TestNewsSampleDoesNotMoveDigestBaseline(t *testing.T) {
	s := openTest(t, dtConfig(), finalModel())
	now := time.Now()
	const sampleURL = "https://example.org/sample"
	_, err := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES(?,?,?,?,?)",
		"ai-news:test:5", s.ownerTarget(), "【新闻推送测试】来源\n样例\n2026-01-01 · "+sampleURL, now.UnixMilli(), "sent")
	if err != nil {
		t.Fatal(err)
	}
	items := []newsItem{
		{Title: "已经测试", Link: sampleURL, Source: "来源", Date: now},
		{Title: "同轮新闻", Link: "https://example.org/new", Source: "来源", Date: now.Add(-time.Minute)},
	}
	out, err := s.newsDigest(context.Background(), Event{ID: 6, AvailableAt: now.UnixMilli()}, items)
	if err != nil || len(out.Notifications) != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	body := out.Notifications[0].Content
	if strings.Contains(body, sampleURL) || !strings.Contains(body, "同轮新闻") {
		t.Fatal(body)
	}
}
