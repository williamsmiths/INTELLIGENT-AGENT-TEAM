package work

import (
	"context"
	"github.com/williamsmiths/kongcowork-harness/internal/intel"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// ReleaseLease is the holder giving its task back (T-H-21, T-H-05). Caller
// must use a trusted scope (store.Runtime): a Worker may update its own lease
// but not the task row, and both must change in one transaction. The function
// authorizes the actor itself: only the principal holding the ACTIVE lease of
// this task gets through, anyone else gets 404 (AC-H-016).
func ReleaseLease(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (Change, error) {
	if _, err := loadTask(ctx, tx, taskID, true); err != nil {
		return Change{}, err
	}
	l, err := taskLease(ctx, tx, taskID, true)
	if err != nil {
		return Change{}, err
	}
	if l == nil || l.PrincipalID != actor.ID {
		return Change{}, notFound("task")
	}
	if err := endLease(ctx, tx, l, "RELEASED", "released", actor.ID, true); err != nil {
		return Change{}, err
	}
	t, err := loadTask(ctx, tx, taskID, false)
	return Change{Task: t, Ended: l}, err
}

// RevokeLease takes a task back from a stuck agent without granting a new
// lease (PM-H-22, T-H-22, T-H-05; owner only; actor scope). AC-H-108.
func RevokeLease(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (Change, error) {
	if err := requireOwner(actor); err != nil {
		return Change{}, err
	}
	if _, err := loadTask(ctx, tx, taskID, true); err != nil {
		return Change{}, err
	}
	l, err := taskLease(ctx, tx, taskID, true)
	if err != nil {
		return Change{}, err
	}
	if l == nil {
		return Change{}, kerr.New(kerr.Conflict, "task has no active lease")
	}
	if err := endLease(ctx, tx, l, "REVOKED", "revoked", actor.ID, true); err != nil {
		return Change{}, err
	}
	t, err := loadTask(ctx, tx, taskID, false)
	return Change{Task: t, Ended: l}, err
}

// ReplaceLease applies T-H-22 with a re-grant: in one transaction the ACTIVE
// lease of the task is REVOKED and a new one is granted to the same principal
// with epoch + 1, the new task revision (0 = the task's current revision),
// context not acknowledged and the notice pending. The task state is not
// touched. A task without a lease is left alone; an overdue lease expires
// instead (T-H-23). Caller must use a trusted scope (owner or store.Runtime).
func ReplaceLease(ctx context.Context, tx pgx.Tx, taskID, reason, notice string, newRevision int) (Change, error) {
	return replaceLease(ctx, tx, taskID, reason, notice, newRevision, "")
}

func replaceLease(ctx context.Context, tx pgx.Tx, taskID, reason, notice string, newRevision int, actorID string) (Change, error) {
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil {
		return Change{}, err
	}
	if newRevision > 0 {
		t.Revision = newRevision
	}
	old, err := taskLease(ctx, tx, taskID, true)
	if err != nil || old == nil {
		return Change{Task: t}, err
	}
	ch := Change{Ended: old}
	if old.due {
		err = endLease(ctx, tx, old, "EXPIRED", "expired", "", true)
	} else if err = endLease(ctx, tx, old, "REVOKED", reason, actorID, false); err == nil {
		// The owner's forced mode and switched-off mechanisms stay; the router runs again (AC-S2-030).
		// A lease that only had the default features gets the default again for its new mode, so a
		// task the router moves to DEEP gets the gate back (spec S2 C.1, REV-008 M-8a).
		opt := IntelOptions{Features: old.IntelFeatures}
		if intel.IsDefaultFeatures(old.IntelFeatures, old.IntelMode) {
			opt.Features = nil
		}
		if old.IntelForced {
			opt.Mode = old.IntelMode
		}
		ch.Granted, err = grantLease(ctx, tx, t, old.PrincipalID, old.Mode, notice, reason, actorID, opt)
	}
	if err != nil {
		return Change{}, err
	}
	ch.Task, err = loadTask(ctx, tx, taskID, false)
	return ch, err
}
