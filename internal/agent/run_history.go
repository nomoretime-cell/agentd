package agent

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// The date and byte offset refer to a complete line in an append-only transcript.
// They identify the recorded attempt, including when an event was run more than once.
const historyWindow = 2 << 20

type historyInput struct {
	Event int64  `json:"event"`
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	Text  string `json:"text"`
}
type historyNotice struct {
	Key            string `json:"key"`
	Target         string `json:"target"`
	Content        string `json:"content"`
	DeliveryID     int64  `json:"delivery_id"`
	DeliveryStatus string `json:"delivery_status"`
}
type historyRecord struct {
	ReplyIDs []string     `json:"reply_ids"`
	Indexed  bool         `json:"indexed"`
	Channel  string       `json:"channel"`
	Evidence *runEvidence `json:"evidence"`
	Calls    []modelCall  `json:"calls"`
	Attempt  string       `json:"attempt"`
	runScope
	Type          string          `json:"type"`
	Time          string          `json:"ts"`
	Status        string          `json:"status"`
	Answer        string          `json:"answer"`
	Error         string          `json:"error"`
	Action        string          `json:"pending_action"`
	Inputs        []historyInput  `json:"inputs"`
	Notifications []historyNotice `json:"notifications"`
}
type historySummary struct {
	Channel string `json:"channel"`
	Active  bool   `json:"active"`
	runScope
	ID      string `json:"id"`
	Time    string `json:"ts"`
	Status  string `json:"status"`
	Excerpt string `json:"excerpt"`
}

func historyPosition(id string) (string, int64, error) {
	day, n, ok := strings.Cut(id, ".")
	_, dateErr := time.Parse(time.DateOnly, day)
	pos, err := strconv.ParseInt(n, 10, 64)
	if !ok || dateErr != nil || err != nil || pos < 0 {
		return "", 0, errors.New("invalid transcript position")
	}
	return day, pos, nil
}

func (s *Service) decodeHistory(line []byte) (historyRecord, error) {
	var rec historyRecord
	err := json.Unmarshal(line, &rec)
	if err != nil || rec.Type != "run" {
		return rec, err
	}
	rec.ReplyIDs = nil
	rec.Evidence = nil
	rec.Calls = nil // Receipts are loaded from the Inbox ledger for this attempt.
	rec.Answer, rec.Error, rec.Action = s.redact(rec.Answer), s.redact(rec.Error), s.redact(rec.Action)
	rec.Role, rec.Principal, rec.Conversation = s.redact(rec.Role), s.redact(rec.Principal), s.redact(rec.Conversation)
	if rec.Channel == "" {
		for _, in := range rec.Inputs {
			if imEventKind(in.Kind) {
				rec.Channel = in.Kind
				break
			}
			if in.Kind == "message" {
				rec.Channel = "local"
			}
		}
	}
	rec.Time, rec.Status = s.redact(rec.Time), s.redact(rec.Status)
	for i := range rec.Inputs {
		in := &rec.Inputs[i]
		in.Text, in.Kind, in.Name = s.redact(in.Text), s.redact(in.Kind), s.redact(in.Name)
	}
	for i := range rec.Notifications {
		n := &rec.Notifications[i]
		n.Target, n.Content = s.redact(n.Target), s.redact(n.Content)
		// Only the Outbox lookup below can establish a delivery receipt.
		n.DeliveryID, n.DeliveryStatus = 0, ""
	}
	return rec, nil
}

func (r historyRecord) excerpt(query string) (string, bool) {
	fields := []string{r.Answer}
	for _, in := range r.Inputs {
		fields = append(fields, in.Text, in.Name, in.Kind)
	}
	for _, n := range r.Notifications {
		fields = append(fields, n.Content)
	}
	fields = append(fields, r.Error)
	for _, field := range fields {
		if field != "" && strings.Contains(strings.ToLower(field), query) {
			runes := []rune(field)
			if len(runes) > 140 {
				return string(runes[:140]) + "…", true
			}
			return field, true
		}
	}
	return "", query == ""
}

