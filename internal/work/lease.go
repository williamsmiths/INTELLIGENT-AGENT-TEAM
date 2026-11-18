package work

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

const leaseCols = `id, project_id, task_id, principal_id, epoch, task_revision, repo_path, mode, state,
	context_acked, pending_notice, wrote_files, reason, granted_at, expires_at, ended_at, expires_at < now(),
	intel_mode, intel_signals, intel_features, intel_forced, intel_phase, intel_mark, intel_inbox, write_notice`

func scanLease(row pgx.Row) (*Lease, error) {
	var l Lease
	err := row.Scan(&l.ID, &l.ProjectID, &l.TaskID, &l.PrincipalID, &l.Epoch, &l.TaskRevision, &l.RepoPath, &l.Mode,
		&l.State, &l.ContextAcked, &l.PendingNotice, &l.WroteFiles, &l.Reason, &l.GrantedAt, &l.ExpiresAt, &l.EndedAt, &l.due,
		&l.IntelMode, &l.IntelSignals, &l.IntelFeatures, &l.IntelForced, &l.IntelPhase, &l.IntelMark, &l.IntelInbox, &l.WriteNotice)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &l, nil
}

// taskLease returns the ACTIVE lease of a task, or nil. Lock it only after the task row.
func taskLease(ctx context.Context, tx pgx.Tx, taskID string, lock bool) (*Lease, error) {
	q := `SELECT ` + leaseCols + ` FROM lease WHERE task_id = $1 AND state = 'ACTIVE'`
	if lock {
		q += ` FOR NO KEY UPDATE`
	}
	return scanLease(tx.QueryRow(ctx, q, taskID))
}

// endLease moves an ACTIVE lease to RELEASED, REVOKED or EXPIRED. With settle
// it also applies T-H-05: a RUNNING task goes back to RUNNABLE, while VERIFIED
// and WAITING_DECISION keep their state (AC-H-019). The caller holds the task row lock.
func endLease(ctx context.Context, tx pgx.Tx, l *Lease, state, reason, actorID string, settle bool) error {
	err := tx.QueryRow(ctx, `UPDATE lease SET state = $2, reason = $3, ended_at = now()
		WHERE id = $1 AND state = 'ACTIVE' RETURNING ended_at`, l.ID, state, reason).Scan(&l.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return kerr.New(kerr.Conflict, "lease is no longer active")
	}
	if err != nil {
		return err
	}
	l.State, l.Reason = state, reason
	if settle {
		if _, err := tx.Exec(ctx, `UPDATE task SET state = 'RUNNABLE', updated_at = now()
			WHERE id = $1 AND state = 'RUNNING'`, l.TaskID); err != nil {
			return err
		}
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: l.ProjectID, TaskID: l.TaskID, Type: "LEASE_ENDED",
		Actor: actorID, Payload: map[string]any{"lease_id": l.ID, "principal_id": l.PrincipalID, "epoch": l.Epoch,
			"state": state, "reason": reason}})
	return err
}

// expireTask ends the lease of one task when it is overdue (T-H-23) and returns it.
func expireTask(ctx context.Context, tx pgx.Tx, taskID string) (*Lease, error) {
	if _, err := loadTask(ctx, tx, taskID, true); err != nil {
		return nil, err
	}
	l, err := taskLease(ctx, tx, taskID, true)
	if err != nil || l == nil || !l.due {
		return nil, err
	}
	return l, endLease(ctx, tx, l, "EXPIRED", "expired", "", true)
}

// ExpireLeases is the periodic sweep of T-H-23: every ACTIVE lease past
// expires_at becomes EXPIRED and its RUNNING task RUNNABLE. Tasks are locked
// in id order. Caller must use store.Runtime scope.
func ExpireLeases(ctx context.Context, tx pgx.Tx) ([]Lease, error) {
	rows, err := tx.Query(ctx,
		`SELECT task_id::text FROM lease WHERE state = 'ACTIVE' AND expires_at < now() ORDER BY task_id`)
	if err != nil {
		return nil, err
	}
	taskIDs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var ended []Lease
	for _, id := range taskIDs {
		l, err := expireTask(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		if l != nil {
			ended = append(ended, *l)
		}
	}
	return ended, nil
}

// ActiveLease returns the live lease of a principal with its task, or nil. It
// is the lease read of the hooks, so it applies the lazy expiry of T-H-23: an
// overdue lease is ended here and nil is returned (AC-H-019). The project and
// task of a request always come from this row, never from the request body
// (AC-H-083). Caller must use store.Runtime scope.
func ActiveLease(ctx context.Context, tx pgx.Tx, principalID string) (*Held, error) {
	if !validID(principalID) {
		return nil, nil
	}
	for range 2 {
		l, err := scanLease(tx.QueryRow(ctx,
			`SELECT `+leaseCols+` FROM lease WHERE principal_id = $1 AND state = 'ACTIVE'`, principalID))
		if err != nil || l == nil {
			return nil, err
		}
		if !l.due {
			t, err := loadTask(ctx, tx, l.TaskID, false)
			if err != nil {
				return nil, err
			}
			return &Held{Lease: *l, Task: t}, nil
		}
		if _, err := expireTask(ctx, tx, l.TaskID); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// TouchLease is the heartbeat of a hook event: the lease lives 8 more hours
// (T-H-23). An ended or overdue lease is not revived. Holder's scope or store.Runtime.
func TouchLease(ctx context.Context, tx pgx.Tx, leaseID string) error {
	return updateLive(ctx, tx, leaseID, `expires_at = now() + interval '8 hours'`, ` AND expires_at >= now()`)
}

// MarkWrote records that the holder wrote a file under this lease (stop hook,
// AC-H-027). Holder's scope or store.Runtime.
func MarkWrote(ctx context.Context, tx pgx.Tx, leaseID string) error {
	return updateLive(ctx, tx, leaseID, `wrote_files = true`, ``)
}

// AckContext records that the holder received the current context packet and
// returns the pending notice (verbatim feedback or change summary) it cleared,
// so the caller delivers it exactly once (AC-H-020, AC-H-050). Holder's scope or store.Runtime.
func AckContext(ctx context.Context, tx pgx.Tx, leaseID string) (string, error) {
	if !validID(leaseID) {
		return "", notFound("lease")
	}
	var notice string
	err := tx.QueryRow(ctx, `SELECT pending_notice FROM lease WHERE id = $1 AND state = 'ACTIVE' FOR NO KEY UPDATE`,
		leaseID).Scan(&notice)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", notFound("lease")
	}
	if err != nil {
		return "", err
	}
	return notice, updateLive(ctx, tx, leaseID, `context_acked = true, pending_notice = ''`, ``)
}

func updateLive(ctx context.Context, tx pgx.Tx, leaseID, set, cond string) error {
	if !validID(leaseID) {
		return notFound("lease")
	}
	tag, err := tx.Exec(ctx, `UPDATE lease SET `+set+` WHERE id = $1 AND state = 'ACTIVE'`+cond, leaseID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return notFound("lease")
	}
	return nil
}
