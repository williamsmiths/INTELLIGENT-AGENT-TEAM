package work

import (
	"context"
	"encoding/json"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
)

// Event is one journal row. Payloads hold ids and reason codes only (PM-H-20).
type Event struct {
	Seq       int64           `json:"seq"`
	ProjectID string          `json:"project_id,omitempty"`
	TaskID    string          `json:"task_id,omitempty"`
	Type      string          `json:"type"`
	Actor     string          `json:"actor,omitempty"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
}

const eventCols = `seq, COALESCE(project_id::text, ''), COALESCE(task_id::text, ''), type, COALESCE(actor::text, ''),
	payload, created_at`

// ListEvents pages the journal of one project by seq (PM-H-20; actor scope):
// the owner, or a member of the project. Events of other projects can never
// appear: the query names the project and row-level security filters again (AC-H-082).
func ListEvents(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string, after int64, limit int) ([]Event, error) {
	if err := requireMember(actor, projectID); err != nil {
		return nil, err
	}
	return queryEvents(ctx, tx, `project_id = $3`, after, limit, projectID)
}

// ListSystemEvents pages the events that belong to no project: GLOBAL
// knowledge, hooks, tailer (PM-H-20; owner only; actor scope).
func ListSystemEvents(ctx context.Context, tx pgx.Tx, actor authn.Principal, after int64, limit int) ([]Event, error) {
	if err := requireOwner(actor); err != nil {
		return nil, err
	}
	return queryEvents(ctx, tx, `project_id IS NULL`, after, limit)
}

func queryEvents(ctx context.Context, tx pgx.Tx, where string, after int64, limit int, args ...any) ([]Event, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := tx.Query(ctx, `SELECT `+eventCols+` FROM journal WHERE seq > $1 AND `+where+` ORDER BY seq LIMIT $2`,
		append([]any{after, limit}, args...)...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Event, error) {
		var e Event
		var payload []byte
		err := row.Scan(&e.Seq, &e.ProjectID, &e.TaskID, &e.Type, &e.Actor, &payload, &e.CreatedAt)
		e.Payload = payload
		return e, err
	})
}