type historyPage struct {
	Items      []historySummary `json:"items"`
	Next       string           `json:"next"`
	From       string           `json:"from"`
	To         string           `json:"to"`
	Timezone   string           `json:"timezone"`
	Incomplete bool             `json:"incomplete"`
}

func (s *Service) runHistory(ctx context.Context, q url.Values) (historyPage, error) {
	loc, err := time.LoadLocation(s.cfg.Timezone)
	page := historyPage{Items: []historySummary{}, Timezone: s.cfg.Timezone}
	if err != nil {
		return page, err
	}
	last := q.Get("to")
	if last == "" {
		last = time.Now().In(loc).Format(time.DateOnly)
	}
	to, err := time.Parse(time.DateOnly, last)
	if err != nil {
		return page, errors.New("invalid end date")
	}
	from := to.AddDate(0, 0, -6)
	if first := q.Get("from"); first != "" {
		from, err = time.Parse(time.DateOnly, first)
		if err != nil || from.After(to) || from.Before(to.AddDate(0, 0, -30)) {
			return page, errors.New("select 1 to 31 days")
		}
	}
	page.From, page.To = from.Format(time.DateOnly), last
	limit := 20
	cursor := q.Get("cursor")
	if cursor == "" || strings.HasPrefix(cursor, "db:") {
		items, next, err := s.indexedPage(ctx, q, from, to, loc)
		if err != nil {
			return page, err
		}
		page.Items = append(page.Items, items...)
		if next != "" {
			page.Next = next
			return page, nil
		}
		if len(items) == limit {
			page.Next = "archive"
			return page, nil
		}
		cursor = ""
	}
	if cursor == "archive" {
		cursor = ""
	}
	day, end := last, int64(-1)
	if cursor != "" {
		day, end, err = historyPosition(cursor)
		if err != nil || day < page.From || day > last {
			return page, errors.New("invalid history cursor")
		}
	}
	query := strings.ToLower(strings.TrimSpace(q.Get("q")))
	budget := int64(historyWindow)
	for date, _ := time.Parse(time.DateOnly, day); !date.Before(from); date = date.AddDate(0, 0, -1) {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		day = date.Format(time.DateOnly)
		f, size, err := s.openTranscript("sessions/"+day+".jsonl", 0)
		if errors.Is(err, os.ErrNotExist) {
			end = -1
			continue
		}
		if err != nil {
			return page, errors.New("cannot open run history")
		}
		if end < 0 {
			end = size
		}
		if end > size {
			f.Close()
			return page, errors.New("history changed; refresh the list")
		}
		if budget == 0 && end > 0 {
			f.Close()
			page.Next = fmt.Sprintf("%s.%d", day, end)
			return page, nil
		}
		start := max(int64(0), end-budget)
		// Include one preceding byte to distinguish a true line boundary.
		at := max(int64(0), start-1)
		buf := make([]byte, end-at)
		_, err = f.ReadAt(buf, at)
		f.Close()
		if err != nil {
			return page, errors.New("cannot read run history")
		}
		budget -= end - start
		first := int(start - at)
		if first > 0 && buf[first-1] != '\n' {
			newline := bytes.IndexByte(buf[first:], '\n')
			if newline < 0 {
				if end-start < historyWindow {
					page.Next = fmt.Sprintf("%s.%d", day, end)
					return page, nil
				}
				page.Incomplete = true
				page.Next = fmt.Sprintf("%s.%d", day, start)
				return page, nil
			}
			first += newline + 1
		}
		for hi := len(buf); hi > first; {
			if err := ctx.Err(); err != nil {
				return page, err
			}
			complete := buf[hi-1] == '\n'
			tail := hi
			if complete {
				tail--
			}
			lo := first + bytes.LastIndexByte(buf[first:tail], '\n') + 1
			line := buf[lo:tail]
			hi = lo
			if !complete {
				page.Incomplete = true
				continue
			}
			rec, err := s.decodeHistory(line)
			if err != nil {
				page.Incomplete = true
				continue
			}
			if rec.Type != "run" || rec.Indexed || !rec.matches(q) {
				continue
			}
			excerpt, ok := rec.excerpt(query)
			if !ok {
				continue
			}
			id := fmt.Sprintf("%s.%d", day, at+int64(lo))
			page.Items = append(page.Items, rec.summary(id, excerpt))
			if len(page.Items) == 20 {
				page.Next = id
				return page, nil
			}
		}
		if start > 0 {
			next := at + int64(first)
			if next == end && end-start == historyWindow {
				next = start
				page.Incomplete = true
			}
			page.Next = fmt.Sprintf("%s.%d", day, next)
			return page, nil
		}
		end = -1
	}
	return page, nil
}

