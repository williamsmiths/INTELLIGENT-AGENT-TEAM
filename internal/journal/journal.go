// Package journal appends to the harness event journal. Payloads carry ids and
// reason codes only, never knowledge content (spec PM-H-20).
package journal

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Event is one journal row. Empty ProjectID, TaskID or Actor are stored as NULL.
type Event struct {
	ProjectID string
	TaskID    string
	Type      string
	Actor     string
	Payload   map[string]any
}

// Append writes one event in the caller's transaction and returns its sequence number.
func Append(ctx context.Context, tx pgx.Tx, e Event) (int64, error) {
	payload := e.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	var seq int64
	err = tx.QueryRow(ctx,
		`INSERT INTO journal (project_id, task_id, type, actor, payload)
		 VALUES (nullif($1,'')::uuid, nullif($2,'')::uuid, $3, nullif($4,'')::uuid, $5) RETURNING seq`,
		e.ProjectID, e.TaskID, e.Type, e.Actor, raw).Scan(&seq)
	return seq, err
}
