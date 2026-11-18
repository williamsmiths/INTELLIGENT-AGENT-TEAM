package openbot

import (
	"context"
	"database/sql"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

type turnEvent struct {
	sequence                                                int64
	aggregateType, aggregateID, eventType, occurredAt, turn string
	status                                                  string
}

// stepEvents applies turn.started / turn.completed events to agent_turn. Only
// the turn id and status are extracted, inside SQLite: the rest of the payload
// (message content) never enters the harness. The host writes them under
// "detail"; a top-level field is read too.
func (t *Tailer) stepEvents(ctx context.Context, db *sql.DB, batch int, st *Stats) (bool, error) {
	raw, err := t.cursor(ctx, eventCursor)
	if err != nil {
		return false, err
	}
	var after int64
	if raw != "" {
		if after, err = strconv.ParseInt(raw, 10, 64); err != nil {
			return false, err
		}
	}
	// The newest sequence bounds this batch, so a batch that is not full may move
	// the cursor past every event it did not select without skipping a later one.
	var newest int64
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(max(sequence), 0) FROM orchestration_events`).Scan(&newest); err != nil {
		return false, err
	}
	if newest <= after {
		return false, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT sequence, aggregate_type, aggregate_id, event_type, occurred_at,
			CAST(COALESCE(json_extract(payload_json, '$.turnId'), json_extract(payload_json, '$.detail.turnId'), '') AS TEXT),
			CAST(COALESCE(json_extract(payload_json, '$.status'), json_extract(payload_json, '$.detail.status'), '') AS TEXT)
		FROM orchestration_events
		WHERE sequence > ? AND sequence <= ? AND event_type IN ('turn.started', 'turn.completed')
		ORDER BY sequence LIMIT ?`, after, newest, batch)
	if err != nil {
		return false, err
	}
	var page []turnEvent
	for rows.Next() {
		var e turnEvent
		if err := rows.Scan(&e.sequence, &e.aggregateType, &e.aggregateID, &e.eventType, &e.occurredAt, &e.turn, &e.status); err != nil {
			rows.Close()
			return false, err
		}
		page = append(page, e)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	next, full := newest, len(page) == batch
	if full {
		next = page[len(page)-1].sequence
	}
	applied := 0
	err = t.runtime(ctx, func(tx pgx.Tx) error {
		applied = 0
		for _, e := range page {
			at, err := time.Parse(time.RFC3339Nano, e.occurredAt)
			if err != nil || e.turn == "" {
				continue
			}
			if err := applyTurn(ctx, tx, e, at); err != nil {
				return err
			}
			applied++
		}
		return setCursor(ctx, tx, eventCursor, strconv.FormatInt(next, 10))
	})
	st.Turns += applied
	return full, err
}

// applyTurn upserts one observed turn. Replaying an event changes nothing:
// a start never reopens a turn that already completed.
func applyTurn(ctx context.Context, tx pgx.Tx, e turnEvent, at time.Time) error {
	thread := ""
	if e.aggregateType == "thread" {
		thread = e.aggregateID
	}
	if e.eventType == "turn.started" {
		_, err := tx.Exec(ctx, `INSERT INTO agent_turn (turn_id, thread_id, status, started_at) VALUES ($1, $2, 'running', $3)
			ON CONFLICT (turn_id) DO UPDATE SET started_at = COALESCE(agent_turn.started_at, EXCLUDED.started_at),
				thread_id = CASE WHEN agent_turn.thread_id = '' THEN EXCLUDED.thread_id ELSE agent_turn.thread_id END`,
			e.turn, thread, at)
		return err
	}
	status := e.status
	if status == "" {
		status = "completed"
	}
	_, err := tx.Exec(ctx, `INSERT INTO agent_turn (turn_id, thread_id, status, completed_at) VALUES ($1, $2, $3, $4)
		ON CONFLICT (turn_id) DO UPDATE SET status = EXCLUDED.status, completed_at = EXCLUDED.completed_at,
			thread_id = CASE WHEN agent_turn.thread_id = '' THEN EXCLUDED.thread_id ELSE agent_turn.thread_id END`,
		e.turn, thread, status, at)
	return err
}
