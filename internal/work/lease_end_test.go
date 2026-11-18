package work_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// overdue moves expires_at of the task's lease into the past, as time would.
func (e *env) overdue(taskID string) {
	e.t.Helper()
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE lease SET expires_at = now() - interval '1 minute'
			WHERE task_id = $1 AND state = 'ACTIVE'`, taskID)
		return err
	}))
}

// evidence seeds PASS evidence the way the runner writes it, then verifies the task (T-H-03).
func (e *env) verified(task work.Task, candidate string) work.Task {
	e.t.Helper()
	var out work.Task
	e.must(e.rt(func(tx pgx.Tx) (err error) {
		for _, check := range []string{"unit", "scope"} {
			if _, err := tx.Exec(e.ctx, `INSERT INTO evidence (project_id, task_id, task_revision, candidate_hash,
					check_id, command, exit_code, result, runner_id, started_at, finished_at)
				SELECT project_id, id, revision, $2, $3, 'true', 0, 'PASS', 'test-runner', now(), now()
				FROM task WHERE id = $1`, task.ID, candidate, check); err != nil {
				return err
			}
		}
		out, err = work.MarkVerified(e.ctx, tx, task.ID, candidate)
		return err
	}))
	return out
}

// AC-H-019: an overdue lease expires the moment it is read, and by the sweep;
// VERIFIED and WAITING_DECISION keep their state when the lease ends.
func TestAC_H_019_LeaseExpiry(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)

	// A hook event of the holder extends the lease by 8 hours and sees it.
	e.must(e.rt(func(tx pgx.Tx) error {
		held, err := work.ActiveLease(e.ctx, tx, w.ID)
		if err != nil {
			return err
		}
		if held == nil || held.Task.ID != task.ID || held.Lease.ProjectID != a.ID {
			t.Fatalf("active lease = %+v", held)
		}
		return work.TouchLease(e.ctx, tx, held.Lease.ID)
	}))
	e.overdue(task.ID)
	// The lease read of the hook: no lease any more (the hook answers "deny: needs a lease").
	e.must(e.rt(func(tx pgx.Tx) error {
		held, err := work.ActiveLease(e.ctx, tx, w.ID)
		if held != nil {
			t.Fatalf("overdue lease still returned: %+v", held.Lease)
		}
		return err
	}))
	if st := scalar[string](e, `SELECT state FROM lease WHERE task_id = $1`, task.ID); st != "EXPIRED" {
		t.Fatalf("lease is %s, want EXPIRED", st)
	}
	if st := e.view(task.ID).Task.State; st != "RUNNABLE" {
		t.Fatalf("task is %s, want RUNNABLE", st)
	}

	// The sweep ends overdue leases nobody reads; an overdue lease cannot be revived by a heartbeat,
	// is not shown as live, and does not block a new assign.
	ch, err := e.assign(e.owner, task.ID, w.ID, "")
	e.must(err)
	e.overdue(task.ID)
	wantKind(t, e.as(w, func(tx pgx.Tx) error { return work.TouchLease(e.ctx, tx, ch.Granted.ID) }),
		kerr.NotFound, "heartbeat on an overdue lease")
	if e.view(task.ID).Lease != nil {
		t.Fatal("overdue lease shown as live")
	}
	e.must(e.rt(func(tx pgx.Tx) error {
		ended, err := work.ExpireLeases(e.ctx, tx)
		if err == nil && (len(ended) != 1 || ended[0].State != "EXPIRED" || ended[0].Epoch != 2) {
			t.Fatalf("sweep ended %+v", ended)
		}
		return err
	}))
	if st := e.view(task.ID).Task.State; st != "RUNNABLE" {
		t.Fatalf("task is %s after the sweep, want RUNNABLE", st)
	}
	_, err = e.assign(e.owner, task.ID, w.ID, "")
	e.must(err)
	e.overdue(task.ID)
	ch, err = e.assign(e.owner, task.ID, v.ID, "") // the assign reads the overdue lease and ends it first
	e.must(err)
	if st := scalar[string](e, `SELECT state FROM lease WHERE task_id = $1 AND epoch = 3`, task.ID); st != "EXPIRED" ||
		ch.Granted.Epoch != 4 || ch.Task.State != "RUNNING" {
		t.Fatalf("assign over an overdue lease: old lease %s, new %+v", st, ch.Granted)
	}

	// VERIFIED keeps its state and its verified candidate when the holder releases.
	done := e.verified(ch.Task, "tree-c1")
	if done.State != "VERIFIED" || done.VerifiedCandidate != "tree-c1" {
		t.Fatalf("verified: %+v", done)
	}
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := work.ReleaseLease(e.ctx, tx, v, task.ID)
		return err
	}))
	if got := e.view(task.ID); got.Task.State != "VERIFIED" || got.Task.VerifiedCandidate != "tree-c1" || got.Lease != nil {
		t.Fatalf("after release of a VERIFIED task: %+v", got)
	}

	// WAITING_DECISION keeps its state when the lease expires.
	waiting := e.running(a.ID, "T-2", w)
	if res, err := e.usage(w, "r-1", 1.00, time.Now()); err != nil || !res.Exhausted {
		t.Fatalf("usage: %+v, %v", res, err)
	}
	e.overdue(waiting.ID)
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := work.ExpireLeases(e.ctx, tx)
		return err
	}))
	if got := e.view(waiting.ID); got.Task.State != "WAITING_DECISION" || got.Lease != nil {
		t.Fatalf("after expiry of a WAITING_DECISION task: %+v", got)
	}
}

// AC-H-108 (second part): no assign while a budget is EXHAUSTED; only the owner
// revokes a lease, and no new lease is granted.
func TestAC_H_108_NoAssignWhenExhausted_OwnerRevokes(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.runnable(a.ID, "T-1")
	limits := func(projectMax, taskMax float64) {
		e.must(e.as(e.owner, func(tx pgx.Tx) error {
			if _, err := work.SetTaskBudget(e.ctx, tx, e.owner, task.ID, work.Limits{MaxCost: cost(taskMax)}); err != nil {
				return err
			}
			_, err := work.SetProjectLimits(e.ctx, tx, e.owner, a.ID, work.ProjectLimits{
				DefaultTaskMaxCost: cost(1), Budget: work.Limits{MaxCost: cost(projectMax)}})
			return err
		}))
	}
	for what, l := range map[string][2]float64{"TASK budget EXHAUSTED": {10, 0}, "PROJECT budget EXHAUSTED": {0, 1}} {
		limits(l[0], l[1])
		_, err := e.assign(e.owner, task.ID, w.ID, "")
		wantKind(t, err, kerr.Conflict, what)
	}
	e.must(e.as(e.owner, func(tx pgx.Tx) error { // SYSTEM EXHAUSTED blocks too (T-H-02)
		_, err := work.SetSystemBudget(e.ctx, tx, e.owner, work.Limits{MaxTurns: ptr(int64(0))})
		return err
	}))
	limits(10, 1)
	_, err := e.assign(e.owner, task.ID, w.ID, "")
	wantKind(t, err, kerr.Conflict, "SYSTEM budget EXHAUSTED")
	if v := e.view(task.ID); v.Task.State != "RUNNABLE" || v.Lease != nil {
		t.Fatalf("a refused assign changed the task: %+v", v)
	}
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetSystemBudget(e.ctx, tx, e.owner, work.Limits{})
		return err
	}))
	_, err = e.assign(e.owner, task.ID, w.ID, "")
	e.must(err)

	wantKind(t, e.as(lead, func(tx pgx.Tx) error {
		_, err := work.RevokeLease(e.ctx, tx, lead, task.ID)
		return err
	}), kerr.Forbidden, "lead revokes")
	wantKind(t, e.as(w, func(tx pgx.Tx) error {
		_, err := work.RevokeLease(e.ctx, tx, w, task.ID)
		return err
	}), kerr.Forbidden, "worker revokes")
	var ch work.Change
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		ch, err = work.RevokeLease(e.ctx, tx, e.owner, task.ID)
		return err
	}))
	if ch.Ended.State != "REVOKED" || ch.Ended.Reason != "revoked" || ch.Granted != nil || ch.Task.State != "RUNNABLE" {
		t.Fatalf("revoke: %+v", ch)
	}
	if n := scalar[int](e, `SELECT count(*) FROM lease WHERE task_id = $1 AND state = 'ACTIVE'`, task.ID); n != 0 {
		t.Fatalf("%d active leases after revoke", n)
	}
	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.RevokeLease(e.ctx, tx, e.owner, task.ID)
		return err
	}), kerr.Conflict, "revoke without a lease")
}
