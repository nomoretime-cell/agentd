package agent

import (
	"context"
	"fmt"
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
	_, e := s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('ai-news:10',?,?,?,'sent')", s.newsTarget(), prior, at.UnixMilli())
	if e != nil {
		t.Fatal(e)
	}
	items := []newsItem{{"旧文换标题", old + "#summary", at.Add(time.Minute), "来源"}, {"以前未上榜", "https://example.com/archive", at.Add(-time.Hour), "来源"}}
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
	items = append(items, newsItem{"新文章", "https://example.com/new", at.Add(time.Hour), "来源"})
	out := digest()
	if len(out.Notifications) != 1 || !strings.Contains(out.Notifications[0].Content, "新文章") || strings.Contains(out.Notifications[0].Content, old) {
		t.Fatal(out)
	}
	_, e = s.db.Exec("INSERT INTO outbox(dedup_key,target,content,deliver_at,status) VALUES('ai-news:11',?,?,?,'unknown')", s.newsTarget(), out.Notifications[0].Content, at.Add(time.Hour).UnixMilli())
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
		items = append(items, newsItem{strings.Repeat("新闻", 120), fmt.Sprintf("https://example.com/article/%02d", i), at, "来源"})
	}
	items = append(items, newsItem{"超长网址", "https://example.com/" + strings.Repeat("a", 4050), at, "来源"})
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
		if len(n.Content) > dingTalkTextLimit || !utf8.ValidString(n.Content) {
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
