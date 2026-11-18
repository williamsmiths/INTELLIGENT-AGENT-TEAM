package work

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// liveLease is true while the task has an ACTIVE lease that is not overdue (T-H-23).
const liveLease = `EXISTS (SELECT 1 FROM lease l WHERE l.task_id = t.id AND l.state = 'ACTIVE' AND l.expires_at >= now())`

// exhaustTask applies T-H-06: a RUNNING task whose TASK budget is EXHAUSTED
// waits for the owner and keeps its lease. The event names the enforcement of
// the holder, so a SUPERVISED agent is never reported as blocked (AC-H-035).
// A task already waiting for another reason gets "budget" added to its set
// (spec S2 C.8), so it stays waiting until both are resolved.
func exhaustTask(ctx context.Context, tx pgx.Tx, t Task, actorID string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE task SET state = 'WAITING_DECISION', updated_at = now(),
			waiting_reasons = array_append(waiting_reasons, 'budget')
		WHERE id = $1 AND state IN ('RUNNING','WAITING_DECISION') AND NOT ('budget' = ANY(waiting_reasons))`, t.ID)
	if err != nil || tag.RowsAffected() == 0 {
		return false, err
	}
	var leaseID, enforcement string
	err = tx.QueryRow(ctx, `SELECT l.id, lower(p.enforcement) FROM lease l JOIN principal p ON p.id = l.principal_id
		WHERE l.task_id = $1 AND l.state = 'ACTIVE'`, t.ID).Scan(&leaseID, &enforcement)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "BUDGET_EXHAUSTED",
		Actor: actorID, Payload: map[string]any{"level": "TASK", "lease_id": leaseID, "enforcement": enforcement}})
	return true, err
}

// resumeTask applies T-H-07 once the TASK budget is no longer EXHAUSTED: the
// reason "budget" leaves the set; with an empty set the task is RUNNING when
// the lease is still live, RUNNABLE when it ended while waiting (AC-H-034),
// otherwise it keeps waiting for its other reasons (spec S2 C.8). The task
// row is locked before the lease is read (RemoveWaitingReason, REV-005 #1).
func resumeTask(ctx context.Context, tx pgx.Tx, t Task, actorID string) error {
	state, had, err := RemoveWaitingReason(ctx, tx, t.ID, ReasonBudget)
	if err != nil || !had {
		return err
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "BUDGET_RAISED",
		Actor: actorID, Payload: map[string]any{"state": state}})
	return err
}

// staleTask applies T-H-04 in one statement: a VERIFIED task loses its
// verified candidate and goes to RUNNING (live lease), WAITING_DECISION (live
// lease, TASK budget EXHAUSTED) or RUNNABLE (no lease). Other states do not change.
func staleTask(ctx context.Context, tx pgx.Tx, t Task, reason, actorID string) (bool, error) {
	var state string
	err := tx.QueryRow(ctx, `UPDATE task t SET verified_candidate = NULL, updated_at = now(),
			state = CASE
				WHEN NOT `+liveLease+` THEN 'RUNNABLE'
				WHEN EXISTS (SELECT 1 FROM budget b WHERE b.level = 'TASK' AND b.task_id = t.id AND b.state = 'EXHAUSTED')
					THEN 'WAITING_DECISION'
				ELSE 'RUNNING' END,
			waiting_reasons = CASE WHEN `+liveLease+` AND EXISTS (SELECT 1 FROM budget b WHERE b.level = 'TASK'
					AND b.task_id = t.id AND b.state = 'EXHAUSTED') AND NOT ('budget' = ANY(t.waiting_reasons))
				THEN array_append(t.waiting_reasons, 'budget') ELSE t.waiting_reasons END
		WHERE t.id = $1 AND t.state = 'VERIFIED' RETURNING t.state`, t.ID).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "EVIDENCE_STALE",
		Actor: actorID, Payload: map[string]any{"reason": reason, "state": state}})
	return true, err
}

// MarkStale applies T-H-04 for the Runtime: the holder just wrote a file, or a
// new verify saw another candidate. A task that is not VERIFIED is returned
// unchanged. Caller must use store.Runtime scope.
func MarkStale(ctx context.Context, tx pgx.Tx, taskID string) (Task, error) {
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil {
		return Task{}, err
	}
	if _, err := staleTask(ctx, tx, t, "candidate_changed", ""); err != nil {
		return Task{}, err
	}
	return loadTask(ctx, tx, taskID, false)
}

// MarkVerified applies T-H-03 for the Runtime and enforces its condition next
// to the transition: every required check and the "scope" check must have its
// newest evidence PASS on this candidate and on the current revision, so a
// feedback that arrived during the run cannot be verified over. Verifying the
// already verified candidate again is a no-op. Caller must use store.Runtime scope.
func MarkVerified(ctx context.Context, tx pgx.Tx, taskID, candidate string) (Task, error) {
	if candidate == "" {
		return Task{}, kerr.New(kerr.Invalid, "candidate hash is required")
	}
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil {
		return Task{}, err
	}
	if t.State == "VERIFIED" && t.VerifiedCandidate == candidate {
		return t, nil
	}
	if t.State != "RUNNING" {
		return Task{}, kerr.New(kerr.Conflict, "task is %s, only a RUNNING task can become VERIFIED", t.State)
	}
	checks := []string{"scope"}
	for _, c := range t.RequiredChecks {
		checks = append(checks, c.ID)
	}
	rows, err := tx.Query(ctx, `SELECT c.id FROM unnest($1::text[]) AS c(id)
		WHERE COALESCE((SELECT e.result FROM evidence e
			WHERE e.task_id = $2 AND e.candidate_hash = $3 AND e.task_revision = $4 AND e.check_id = c.id
			ORDER BY e.finished_at DESC, e.started_at DESC LIMIT 1), '') <> 'PASS'`,
		checks, t.ID, candidate, t.Revision)
	if err != nil {
		return Task{}, err
	}
	missing, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return Task{}, err
	}
	if len(missing) > 0 {
		return Task{}, kerr.New(kerr.Conflict, "no PASS evidence on this candidate and revision %d for: %s",
			t.Revision, strings.Join(missing, ", "))
	}
	if _, err := tx.Exec(ctx, `UPDATE task SET state = 'VERIFIED', verified_candidate = $2, updated_at = now()
		WHERE id = $1`, t.ID, candidate); err != nil {
		return Task{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "TASK_VERIFIED",
		Payload: map[string]any{"candidate_hash": candidate, "revision": t.Revision}}); err != nil {
		return Task{}, err
	}
	return loadTask(ctx, tx, taskID, false)
}
