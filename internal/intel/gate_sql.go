package intel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Kinds and statuses of an understanding row.
const (
	KindFull     = "FULL"
	KindDelta    = "DELTA"
	StatusReject = "REJECTED"
	StatusAccept = "ACCEPTED"
	StatusForced = "FORCED"
)

// Submission limits (C.2, C.5): the third full submission and the fourth delta
// are taken even when they miss items, so an agent is never locked forever.
const (
	FullRejectLimit  = 2
	DeltaRejectLimit = 3
)

// Pending says which understanding a task still needs at a revision: a full
// one when none was ever accepted, else a delta when the revision moved past
// the full one and no delta was accepted for it (C.2, C.5).
type Pending struct {
	Full  bool
	Delta bool
}

// Any reports whether a write must wait.
func (p Pending) Any() bool { return p.Full || p.Delta }

// GatePending reads what the task still needs at this revision.
func GatePending(ctx context.Context, tx pgx.Tx, taskID string, revision int) (Pending, error) {
	var fullRev *int
	var delta bool
	err := tx.QueryRow(ctx, `SELECT
			(SELECT max(task_revision) FROM understanding WHERE task_id = $1 AND kind = 'FULL' AND status <> 'REJECTED'),
			EXISTS (SELECT 1 FROM understanding WHERE task_id = $1 AND kind = 'DELTA' AND status <> 'REJECTED' AND task_revision = $2)`,
		taskID, revision).Scan(&fullRev, &delta)
	if err != nil {
		return Pending{}, err
	}
	if fullRev == nil {
		return Pending{Full: true}, nil
	}
	return Pending{Delta: *fullRev < revision && !delta}, nil
}

// Rejections counts the rejected submissions of one kind at one revision.
func Rejections(ctx context.Context, tx pgx.Tx, taskID string, revision int, kind string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM understanding WHERE task_id = $1 AND task_revision = $2 AND kind = $3 AND status = 'REJECTED'`,
		taskID, revision, kind).Scan(&n)
	return n, err
}

// UnderstandingRow is one stored submission.
type UnderstandingRow struct {
	ID           string          `json:"id"`
	ProjectID    string          `json:"project_id"`
	TaskID       string          `json:"task_id"`
	LeaseID      string          `json:"lease_id"`
	TaskRevision int             `json:"task_revision"`
	Author       string          `json:"author"`
	Kind         string          `json:"kind"`
	Status       string          `json:"status"`
	Body         json.RawMessage `json:"body"`
	Unaddressed  []Missing       `json:"unaddressed"`
	CreatedAt    time.Time       `json:"created_at"`
}

// InsertUnderstanding stores a submission; the author's scope may write it (C.11).
func InsertUnderstanding(ctx context.Context, tx pgx.Tx, r UnderstandingRow) (string, error) {
	missing, err := json.Marshal(orNone(r.Unaddressed))
	if err != nil {
		return "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO understanding (project_id, task_id, lease_id, task_revision, author, kind, status, body, unaddressed)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`, r.ProjectID, r.TaskID, r.LeaseID, r.TaskRevision, r.Author,
		r.Kind, r.Status, []byte(r.Body), missing).Scan(&id)
	return id, err
}

// ListUnderstandings returns every submission of a task, oldest first.
func ListUnderstandings(ctx context.Context, tx pgx.Tx, taskID string) ([]UnderstandingRow, error) {
	rows, err := tx.Query(ctx, `SELECT id, project_id, task_id, lease_id, task_revision, author, kind, status, body, unaddressed, created_at
		FROM understanding WHERE task_id = $1 ORDER BY created_at, id`, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (UnderstandingRow, error) {
		var r UnderstandingRow
		var body, missing []byte
		if err := row.Scan(&r.ID, &r.ProjectID, &r.TaskID, &r.LeaseID, &r.TaskRevision, &r.Author, &r.Kind, &r.Status,
			&body, &missing, &r.CreatedAt); err != nil {
			return r, err
		}
		r.Body = body
		return r, json.Unmarshal(missing, &r.Unaddressed)
	})
}

// WorkPhase is the phase of work outside discovery (C.7): REWORK from the
// first failing attempt that no all-pass attempt has followed yet, else BUILD.
func WorkPhase(ctx context.Context, tx pgx.Tx, taskID string) (string, error) {
	var rework bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM attempt a WHERE a.task_id = $1 AND NOT a.all_pass
		AND a.id > COALESCE((SELECT max(id) FROM attempt p WHERE p.task_id = $1 AND p.all_pass), 0))`, taskID).Scan(&rework)
	if rework {
		return PhaseRework, err
	}
	return PhaseBuild, err
}

// LockTask serializes the intel writes of one task (numbering, allowances)
// without needing the right to lock the task row itself.
func LockTask(ctx context.Context, tx pgx.Tx, taskID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kch.intel:' || $1, 0))`, taskID)
	return err
}

func orNone[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}

// LatestDelta returns the paths and keys the newest accepted delta of a task declared as changed.
func LatestDelta(ctx context.Context, tx pgx.Tx, taskID string) (changed []string, found bool, err error) {
	var body []byte
	err = tx.QueryRow(ctx, `SELECT body FROM understanding WHERE task_id = $1 AND kind = 'DELTA' AND status <> 'REJECTED'
		ORDER BY created_at DESC, id DESC LIMIT 1`, taskID).Scan(&body)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var u Understanding
	if err := json.Unmarshal(body, &u); err != nil || u.Delta == nil {
		return nil, false, err
	}
	return u.Delta.Changed, true, nil
}
