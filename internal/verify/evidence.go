package verify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrForeignResult is returned by Record for a Result that Run did not produce for that Request.
var ErrForeignResult = errors.New("verify: result was not produced by Run for this request")

// Evidence is one stored evidence row.
type Evidence struct {
	ID         string
	ProjectID  string
	TaskID     string
	Revision   int
	Candidate  string
	CheckID    string
	Command    string
	ExitCode   *int
	Result     string
	LogSHA256  string
	LogPath    string
	Detail     map[string]any
	RunnerID   string
	StartedAt  time.Time
	FinishedAt time.Time
}

// Record inserts the evidence rows of one run. It is the only writer of
// evidence in the system (PM-H-18): the transaction must run under
// store.Runtime, and row-level security refuses any other scope. res must come
// from Run with the same req; a Result built or decoded elsewhere is refused,
// so there is no path for a result claimed by someone else (AC-H-042).
// Recording the same Result twice inserts nothing new (AC-H-045).
func Record(ctx context.Context, tx pgx.Tx, req Request, res Result) error {
	if res.sealedFor == "" || res.sealedFor != req.sealKey() {
		return ErrForeignResult
	}
	for _, it := range res.Items {
		if it.Result == ResultPass && (it.ExitCode == nil || *it.ExitCode != 0) {
			return fmt.Errorf("verify: %s is PASS without exit code 0", it.CheckID)
		}
		detail, err := json.Marshal(it.Detail)
		if err != nil {
			return fmt.Errorf("verify: encode detail of %s: %w", it.CheckID, err)
		}
		if it.Detail == nil {
			detail = []byte("{}")
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO evidence (id, project_id, task_id, task_revision, candidate_hash, check_id, command,
			                       exit_code, result, log_sha256, log_path, detail, runner_id, started_at, finished_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
			 ON CONFLICT (id) DO NOTHING`,
			it.ID, req.ProjectID, req.TaskID, req.Revision, res.Candidate, it.CheckID, it.Command,
			it.ExitCode, it.Result, it.LogSHA256, it.LogPath, detail, req.RunnerID, it.StartedAt, it.FinishedAt)
		if err != nil {
			return fmt.Errorf("verify: insert evidence %s: %w", it.CheckID, err)
		}
	}
	return nil
}

const evidenceColumns = `id::text, project_id::text, task_id::text, task_revision, candidate_hash, check_id, command,
	exit_code, result, log_sha256, log_path, detail, runner_id, started_at, finished_at`

// Latest returns the newest evidence row of each check of a task, ordered by
// check id, whatever its revision or candidate. Row-level security limits the
// rows to the projects of the caller's scope (PM-H-16).
func Latest(ctx context.Context, tx pgx.Tx, taskID string) ([]Evidence, error) {
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT ON (check_id) `+evidenceColumns+`
		   FROM evidence WHERE task_id = $1
		  ORDER BY check_id, finished_at DESC, started_at DESC, id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("verify: read evidence: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Evidence, error) {
		var e Evidence
		var detail []byte
		err := row.Scan(&e.ID, &e.ProjectID, &e.TaskID, &e.Revision, &e.Candidate, &e.CheckID, &e.Command,
			&e.ExitCode, &e.Result, &e.LogSHA256, &e.LogPath, &detail, &e.RunnerID, &e.StartedAt, &e.FinishedAt)
		if err != nil {
			return e, err
		}
		return e, json.Unmarshal(detail, &e.Detail)
	})
	if err != nil {
		return nil, fmt.Errorf("verify: read evidence: %w", err)
	}
	return out, nil
}

// HasPassFor reports whether every listed check and the built-in "scope" check
// have PASS as their newest evidence on exactly this task revision and
// candidate (the evidence condition of T-H-03). A later FAIL or ERROR on the
// same candidate overrides an older PASS.
func HasPassFor(ctx context.Context, tx pgx.Tx, taskID string, revision int, candidate string, checkIDs []string) (bool, error) {
	if candidate == "" {
		return false, nil
	}
	want := map[string]bool{ScopeCheckID: true}
	for _, id := range checkIDs {
		want[id] = true
	}
	ids := make([]string, 0, len(want))
	for id := range want {
		ids = append(ids, id)
	}
	var passed int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM (
		   SELECT DISTINCT ON (check_id) result, exit_code FROM evidence
		    WHERE task_id = $1 AND task_revision = $2 AND candidate_hash = $3 AND check_id = ANY($4)
		    ORDER BY check_id, finished_at DESC, started_at DESC, id) newest
		  WHERE result = 'PASS' AND exit_code = 0`, taskID, revision, candidate, ids).Scan(&passed)
	if err != nil {
		return false, fmt.Errorf("verify: read evidence: %w", err)
	}
	return passed == len(ids), nil
}
