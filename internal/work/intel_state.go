package work

import (
	"context"
	"errors"
	"slices"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Waiting reasons of a task (spec S2 C.8): WAITING_DECISION while the set is
// not empty; each event removes exactly its own reason.
const (
	ReasonBudget      = "budget"      // T-H-06 / T-H-07
	ReasonQuestion    = "question"    // T-S2-01 / T-S2-02
	ReasonNonprogress = "nonprogress" // T-S2-01 / T-S2-03
)

// Intel reports whether a mechanism of the Intelligence Layer applies to this
// lease: FULL mode and the feature switched on (spec S2 C.1). Callers also
// check the HOOK enforcement of the holder where the mechanism blocks a tool.
func (l Lease) Intel(feature string) bool {
	return l.Mode == "FULL" && slices.Contains(l.IntelFeatures, feature)
}

// AddWaitingReason applies T-S2-01: the reason joins the task's set and a
// RUNNING task becomes WAITING_DECISION, keeping its lease. A task in another
// state keeps its state; a SUPERSEDED task is left alone. It reports whether
// the reason was new. Trusted scope (owner or store.Runtime).
func AddWaitingReason(ctx context.Context, tx pgx.Tx, taskID, reason, event, actorID string, payload map[string]any) (bool, error) {
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil {
		return false, err
	}
	if t.State == "SUPERSEDED" || slices.Contains(t.WaitingReasons, reason) {
		return false, nil
	}
	if t.State != "RUNNING" && t.State != "WAITING_DECISION" {
		return false, nil // VERIFIED, RUNNABLE, PROPOSED: no T-S2-01 from there
	}
	if _, err := tx.Exec(ctx, `UPDATE task SET state = 'WAITING_DECISION', waiting_reasons = array_append(waiting_reasons, $2),
		updated_at = now() WHERE id = $1`, t.ID, reason); err != nil {
		return false, err
	}
	p := map[string]any{"reason": reason}
	for k, v := range payload {
		p[k] = v
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: event, Actor: actorID, Payload: p})
	return true, err
}

// RemoveWaitingReason applies T-H-07 / T-S2-02 / T-S2-03 for one reason: it
// leaves the set, and when the set is empty a waiting task goes to RUNNING
// with a live lease, else RUNNABLE (spec S2 C.8). It returns the task state
// and whether the reason was in the set. Trusted scope.
//
// The task row is locked by a statement of its own BEFORE the lease is read
// (REV-005 #1): every path that ends a lease holds that lock, so the next
// statement's snapshot already sees a release that committed while this one
// waited. A single UPDATE deciding from its own snapshot could set RUNNING
// from a lease that had just ended.
func RemoveWaitingReason(ctx context.Context, tx pgx.Tx, taskID, reason string) (string, bool, error) {
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil {
		return "", false, err
	}
	if !slices.Contains(t.WaitingReasons, reason) {
		return t.State, false, nil
	}
	var state string
	err = tx.QueryRow(ctx, `UPDATE task t SET waiting_reasons = array_remove(t.waiting_reasons, $2), updated_at = now(),
			state = CASE WHEN t.state = 'WAITING_DECISION' AND cardinality(array_remove(t.waiting_reasons, $2)) = 0
				THEN CASE WHEN `+liveLease+` THEN 'RUNNING' ELSE 'RUNNABLE' END ELSE t.state END
		WHERE t.id = $1 RETURNING t.state`, t.ID, reason).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, notFound("task")
	}
	return state, err == nil, err
}

// LockTask takes the task row lock for a trusted caller that changes the
// intel state of a task in several statements (REV-005 #9) and returns the
// task as it is under that lock. Lock order: the task, then its lease.
func LockTask(ctx context.Context, tx pgx.Tx, taskID string) (Task, error) {
	return loadTask(ctx, tx, taskID, true)
}

// SetPhase records the cost phase of a live lease (spec S2 C.7). Trusted scope.
func SetPhase(ctx context.Context, tx pgx.Tx, leaseID, phase string) error {
	if phase != "DISCOVERY" && phase != "BUILD" && phase != "REWORK" {
		return kerr.New(kerr.Invalid, "unknown phase")
	}
	_, err := tx.Exec(ctx, `UPDATE lease SET intel_phase = $2 WHERE id = $1 AND state = 'ACTIVE'`, leaseID, phase)
	return err
}

// SetMark stores the working tree seen at the first tool call of a lease, once.
func SetMark(ctx context.Context, tx pgx.Tx, leaseID string, mark []byte) error {
	_, err := tx.Exec(ctx, `UPDATE lease SET intel_mark = $2 WHERE id = $1 AND intel_mark IS NULL`, leaseID, mark)
	return err
}

// PushInbox appends text for the next context injected to the holder (C.4 steer).
func PushInbox(ctx context.Context, tx pgx.Tx, leaseID, text string) error {
	_, err := tx.Exec(ctx, `UPDATE lease SET intel_inbox = CASE WHEN intel_inbox = '' THEN $2 ELSE intel_inbox || E'\n\n' || $2 END
		WHERE id = $1 AND state = 'ACTIVE'`, leaseID, text)
	return err
}

// PopInbox returns and clears the pending text of a live lease.
func PopInbox(ctx context.Context, tx pgx.Tx, leaseID string) (string, error) {
	var text string
	err := tx.QueryRow(ctx, `UPDATE lease l SET intel_inbox = '' FROM (SELECT id, intel_inbox FROM lease WHERE id = $1 FOR UPDATE) old
		WHERE l.id = old.id AND l.state = 'ACTIVE' RETURNING old.intel_inbox`, leaseID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// TakeWriteNotice returns and clears the notice refused once at the first write (AC-S2-039).
func TakeWriteNotice(ctx context.Context, tx pgx.Tx, leaseID string) (string, error) {
	var text string
	err := tx.QueryRow(ctx, `UPDATE lease l SET write_notice = '' FROM (SELECT id, write_notice FROM lease WHERE id = $1 FOR UPDATE) old
		WHERE l.id = old.id AND l.state = 'ACTIVE' RETURNING old.write_notice`, leaseID).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// AppendNotice adds text to the pending notice of a live lease (C.5 feedback impact). Trusted scope.
func AppendNotice(ctx context.Context, tx pgx.Tx, leaseID, text string) error {
	_, err := tx.Exec(ctx, `UPDATE lease SET pending_notice = CASE WHEN pending_notice = '' THEN $2 ELSE pending_notice || E'\n\n' || $2 END
		WHERE id = $1 AND state = 'ACTIVE'`, leaseID, text)
	return err
}
