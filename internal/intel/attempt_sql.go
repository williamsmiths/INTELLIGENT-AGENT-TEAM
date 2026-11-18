package intel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// AttemptRow is one verify of a lease holder (C.4). Runtime only.
type AttemptRow struct {
	ID            int64          `json:"id"`
	ProjectID     string         `json:"project_id"`
	TaskID        string         `json:"task_id"`
	LeaseID       string         `json:"lease_id"`
	PrincipalID   string         `json:"principal_id"`
	TaskRevision  int            `json:"task_revision"`
	Candidate     string         `json:"candidate"`
	AllPass       bool           `json:"all_pass"`
	Passing       []string       `json:"passing"`
	Failing       []string       `json:"failing"`
	Fingerprint   string         `json:"fingerprint"`
	Repeats       int            `json:"repeats"`
	DeltaMismatch map[string]any `json:"delta_mismatch,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

const attemptCols = `id, project_id, task_id, lease_id, principal_id, task_revision, candidate, all_pass, passing, failing,
	fingerprint, repeats, delta_mismatch, created_at`

func scanAttempt(row pgx.Row) (AttemptRow, error) {
	var a AttemptRow
	var mismatch []byte
	if err := row.Scan(&a.ID, &a.ProjectID, &a.TaskID, &a.LeaseID, &a.PrincipalID, &a.TaskRevision, &a.Candidate, &a.AllPass,
		&a.Passing, &a.Failing, &a.Fingerprint, &a.Repeats, &mismatch, &a.CreatedAt); err != nil {
		return a, err
	}
	if len(mismatch) > 0 {
		return a, json.Unmarshal(mismatch, &a.DeltaMismatch)
	}
	return a, nil
}

// PreviousAttempt returns the last attempt of a task, or nil when there is none
// or the owner resumed the task after it (the counter starts again, T-S2-03).
func PreviousAttempt(ctx context.Context, tx pgx.Tx, taskID string) (*AttemptRow, error) {
	a, err := scanAttempt(tx.QueryRow(ctx, `SELECT `+attemptCols+` FROM attempt a WHERE task_id = $1
		AND NOT EXISTS (SELECT 1 FROM journal j WHERE j.task_id = $1 AND j.type = 'NONPROGRESS_RESUMED' AND j.created_at >= a.created_at)
		ORDER BY id DESC LIMIT 1`, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &a, err
}

// InsertAttempt stores an attempt (runtime scope).
func InsertAttempt(ctx context.Context, tx pgx.Tx, a AttemptRow) (AttemptRow, error) {
	var mismatch []byte
	if a.DeltaMismatch != nil {
		var err error
		if mismatch, err = json.Marshal(a.DeltaMismatch); err != nil {
			return AttemptRow{}, err
		}
	}
	return scanAttempt(tx.QueryRow(ctx, `INSERT INTO attempt (project_id, task_id, lease_id, principal_id, task_revision, candidate,
			all_pass, passing, failing, fingerprint, repeats, delta_mismatch)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12) RETURNING `+attemptCols,
		a.ProjectID, a.TaskID, a.LeaseID, a.PrincipalID, a.TaskRevision, a.Candidate, a.AllPass, orNone(a.Passing), orNone(a.Failing),
		a.Fingerprint, a.Repeats, mismatch))
}

// ListAttempts returns the attempts of a task, oldest first.
func ListAttempts(ctx context.Context, tx pgx.Tx, taskID string) ([]AttemptRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+attemptCols+` FROM attempt WHERE task_id = $1 ORDER BY id`, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (AttemptRow, error) { return scanAttempt(row) })
}

// BonusGranted reports whether convergence already granted the extra consult of the task.
func BonusGranted(ctx context.Context, tx pgx.Tx, taskID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal WHERE task_id = $1 AND type = 'NONPROGRESS')`, taskID).Scan(&ok)
	return ok, err
}

// LessonFacts are why a lease is asked for a lesson after its all-pass verify (C.6).
type LessonFacts struct {
	FailedAttempts  int      `json:"failed_attempts"`
	Attempts        int      `json:"attempts"`
	FailedChecks    []string `json:"failed_checks"`
	ScopeViolations int      `json:"scope_violations"`
	Feedback        bool     `json:"feedback"`
}

// Due reports whether a lesson is to be asked for.
func (f LessonFacts) Due() bool { return f.FailedAttempts > 0 || f.ScopeViolations > 0 || f.Feedback }

// LeaseFacts reads the facts of C.6 for a lease: its failing attempts and
// their checks, its SCOPE_VIOLATION events, and whether its revision came
// from an owner feedback (an answer different from the default included).
func LeaseFacts(ctx context.Context, tx pgx.Tx, leaseID, taskID string, revision int) (LessonFacts, error) {
	var f LessonFacts
	err := tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM attempt WHERE lease_id = $1 AND NOT all_pass),
			(SELECT count(*) FROM attempt WHERE lease_id = $1),
			COALESCE((SELECT array_agg(DISTINCT c ORDER BY c) FROM attempt, unnest(failing) c WHERE lease_id = $1), '{}'),
			(SELECT count(*) FROM journal WHERE task_id = $2 AND type = 'SCOPE_VIOLATION' AND payload->>'lease_id' = $1::text),
			EXISTS (SELECT 1 FROM journal WHERE task_id = $2 AND type = 'TASK_FEEDBACK' AND (payload->>'revision')::int = $3)`,
		leaseID, taskID, revision).Scan(&f.FailedAttempts, &f.Attempts, &f.FailedChecks, &f.ScopeViolations, &f.Feedback)
	return f, err
}
