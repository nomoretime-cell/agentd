package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Each source defines one row per audited item. The graph and drill-down
// always share its date field, identity and selection rules.
type activitySource struct{ from, at, id, total, detail string }

const hasIndexedAttempt = ", coalesce(json_type(i.run_index,'$.'||(j.value->>'attempt'))='object',0)"

var activitySources = map[string]activitySource{
	"replies": {
		`inbox i JOIN json_each(i.replies) j LEFT JOIN outbox o ON o.dedup_key=j.value->>'delivery_key'`,
		"i.reply_at", "j.value->>'id'", "NULL",
		`i.id, j.value->>'attempt',j.value->>'conversation', j.value->>'body', coalesce(o.status,''), '',NULL,NULL` + hasIndexedAttempt,
	},
	"tokens": {
		`(SELECT id,model_calls,run_index FROM inbox WHERE last_call_at >= :start) i JOIN json_each(i.model_calls) j`,
		"j.value->>'at'", "j.key", "j.value->>'total'",
		`i.id, j.value->>'attempt', j.value->>'model', j.value->>'purpose', j.value->>'executor','',j.value->>'input',j.value->>'output'` + hasIndexedAttempt,
	},
	"news": {
		`(SELECT * FROM outbox WHERE status='sent' AND ` + formalNewsSQL + `) o`,
		"o.sent_at", "cast(o.id as text)", "NULL",
		`0,'',o.target,o.content,o.confirmed_by,o.dedup_key,NULL,NULL,0`,
	},
}

func (src activitySource) rows(ctx context.Context, tx *sql.Tx, start, end time.Time, cursor string, detail bool) (*sql.Rows, error) {
	fields := src.at + ", " + src.id + ", " + src.total
	if detail {
		fields += ", " + src.detail
	}
	q := "SELECT " + fields + " FROM " + src.from + " WHERE " + src.at + ">=:start AND " + src.at + "<:end"
	args := []any{sql.Named("start", start.UnixMilli()), sql.Named("end", end.UnixMilli())}
	if cursor != "" {
		stamp, id, ok := strings.Cut(cursor, ".")
		at, err := strconv.ParseInt(stamp, 10, 64)
		if !ok || err != nil || id == "" {
			return nil, errors.New("invalid activity cursor")
		}
		q += " AND (" + src.at + ", " + src.id + ") > (:after,:key)"
		args = append(args, sql.Named("after", at), sql.Named("key", id))
	}
	if detail {
		q += " ORDER BY 1,2 LIMIT 31"
	}
	return tx.QueryContext(ctx, q, args...)
}

type activityDay struct {
	Date     string `json:"date"`
	Coverage string `json:"coverage"`
	Replies  int    `json:"replies"`
	News     int    `json:"news"`
	Calls    int    `json:"calls"`
	Reported int    `json:"reported"`
	Tokens   int64  `json:"tokens"`
}
type activityOverview struct {
	Timezone    string        `json:"timezone"`
	Since       time.Time     `json:"since"`
	Days        []activityDay `json:"days"`
	UndatedNews int           `json:"undated_news"`
}

func (s *Service) dailyActivity(ctx context.Context, days int, now time.Time) (activityOverview, error) {
	result := activityOverview{Timezone: s.cfg.Timezone, Since: s.metricsSince}
	if days != 7 && days != 30 {
		return result, errors.New("select 7 or 30 days")
	}
	loc, _ := time.LoadLocation(s.cfg.Timezone)
	result.Since = result.Since.In(loc)
	today, _ := time.ParseInLocation(time.DateOnly, now.In(loc).Format(time.DateOnly), loc)
	start := today.AddDate(0, 0, 1-days)
	byDate := map[string]int{}
	for i := 0; i < days; i++ {
		day := start.AddDate(0, 0, i)
		coverage := "full"
		if !day.AddDate(0, 0, 1).After(s.metricsSince) {
			coverage = "none"
		} else if day.Before(s.metricsSince) {
			coverage = "partial"
		}
		key := day.Format(time.DateOnly)
		byDate[key] = i
		result.Days = append(result.Days, activityDay{Date: key, Coverage: coverage})
	}
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	for kind, src := range activitySources {
		rows, err := src.rows(ctx, tx, start, today.AddDate(0, 0, 1), "", false)
		if err != nil {
			return result, err
		}
		for rows.Next() {
			var at int64
			var id string
			var total sql.NullInt64
			if err = rows.Scan(&at, &id, &total); err != nil {
				rows.Close()
				return result, err
			}
			day := &result.Days[byDate[time.UnixMilli(at).In(loc).Format(time.DateOnly)]]
			switch kind {
			case "replies":
				day.Replies++
			case "news":
				day.News++
			case "tokens":
				day.Calls++
				if total.Valid {
					day.Reported++
					day.Tokens += total.Int64
				}
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return result, err
		}
	}
	err = tx.QueryRowContext(ctx, "SELECT count(*) FROM outbox WHERE status='sent' AND sent_at IS NULL AND "+formalNewsSQL).Scan(&result.UndatedNews)
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}

type activityItem struct {
	LocalAt time.Time `json:"local_at"`
	Run     string    `json:"run"`
	At      int64     `json:"at"`
	ID      string    `json:"id"`
	Total   *int64    `json:"total"`
	Event   int64     `json:"event"`
	Attempt string    `json:"attempt"`
	Label   string    `json:"label"`
	Body    string    `json:"body"`
	Status  string    `json:"status"`
	Key     string    `json:"key"`
	Input   *int64    `json:"input"`
	Output  *int64    `json:"output"`
}
type activityPage struct {
	Items []activityItem `json:"items"`
	Next  string         `json:"next"`
}

func (s *Service) activityDetails(ctx context.Context, kind string, q url.Values) (activityPage, error) {
	page := activityPage{Items: []activityItem{}}
	src, ok := activitySources[kind]
	if !ok {
		return page, sql.ErrNoRows
	}
	loc, _ := time.LoadLocation(s.cfg.Timezone)
	date, err := time.ParseInLocation(time.DateOnly, q.Get("date"), loc)
	if err != nil {
		return page, errors.New("invalid activity date")
	}
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	rows, err := src.rows(ctx, tx, date, date.AddDate(0, 0, 1), q.Get("cursor"), true)
	if err != nil {
		return page, err
	}
	defer rows.Close()
	for rows.Next() {
		if len(page.Items) == 30 {
			last := page.Items[29]
			page.Next = fmt.Sprintf("%d.%s", last.At, last.ID)
			break
		}
		var it activityItem
		var indexed bool
		if err = rows.Scan(&it.At, &it.ID, &it.Total, &it.Event, &it.Attempt, &it.Label, &it.Body, &it.Status, &it.Key, &it.Input, &it.Output, &indexed); err != nil {
			return page, err
		}
		if indexed {
			it.Run = durableRunID(it.Event, it.Attempt)
		}
		it.LocalAt = time.UnixMilli(it.At).In(loc)
		it.Body, it.Label = s.redact(it.Body), s.redact(it.Label)
		page.Items = append(page.Items, it)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return page, err
	}
	return page, tx.Commit()
}
