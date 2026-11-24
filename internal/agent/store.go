package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS inbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, dedup_key TEXT NOT NULL UNIQUE,
 payload TEXT NOT NULL, available_at INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','running','done','dead')),
 retries INTEGER NOT NULL DEFAULT 0 CHECK(retries>=0));
CREATE INDEX IF NOT EXISTS idx_inbox_ready ON inbox(status,id);
CREATE TABLE IF NOT EXISTS timers (
 id TEXT PRIMARY KEY, cron_expr TEXT, fire_at INTEGER NOT NULL, prompt TEXT NOT NULL,
 active INTEGER NOT NULL DEFAULT 1 CHECK(active IN (0,1)));
CREATE INDEX IF NOT EXISTS idx_timers_due ON timers(active,fire_at);
CREATE TABLE IF NOT EXISTS actions (
 id TEXT PRIMARY KEY, dedup_key TEXT NOT NULL UNIQUE, tool_name TEXT NOT NULL,
 tool_args TEXT NOT NULL, resume_hint TEXT NOT NULL DEFAULT '',
 status TEXT NOT NULL CHECK(status IN ('pending_approval','approved','rejected','executing','succeeded','failed','unknown')),
 expires_at INTEGER NOT NULL, result TEXT);
CREATE TABLE IF NOT EXISTS outbox (
 id INTEGER PRIMARY KEY AUTOINCREMENT, dedup_key TEXT NOT NULL UNIQUE,
 target TEXT NOT NULL, content TEXT NOT NULL, deliver_at INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'pending' CHECK(status IN ('pending','sending','sent','failed','unknown')),
 retries INTEGER NOT NULL DEFAULT 0 CHECK(retries>=0));
CREATE INDEX IF NOT EXISTS idx_outbox_due ON outbox(status,deliver_at);
PRAGMA user_version=1;
`

// Nullable columns preserve the distinction between historical gaps and observed zeroes.
const schemaV2 = `
ALTER TABLE inbox ADD COLUMN reply_at INTEGER;
ALTER TABLE inbox ADD COLUMN replies TEXT;
ALTER TABLE inbox ADD COLUMN last_call_at INTEGER;
ALTER TABLE inbox ADD COLUMN model_calls TEXT;
ALTER TABLE outbox ADD COLUMN sent_at INTEGER;
ALTER TABLE outbox ADD COLUMN confirmed_by TEXT;
CREATE INDEX idx_inbox_reply_at ON inbox(reply_at) WHERE reply_at IS NOT NULL;
CREATE INDEX idx_inbox_last_call ON inbox(last_call_at) WHERE last_call_at IS NOT NULL;
CREATE INDEX idx_outbox_sent_at ON outbox(sent_at) WHERE sent_at IS NOT NULL;
CREATE TRIGGER record_delivery_confirmation AFTER UPDATE OF status ON outbox
WHEN NEW.status='sent' AND OLD.status!='sent' AND NEW.sent_at IS NULL
BEGIN
  UPDATE outbox SET sent_at=CAST(unixepoch('subsec')*1000 AS INTEGER),
    confirmed_by=CASE OLD.status WHEN 'sending' THEN 'adapter' ELSE 'owner' END WHERE id=NEW.id;
