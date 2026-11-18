package work

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// ReadyTask applies T-H-01 (PM-H-11; actor scope): every condition is checked
// and the missing ones are named in the 409 (AC-H-011). On success the current
// version of every requirement key is pinned in dependency_versions (AC-H-010).
func ReadyTask(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (Task, error) {
	t, err := manageTask(ctx, tx, actor, taskID)
	if err != nil {
		return Task{}, err
	}
	if t.State != "PROPOSED" {
		return Task{}, kerr.New(kerr.Conflict, "task is %s, only a PROPOSED task can become RUNNABLE", t.State)
	}
	var missing []string
	if strings.TrimSpace(t.DesiredOutcome) == "" {
		missing = append(missing, "desired_outcome")
	}
	if len(t.AllowedPaths) == 0 {
		missing = append(missing, "allowed_paths")
	}
	if len(t.RequiredChecks) == 0 {
		missing = append(missing, "required_checks")
	}
	var repoOK bool
	var projectMax, taskMax *int64
	if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM project_repo WHERE project_id = $1 AND root = $2),
			(SELECT max_cost_microusd FROM budget WHERE level = 'PROJECT' AND project_id = $1),
			(SELECT max_cost_microusd FROM budget WHERE level = 'TASK' AND task_id = $3)`,
		t.ProjectID, t.RepoPath, t.ID).Scan(&repoOK, &projectMax, &taskMax); err != nil {
		return Task{}, err
	}
	if !repoOK {
		missing = append(missing, "repo_path (a registered repo root of the project)")
	}
	if projectMax == nil {
		missing = append(missing, "PROJECT budget max_cost")
	}
	if taskMax == nil {
		missing = append(missing, "TASK budget max_cost")
	}
	// Only the current AUTHORITATIVE + TO_BE version at PROJECT level of the same project can be pinned.
	rows, err := tx.Query(ctx, `SELECT key, version FROM knowledge
		WHERE project_id = $1 AND level = 'PROJECT' AND is_current AND trust = 'AUTHORITATIVE' AND aspect = 'TO_BE'
		  AND key = ANY($2)`, t.ProjectID, t.RequirementKeys)
	if err != nil {
		return Task{}, err
	}
	versions := map[string]int{}
	var key string
	var version int
	if _, err := pgx.ForEachRow(rows, []any{&key, &version}, func() error {
		versions[key] = version
		return nil
	}); err != nil {
		return Task{}, err
	}
	for _, k := range t.RequirementKeys {
		if _, ok := versions[k]; !ok {
			missing = append(missing, "requirement "+k+" (current AUTHORITATIVE TO_BE)")
		}
	}
	if len(missing) > 0 {
		return Task{}, kerr.New(kerr.Conflict, "task is not ready, missing: %s", strings.Join(missing, "; "))
	}
	pinned, err := json.Marshal(versions)
	if err != nil {
		return Task{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE task SET state = 'RUNNABLE', dependency_versions = $2, updated_at = now()
		WHERE id = $1`, t.ID, pinned); err != nil {
		return Task{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "TASK_READY",
		Actor: actor.ID, Payload: map[string]any{"revision": t.Revision}}); err != nil {
		return Task{}, err
	}
	return loadTask(ctx, tx, t.ID, false)
}

// SupersedeTask applies T-H-08 (PM-H-11; actor scope): any open task becomes
// SUPERSEDED and its ACTIVE lease is revoked with reason "superseded", without
// a new lease (AC-H-104).
func SupersedeTask(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (Change, error) {
	t, err := manageTask(ctx, tx, actor, taskID)
	if err != nil {
		return Change{}, err
	}
	if t.State == "SUPERSEDED" {
		return Change{}, kerr.New(kerr.Conflict, "task is already SUPERSEDED")
	}
	l, err := taskLease(ctx, tx, t.ID, true)
	if err != nil {
		return Change{}, err
	}
	if l != nil {
		if err := endLease(ctx, tx, l, "REVOKED", "superseded", actor.ID, false); err != nil {
			return Change{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE task SET state = 'SUPERSEDED', updated_at = now() WHERE id = $1`, t.ID); err != nil {
		return Change{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "TASK_SUPERSEDED",
		Actor: actor.ID, Payload: map[string]any{"from": t.State}}); err != nil {
		return Change{}, err
	}
	t, err = loadTask(ctx, tx, t.ID, false)
	return Change{Task: t, Ended: l}, err
}
