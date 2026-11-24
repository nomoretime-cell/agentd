package agent

import (
	"context"
	"fmt"
	"strconv"
)

type workItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

type workPage struct {
	Counts map[string]int `json:"counts"`
	Items  []workItem     `json:"items"`
}

// Keep the list and its count on the same snapshot, including items older
// than the general-purpose recent-activity endpoints.
func (s *Service) workItems(ctx context.Context, kind, offsetText string) (workPage, error) {
	page := workPage{Counts: map[string]int{}, Items: []workItem{}}
	type source struct{ kind, fields, from string }
	sources := []source{
		{"actions", "id,tool_name,status,resume_hint", "actions WHERE status IN ('pending_approval','unknown')"},
		{"inbox", "id,json_extract(payload,'$.kind'),status,coalesce(json_extract(payload,'$.text'),'')", "inbox WHERE status='dead'"},
		{"outbox", "id,target,status,content", "outbox WHERE status IN ('failed','unknown')"},
	}
	const limit = 20
	offset := 0
	if offsetText != "" {
		var err error
		offset, err = strconv.Atoi(offsetText)
		if err != nil || offset < 0 {
			return page, fmt.Errorf("invalid offset")
		}
	}
	drafts, err := s.Drafts()
	if err != nil {
		return page, err
	}
	page.Counts["drafts"] = len(drafts)
	if kind == "drafts" && offset < len(drafts) {
		for _, d := range drafts[offset : offset+min(limit, len(drafts)-offset)] {
			page.Items = append(page.Items, workItem{d.Name, s.redact(d.Name), d.Kind, s.redact(d.Description)})
		}
	}
	tx, err := s.read.BeginTx(ctx, nil)
	if err != nil {
		return page, err
	}
	defer tx.Rollback()
	for _, src := range sources {
		var count int
		if err = tx.QueryRowContext(ctx, "SELECT count(*) FROM "+src.from).Scan(&count); err != nil {
			return page, err
		}
		page.Counts[src.kind] = count
		if src.kind != kind {
			continue
		}
		rows, err := tx.QueryContext(ctx, "SELECT "+src.fields+" FROM "+src.from+" ORDER BY rowid DESC LIMIT ? OFFSET ?", limit, offset)
		if err != nil {
			return page, err
		}
		defer rows.Close()
		for rows.Next() {
			var item workItem
			if err = rows.Scan(&item.ID, &item.Title, &item.Status, &item.Detail); err != nil {
				return page, err
			}
			item.Title, item.Detail = s.redact(item.Title), s.bounded(item.Detail)
			page.Items = append(page.Items, item)
		}
		if err = rows.Err(); err != nil {
			return page, err
		}
	}
	if _, ok := page.Counts[kind]; !ok {
		return page, fmt.Errorf("invalid work item kind")
	}
	return page, tx.Commit()
}
