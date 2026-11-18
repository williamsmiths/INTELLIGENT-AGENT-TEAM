package memory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// BlackboardEntry is one append-only delta shared inside a project (PM-H-10).
type BlackboardEntry struct {
	ID        int64
	ProjectID string
	TaskID    string // empty when the entry is not about one task
	Kind      string // finding, evidence, proposal, conflict, decision, impact, next_action
	Body      string
	Refs      []string
	Author    string
	CreatedAt time.Time
}

// BlackboardInput is a new entry. Refs point at what the delta is about
// (knowledge keys, files, evidence ids); the spec does not fix their format.
type BlackboardInput struct {
	ProjectID string
	TaskID    string
	Kind      string
	Body      string
	Refs      []string
}

const (
	blackboardCols = `id, project_id::text, coalesce(task_id::text,''), kind, body, refs, author::text, created_at`

	defaultListLimit = 50
	maxListLimit     = 200
)

func scanBlackboard(row pgx.Row) (BlackboardEntry, error) {
	var e BlackboardEntry
	var refs []byte
	if err := row.Scan(&e.ID, &e.ProjectID, &e.TaskID, &e.Kind, &e.Body, &refs, &e.Author, &e.CreatedAt); err != nil {
		return e, err
	}
	if err := json.Unmarshal(refs, &e.Refs); err != nil {
		e.Refs = []string{string(refs)} // refs written by another client in another shape stay readable
	}
	return e, nil
}

func clampLimit(limit int) int {
	if limit <= 0 {
		return defaultListLimit
	}
	return min(limit, maxListLimit)
}

// PostBlackboard appends an entry to the project's blackboard (PM-H-10). It is
// always shared in the project and never a private memory.
func PostBlackboard(ctx context.Context, tx pgx.Tx, actor authn.Principal, in BlackboardInput) (BlackboardEntry, error) {
	if _, err := requireMember(ctx, tx, actor, in.ProjectID); err != nil {
		return BlackboardEntry{}, err
	}
	if !oneOf(in.Kind, blackboardKinds) || strings.TrimSpace(in.Body) == "" {
		return BlackboardEntry{}, kerr.New(kerr.Invalid, "blackboard entry needs a valid kind and a body")
	}
	if in.TaskID != "" {
		var one int
		err := error(pgx.ErrNoRows)
		if isUUID(in.TaskID) {
			err = tx.QueryRow(ctx, `SELECT 1 FROM task WHERE id = $1 AND project_id = $2`, in.TaskID, in.ProjectID).Scan(&one)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			return BlackboardEntry{}, kerr.New(kerr.Invalid, "task is not a task of this project")
		}
		if err != nil {
			return BlackboardEntry{}, wrap("check task", err)
		}
	}
	refs := in.Refs
	if refs == nil {
		refs = []string{}
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return BlackboardEntry{}, wrap("encode refs", err)
	}
	e, err := scanBlackboard(tx.QueryRow(ctx, `INSERT INTO blackboard (project_id, task_id, kind, body, refs, author)
		VALUES ($1, nullif($2,'')::uuid, $3, $4, $5::jsonb, $6) RETURNING `+blackboardCols,
		in.ProjectID, in.TaskID, in.Kind, in.Body, string(raw), actor.ID))
	return e, wrap("insert blackboard entry", err)
}

// ListBlackboard reads entries of a project after a cursor, oldest first
// (PM-H-10). An empty taskID lists the whole project.
func ListBlackboard(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID, taskID string, afterID int64, limit int) ([]BlackboardEntry, error) {
	if _, err := requireMember(ctx, tx, actor, projectID); err != nil {
		return nil, err
	}
	if taskID != "" && !isUUID(taskID) {
		return []BlackboardEntry{}, nil
	}
	rows, err := tx.Query(ctx, `SELECT `+blackboardCols+` FROM blackboard
		WHERE project_id = $1 AND ($2 = '' OR task_id = nullif($2,'')::uuid) AND id > $3
		ORDER BY id LIMIT $4`, projectID, taskID, afterID, clampLimit(limit))
	if err != nil {
		return nil, wrap("list blackboard", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (BlackboardEntry, error) { return scanBlackboard(row) })
	return out, wrap("list blackboard", err)
}
