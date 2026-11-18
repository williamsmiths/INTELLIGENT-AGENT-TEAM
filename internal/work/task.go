package work

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

const taskCols = `id, project_id, key, title, desired_outcome, scope, state, revision, repo_path,
	allowed_paths, protected_paths, required_checks, requirement_keys, dependency_versions,
	COALESCE(verified_candidate, ''), base_commit, created_by, created_at, updated_at, waiting_reasons`

func scanTask(row pgx.Row) (Task, error) {
	var t Task
	var checks, versions []byte
	err := row.Scan(&t.ID, &t.ProjectID, &t.Key, &t.Title, &t.DesiredOutcome, &t.Scope, &t.State, &t.Revision,
		&t.RepoPath, &t.AllowedPaths, &t.ProtectedPaths, &checks, &t.RequirementKeys, &versions,
		&t.VerifiedCandidate, &t.BaseCommit, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.WaitingReasons)
	if err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal(checks, &t.RequiredChecks); err != nil {
		return Task{}, err
	}
	if err := json.Unmarshal(versions, &t.DependencyVersions); err != nil {
		return Task{}, err
	}
	return t, nil
}

// loadTask reads one task. With lock it takes the row lock every state change
// of the task starts with (FOR NO KEY UPDATE: foreign-key inserts are not blocked).
// A task hidden by row-level security is simply not found.
func loadTask(ctx context.Context, tx pgx.Tx, taskID string, lock bool) (Task, error) {
	if !validID(taskID) {
		return Task{}, notFound("task")
	}
	q := `SELECT ` + taskCols + ` FROM task WHERE id = $1`
	if lock {
		q += ` FOR NO KEY UPDATE`
	}
	t, err := scanTask(tx.QueryRow(ctx, q, taskID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, notFound("task")
	}
	return t, err
}

// CreateTask creates a PROPOSED task with its TASK budget, whose cost limit is
// copied from the project's default_task_max_cost (PM-H-11; actor scope).
// repo_path, when given, must be a registered root of the same project (AC-H-108).
func CreateTask(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string, in NewTask) (Task, error) {
	if !validID(projectID) {
		return Task{}, notFound("project")
	}
	if err := requireManager(actor, projectID, "project"); err != nil {
		return Task{}, err
	}
	var defaultCost *int64
	err := tx.QueryRow(ctx, `SELECT default_task_max_cost_microusd FROM project WHERE id = $1`, projectID).Scan(&defaultCost)
	if errors.Is(err, pgx.ErrNoRows) {
		return Task{}, notFound("project")
	}
	if err != nil {
		return Task{}, err
	}
	if strings.TrimSpace(in.Key) == "" || strings.TrimSpace(in.Title) == "" {
		return Task{}, kerr.New(kerr.Invalid, "key and title are required")
	}
	if err := validateChecks(in.RequiredChecks); err != nil {
		return Task{}, err
	}
	repo := in.RepoPath
	if repo != "" {
		if c, err := guard.Canonical(repo, ""); err == nil {
			repo = c
		}
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM project_repo WHERE project_id = $1 AND root = $2)`,
			projectID, repo).Scan(&ok); err != nil {
			return Task{}, err
		}
		if !ok {
			return Task{}, kerr.New(kerr.Invalid, "repo_path is not a registered repo root of this project")
		}
	}
	checks, err := json.Marshal(append([]Check{}, in.RequiredChecks...))
	if err != nil {
		return Task{}, err
	}
	t, err := scanTask(tx.QueryRow(ctx, `INSERT INTO task (project_id, key, title, desired_outcome, scope, repo_path,
			allowed_paths, protected_paths, required_checks, requirement_keys, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+taskCols,
		projectID, in.Key, in.Title, in.DesiredOutcome, in.Scope, repo,
		append([]string{}, in.AllowedPaths...), append([]string{}, in.ProtectedPaths...), checks,
		append([]string{}, in.RequirementKeys...), actor.ID))
	if err != nil {
		return Task{}, conflict(err, "a task with this key already exists in the project")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO budget (level, project_id, task_id, max_cost_microusd) VALUES ('TASK',$1,$2,$3)`,
		projectID, t.ID, defaultCost); err != nil {
		return Task{}, err
	}
	var payload map[string]any
	if in.Via != "" { // spec S2 E.1-12; by_name labels the task for its holder, who cannot read principal rows (N1)
		payload = map[string]any{"via": in.Via, "by_name": actor.Name}
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: projectID, TaskID: t.ID, Type: "TASK_CREATED", Actor: actor.ID, Payload: payload})
	return t, err
}

// validateChecks: every required check needs a unique id other than "scope"
// (the runner's built-in check) and a command (CreateTask, delegate policy).
func validateChecks(checks []Check) error {
	seen := map[string]bool{}
	for _, c := range checks {
		if c.ID == "" || c.Command == "" || seen[c.ID] || c.ID == "scope" {
			return kerr.New(kerr.Invalid, "every required check needs a unique id and a command")
		}
		seen[c.ID] = true
	}
	return nil
}

// GetTask returns a task with its live lease and budget (PM-H-12, PM-H-13; actor scope).
func GetTask(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (TaskView, error) {
	t, l, err := viewTask(ctx, tx, actor, taskID)
	if err != nil {
		return TaskView{}, err
	}
	b, err := budgetView(ctx, tx, t)
	if err != nil {
		return TaskView{}, err
	}
	return TaskView{Task: t, Lease: l, Budget: b}, nil
}