func (s *Service) historicalRun(ctx context.Context, id string) (historyRecord, error) {
	if strings.Contains(id, "~") {
		return s.indexedRun(ctx, id)
	}
	var rec historyRecord
	day, pos, err := historyPosition(id)
	if err != nil {
		return rec, err
	}
	f, size, err := s.openTranscript("sessions/"+day+".jsonl", 0)
	if errors.Is(err, os.ErrNotExist) {
		return rec, sql.ErrNoRows
	}
	if err != nil {
		return rec, errors.New("cannot open run history")
	}
	defer f.Close()
	if pos >= size {
		return rec, sql.ErrNoRows
	}
	if pos > 0 {
		var prev [1]byte
		if _, err = f.ReadAt(prev[:], pos-1); err != nil || prev[0] != '\n' {
			return rec, sql.ErrNoRows
		}
	}
	line, err := bufio.NewReader(io.NewSectionReader(f, pos, historyWindow+1)).ReadBytes('\n')
	if err != nil || len(line) > historyWindow {
		return rec, sql.ErrNoRows
	}
	rec, err = s.decodeHistory(line)
	if err != nil || rec.Type != "run" {
		return rec, sql.ErrNoRows
	}
	return s.runReceipts(ctx, rec)
}
func (s *Service) runReceipts(ctx context.Context, rec historyRecord) (historyRecord, error) {
	var err error
	for i := range rec.Notifications {
		n := &rec.Notifications[i]
		var body, target string
		err = s.read.QueryRowContext(ctx, "SELECT id,status,content,target FROM outbox WHERE dedup_key=?", n.Key).Scan(&n.DeliveryID, &n.DeliveryStatus, &body, &target)
		if err == nil {
			n.Content, n.Target = s.redact(body), s.redact(target)
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return rec, err
		}
		n.Key = s.redact(n.Key)
	}
	if rec.Attempt != "" && len(rec.Inputs) > 0 {
		known := map[int64]bool{}
		for _, notice := range rec.Notifications {
			known[notice.DeliveryID] = true
		}
		inputs, _ := json.Marshal(rec.Inputs)
		rows, err := s.read.QueryContext(ctx, `SELECT id,dedup_key,target,content,status FROM outbox
		  WHERE origin_event IN (SELECT value->>'event' FROM json_each(?)) AND origin_attempt=? ORDER BY id`, string(inputs), rec.Attempt)
		if err != nil {
			return rec, err
		}
		for rows.Next() {
			var n historyNotice
			if err = rows.Scan(&n.DeliveryID, &n.Key, &n.Target, &n.Content, &n.DeliveryStatus); err != nil {
				rows.Close()
				return rec, err
			}
			if !known[n.DeliveryID] {
				n.Key, n.Target, n.Content = s.redact(n.Key), s.redact(n.Target), s.redact(n.Content)
				rec.Notifications = append(rec.Notifications, n)
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return rec, err
		}
	}
	if rec.Attempt != "" && len(rec.Inputs) > 0 {
		rec.Calls = []modelCall{}
		rows, err := s.read.QueryContext(ctx, `SELECT j.value FROM inbox i JOIN json_each(i.model_calls) j WHERE i.id=? AND j.value->>'attempt'=? ORDER BY j.value->>'at',j.key`, rec.Inputs[0].Event, rec.Attempt)
		if err != nil {
			return rec, err
		}
		defer rows.Close()
		for rows.Next() {
			var raw string
			var c modelCall
			if err = rows.Scan(&raw); err != nil {
				return rec, err
			}
			if err = json.Unmarshal([]byte(raw), &c); err != nil {
				return rec, err
			}
			c.Model = s.redact(c.Model)
			rec.Calls = append(rec.Calls, c)
		}
		if err = rows.Err(); err != nil {
			return rec, err
		}
	}
	err = s.runReplies(ctx, &rec)
	return rec, err
}

func (r historyRecord) summary(id, excerpt string) historySummary {
	active := false
	for _, in := range r.Inputs {
		if in.Kind == "message" || imEventKind(in.Kind) {
			active = true
			break
		}
	}
	return historySummary{r.Channel, active, r.runScope, id, r.Time, r.Status, excerpt}
}
func (r historyRecord) matches(q url.Values) bool {
	for key, val := range map[string]string{"channel": r.Channel, "role": r.Role, "principal": r.Principal, "conversation": r.Conversation, "status": r.Status, "attempt": r.Attempt} {
		if q.Has("match_"+key) && q.Get("match_"+key) != val {
			return false
		}
		if filter := q.Get(key); filter != "" && filter != val {
			return false
		}
	}
	if q.Get("active") == "1" && !r.summary("", "").Active {
		return false
	}
	if q.Get("flag") == "approval" && r.Action == "" {
		return false
	}
	if q.Get("flag") == "error" && r.Error == "" && r.Status != "failed" && r.Status != "commit_failed" && r.Status != "interrupted" {
		return false
	}
	if event := q.Get("event"); event != "" {
		for _, in := range r.Inputs {
			if strconv.FormatInt(in.Event, 10) == event {
				return true
			}
		}
		return false
	}
	return true
}
func (s *Service) indexedPage(ctx context.Context, q url.Values, from, to time.Time, loc *time.Location) ([]historySummary, string, error) {
	start, _ := time.ParseInLocation(time.DateOnly, from.Format(time.DateOnly), loc)
	stop, _ := time.ParseInLocation(time.DateOnly, to.Format(time.DateOnly), loc)
	stop = stop.AddDate(0, 0, 1)
	before, key := stop.UnixMilli(), ""
	if cursor := q.Get("cursor"); cursor != "" {
		stamp, part, ok := strings.Cut(strings.TrimPrefix(cursor, "db:"), ":")
		t, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil || len(part) != 48 {
			return nil, "", errors.New("invalid indexed cursor")
		}
		before, key = t, part
	}
	rows, err := s.read.QueryContext(ctx, `SELECT i.id,j.key,j.value->>'at',j.value FROM inbox i JOIN json_each(i.run_index) j
 WHERE i.run_index IS NOT NULL AND i.run_index->>'_last'>=?1 AND i.run_index->>'_first'<?2 AND j.type='object' AND j.value->>'at'>=?1 AND j.value->>'at'<?2 AND (j.value->>'at',j.key)<(?,?)
 ORDER BY j.value->>'at' DESC,j.key DESC LIMIT 101`, start.UnixMilli(), stop.UnixMilli(), before, key)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var items []historySummary
	cursor := ""
	scanned, used := 0, 0
	for rows.Next() {
		if scanned == 100 || used >= historyWindow || len(items) == 20 {
			return items, cursor, nil
		}
		var event, at int64
		var attempt string
		var raw []byte
		if err := rows.Scan(&event, &attempt, &at, &raw); err != nil {
			return nil, "", err
		}
		cursor = fmt.Sprintf("db:%d:%s", at, attempt)
		scanned++
		used += len(raw)
		rec, err := s.decodeHistory(raw)
		if err != nil {
			return nil, "", err
		}
		if !rec.matches(q) {
			continue
		}
		excerpt, ok := rec.excerpt(strings.ToLower(strings.TrimSpace(q.Get("q"))))
		if !ok {
			continue
		}
		items = append(items, rec.summary(durableRunID(event, attempt), excerpt))
	}
	return items, "", rows.Err()
}

// Old transcripts can have missing or unparseable dates; preserve those verbatim.
func serverTimestamp(value string, loc *time.Location) string {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return value
	}
	return t.In(loc).Format(time.RFC3339Nano)
}
