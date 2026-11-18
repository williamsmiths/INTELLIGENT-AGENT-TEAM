// Package openbot is the harness side of the host app (OpenBot / kongCowork).
// The host owns turns, sessions and raw token metering in its SQLite database
// (docs/specs/_context.md); the harness only reads that file (Tailer) and asks
// the host to deliver messages through its local automation API (Notifier).
// Observing is not intervening: nothing here blocks or cancels a turn.
package openbot

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// Cursor names in tail_cursor.
const (
	usageCursor = "openbot_usage"  // JSON: recorded_at, record_id, agent_id of the last usage row
	eventCursor = "openbot_events" // sequence of the last orchestration event
)

// Stats counts what one Step did.
type Stats struct {
	Usage        int  // usage records written to the ledger
	Duplicates   int  // usage records that were already in the ledger (AC-H-032)
	Unattributed int  // of Usage: outside every lease, stored but added to no budget
	UnknownAgent int  // usage of an OpenBot agent without a principal: skipped (AC-H-061)
	Invalid      int  // malformed usage rows: skipped
	Turns        int  // turn.started / turn.completed events applied to agent_turn
	More         bool // a batch was full: more rows are waiting
}

// Tailer reads new usage records and turn events from the host SQLite file
// and records them in the harness under the runtime scope (PM-H-18).
type Tailer struct {
	Pool   *pgxpool.Pool
	Path   string             // the host's openbot.db
	Batch  int                // rows per stream and Step; 200 when zero
	OnStep func(Stats, error) // optional: Run reports every Step here
	db     *sql.DB
}

// NewTailer returns a tailer for the host database at path. The file is opened
// on the first Step, so a host that is not installed yet is only an error then.
func NewTailer(pool *pgxpool.Pool, path string) *Tailer {
	return &Tailer{Pool: pool, Path: path}
}

func (t *Tailer) open() (*sql.DB, error) {
	if t.db != nil {
		return t.db, nil
	}
	db, err := openReadOnly(t.Path)
	if err != nil {
		return nil, err
	}
	t.db = db
	return db, nil
}

// openReadOnly connects strictly read-only: mode=ro never creates or writes the
// file, and query_only makes SQLite refuse every writing statement on this handle.
func openReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := (&url.URL{Scheme: "file", Path: abs}).String() + "?mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Close releases the SQLite handle.
func (t *Tailer) Close() error {
	if t.db == nil {
		return nil
	}
	err := t.db.Close()
	t.db = nil
	return err
}

// Step processes one bounded batch of each stream. Every usage record is
// written together with its cursor in one transaction, and the ledger is
// idempotent on top of that, so Step is safe to repeat and to resume after a
// crash at any point (AC-H-060).
func (t *Tailer) Step(ctx context.Context) (Stats, error) {
	var st Stats
	db, err := t.open()
	if err != nil {
		return st, err
	}
	batch := t.Batch
	if batch <= 0 {
		batch = 200
	}
	moreUsage, err := t.stepUsage(ctx, db, batch, &st)
	if err != nil {
		return st, err
	}
	moreEvents, err := t.stepEvents(ctx, db, batch, &st)
	st.More = moreUsage || moreEvents
	return st, err
}

// Run calls Step every interval until ctx ends. A backlog is drained batch
// after batch without waiting; otherwise, and after an error, the loop sleeps
// until the next tick, so it never spins.
func (t *Tailer) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		st, err := t.Step(ctx)
		if ctx.Err() != nil {
			return ctx.Err() // a step cut short by the shutdown is not an error to report
		}
		if t.OnStep != nil {
			t.OnStep(st, err)
		}
		if err == nil && st.More {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// runtime runs fn under the runtime scope, the only scope that may write the
// ledger, observed turns and cursors.
func (t *Tailer) runtime(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return store.InScope(ctx, t.Pool, store.Runtime, fn)
}

func (t *Tailer) cursor(ctx context.Context, name string) (string, error) {
	var value string
	err := t.runtime(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT value FROM tail_cursor WHERE name = $1`, name).Scan(&value)
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	})
	return value, err
}

func setCursor(ctx context.Context, tx pgx.Tx, name, value string) error {
	_, err := tx.Exec(ctx, `INSERT INTO tail_cursor (name, value) VALUES ($1, $2)
		ON CONFLICT (name) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`, name, value)
	return err
}