END;
PRAGMA user_version=2;
`

type Event struct {
	ID          int64   `json:"id"`
	Key         string  `json:"key"`
	Payload     Payload `json:"payload"`
	AvailableAt int64   `json:"available_at"`
	Status      string  `json:"status"`
	Retries     int     `json:"retries"`
}
type Payload struct {
	// Embedded field order is part of the stored format: insertEvent compares payload bytes.
	ownerScope
	imRoutes
	Kind         string          `json:"kind"`
	Text         string          `json:"text,omitempty"`
	ActionID     string          `json:"action_id,omitempty"`
	Name         string          `json:"name,omitempty"`
	Arguments    json.RawMessage `json:"arguments,omitempty"`
	WebChat      string          `json:"web_chat,omitempty"`
	DreamProfile string          `json:"dream_profile,omitempty"`
	DreamVersion string          `json:"dream_version,omitempty"`
}
type Timer struct {
	ID     string `json:"id"`
	Cron   string `json:"cron_expr"`
	FireAt int64  `json:"fire_at"`
	Prompt string `json:"prompt"`
	Active bool   `json:"active"`
}
type Action struct {
	ID        string `json:"id"`
	Key       string `json:"dedup_key"`
	Name      string `json:"tool_name"`
	Args      string `json:"tool_args"`
	Hint      string `json:"resume_hint"`
	Status    string `json:"status"`
	ExpiresAt int64  `json:"expires_at"`
	Result    string `json:"result"`
}
type Notification struct {
	ID        int64  `json:"id"`
	Key       string `json:"dedup_key"`
	Target    string `json:"target"`
	Content   string `json:"content"`
	DeliverAt int64  `json:"deliver_at"`
	Status    string `json:"status"`
	Retries   int    `json:"retries"`
}

func openDB(root string) (*sql.DB, *sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(root, "data", "state.db")}
	q := url.Values{"_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "journal_mode(WAL)", "synchronous(FULL)"}, "_txlock": {"immediate"}}
	u.RawQuery = q.Encode()
	w, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, nil, e
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	fail := func(err error) (*sql.DB, *sql.DB, error) { w.Close(); return nil, nil, err }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if e = w.PingContext(ctx); e != nil {
		return fail(e)
	}
	var version int
	if e = w.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); e != nil {
		return fail(e)
	}
	if version > 4 {
		return fail(errors.New("database schema is newer than this binary"))
	}
	if version < 2 {
		e = transaction(ctx, w, func(tx *sql.Tx) error {
			if version == 0 {
				if _, err := tx.ExecContext(ctx, schema); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, schemaV2)
			return err
		})
		if e != nil {
			return fail(e)
		}
	}
	if version < 3 {
		e = transaction(ctx, w, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `ALTER TABLE inbox ADD COLUMN run_index TEXT;
ALTER TABLE inbox ADD COLUMN run_data TEXT;
CREATE INDEX idx_inbox_indexed_runs ON inbox(run_index->>'_last') WHERE run_index IS NOT NULL;
ALTER TABLE outbox ADD COLUMN origin_event INTEGER;
ALTER TABLE outbox ADD COLUMN origin_attempt TEXT;
CREATE INDEX idx_outbox_origin ON outbox(origin_event);
PRAGMA user_version=3;`)
			return err
		})
		if e != nil {
			return fail(e)
		}
	}
	if version < 4 {
		e = transaction(ctx, w, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `ALTER TABLE inbox ADD COLUMN chat_head INTEGER;
       CREATE INDEX idx_inbox_chat ON inbox(payload->>'web_chat',id) WHERE payload->>'web_chat' IS NOT NULL;
       PRAGMA user_version=4;`)
			return err
		})
		if e != nil {
			return fail(e)
		}
	}
	q = url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "foreign_keys(1)", "query_only(1)"}, "_txlock": {"deferred"}}
	u.RawQuery = q.Encode()
	r, e := sql.Open("sqlite", u.String())
	if e != nil {
		return fail(e)
	}
	r.SetMaxOpenConns(4)
	r.SetMaxIdleConns(4)
	if e = r.PingContext(ctx); e != nil {
		r.Close()
		return fail(e)
	}
	return w, r, nil
}
func transaction(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit()
}
func insertEvent(ctx context.Context, tx *sql.Tx, key string, p Payload, due int64) (int64, bool, error) {
	b, e := json.Marshal(p)
	if e != nil {
		return 0, false, e
	}
	result, e := tx.ExecContext(ctx, "INSERT INTO inbox(dedup_key,payload,available_at) VALUES(?,?,?) ON CONFLICT(dedup_key) DO NOTHING", key, string(b), due)
	if e != nil {
		return 0, false, e
	}
	n, e := result.RowsAffected()
	if e != nil {
		return 0, false, e
	}
	var id int64
	var existing string
	if e = tx.QueryRowContext(ctx, "SELECT id,payload FROM inbox WHERE dedup_key=?", key).Scan(&id, &existing); e != nil {
		return 0, false, e
	}
	if existing != string(b) {
		return 0, false, fmt.Errorf("%w: dedup key has different payload", ErrConflict)
	}
	return id, n == 1, nil
}
func scanAction(row interface{ Scan(...any) error }) (a Action, e error) {
	e = row.Scan(&a.ID, &a.Key, &a.Name, &a.Args, &a.Hint, &a.Status, &a.ExpiresAt, &a.Result)
	return
}

const actionColumns = "id,dedup_key,tool_name,tool_args,resume_hint,status,expires_at,COALESCE(result,'')"

func (s *Service) action(ctx context.Context, id string) (Action, error) {
	return scanAction(s.read.QueryRowContext(ctx, "SELECT "+actionColumns+" FROM actions WHERE id=?", id))
}

var ErrConflict = errors.New("conflict")
var ErrPaused = errors.New("paused")
var ErrDenied = errors.New("permission denied")
