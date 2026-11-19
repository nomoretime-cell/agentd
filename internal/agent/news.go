package agent

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"time"
)

const newsTimerID = "ai-news-private"

type newsItem struct {
	Title, Link string
	Date        time.Time
	Source      string
}

func (s *Service) readNews(ctx context.Context, source, feed string) ([]newsItem, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, feed, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("RSS HTTP %d", resp.StatusCode)
	}
	var rss struct {
		Items []struct {
			Title string `xml:"title"`
			Link  string `xml:"link"`
			Date  string `xml:"pubDate"`
		} `xml:"channel>item"`
	}
	if err = xml.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rss); err != nil {
		return nil, err
	}
	now := time.Now()
	var items []newsItem
	for _, i := range rss.Items {
		d, err := mail.ParseDate(i.Date)
		u, e := url.Parse(i.Link)
		if err != nil || d.After(now) || e != nil || u.Scheme != "https" || u.Host == "" || strings.TrimSpace(i.Title) == "" {
			continue
		}
		items = append(items, newsItem{strings.Join(strings.Fields(i.Title), " "), i.Link, d, source})
	}
	return items, nil
}

func (s *Service) aiNews(ctx context.Context, ev Event) (Outcome, error) {
	var out Outcome
	if !s.canSendNews() {
		return out, nil
	}
	feeds := map[string]string{"量子位": "https://www.qbitai.com/feed", "TechCrunch AI": "https://techcrunch.com/category/artificial-intelligence/feed/", "Google AI": "https://blog.google/technology/ai/rss/"}
	results := make(chan []newsItem, len(feeds))
	for source, feed := range feeds {
		go func() {
			items, err := s.readNews(ctx, source, feed)
			if err != nil {
				s.log.Warn("AI news feed unavailable", "source", source)
			}
			results <- items
		}()
	}
	var items []newsItem
	for range feeds {
		items = append(items, <-results...)
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return s.newsDigest(ctx, ev, items)
}

// The existing Outbox is the durable article history, including digests sent
// before incremental delivery was enabled. Pending and uncertain deliveries
// reserve their links so a second timer tick cannot queue them again.
func (s *Service) newsDigest(ctx context.Context, ev Event, items []newsItem) (Outcome, error) {
	var out Outcome
	rows, err := s.read.QueryContext(ctx, `SELECT deliver_at,content,dedup_key FROM outbox
 WHERE target=? AND dedup_key GLOB 'ai-news:*' AND status IN ('pending','sending','sent','unknown')`, s.newsTarget())
	if err != nil {
		return out, err
	}
	defer rows.Close()
	seen := map[string]bool{}
	var baseline int64
	for rows.Next() {
		var at int64
		var content, notificationKey string
		if err := rows.Scan(&at, &content, &notificationKey); err != nil {
			return out, err
		}
		if _, hash, ok := strings.Cut(notificationKey, ":long-url:"); ok {
			seen[hash] = true
		}
		if baseline == 0 || at < baseline {
			baseline = at
		}
		for _, line := range strings.Split(content, "\n") {
			_, link, ok := strings.Cut(line, " · https://")
			if ok {
				seen[stableID(newsLinkKey("https://"+strings.TrimSpace(link)))] = true
			}
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Date.Equal(items[j].Date) {
			return items[i].Link < items[j].Link
		}
		return items[i].Date.After(items[j].Date)
	})
	if len(items) == 0 {
		s.log.Warn("AI news: no verifiable headlines this interval")
		return out, nil
	}
	var b strings.Builder
	loc, _ := time.LoadLocation(s.cfg.Timezone)
	header := fmt.Sprintf("AI 资讯 · %s\n", time.Now().In(loc).Format("01-02 15:04"))
	limit := min(dingTalkTextLimit, s.cfg.Limits.MaxOutputBytes)
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out.Notifications = append(out.Notifications, Notification{Key: fmt.Sprintf("ai-news:%d:part:%d", ev.ID, len(out.Notifications)+1), Target: s.newsTarget(), Content: header + b.String()})
		b.Reset()
	}
	for _, i := range items {
		hash := stableID(newsLinkKey(i.Link))
		if seen[hash] || (baseline != 0 && i.Date.UnixMilli() < baseline) {
			continue
		}
		seen[hash] = true
		prefix := "\n" + i.Source + "｜"
		suffix := "\n" + i.Date.In(loc).Format("01-02 15:04") + " · " + i.Link + "\n"
		titleBudget := limit - len(header) - len(prefix) - len(suffix)
		if titleBudget < len("…") {
			// A URL alone can exceed the channel limit. Preserve article identity
			// in the Outbox key so its headline is still sent exactly once locally.
			flush()
			content := header + prefix + i.Title + "\n（原文链接过长）"
			out.Notifications = append(out.Notifications, Notification{Key: fmt.Sprintf("ai-news:%d:long-url:%s", ev.ID, hash), Target: s.newsTarget(), Content: clipHeadline(content, limit)})
			continue
		}
		entry := prefix + clipHeadline(i.Title, titleBudget) + suffix
		if len(header)+b.Len()+len(entry) > limit {
			flush()
		}
		b.WriteString(entry)
	}
	flush()
	return out, nil
}
func (s *Service) newsTarget() string { return "dingtalk-owner:" + s.cfg.DingTalk.AllowedSender }
func (s *Service) canSendNews() bool {
	return s.cfg.DingTalk.Enabled && !s.cfg.DingTalk.DiscoveryOnly && s.cfg.DingTalk.AllowedSender != "" && s.cfg.Permissions["send_message"] == "allow"
}

func newsLinkKey(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Fragment = ""
	u.Host = strings.ToLower(u.Host)
	q := u.Query()
	for key := range q {
		if strings.HasPrefix(strings.ToLower(key), "utm_") {
			q.Del(key)
		}
	}
	u.RawQuery = q.Encode()
	return u.String()
}

func clipHeadline(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	return strings.ToValidUTF8(text[:limit-len("…")], "") + "…"
}
