package work_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AC-H-034: only the owner raises a budget; a waiting task continues, or is RUNNABLE when its lease ended.
func TestAC_H_034_OnlyOwnerRaises(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")
	held, gone := e.running(a.ID, "T-1", w), e.running(a.ID, "T-2", v)
	for _, agent := range []authn.Principal{w, v} {
		if res, err := e.usage(agent, "rec-"+agent.Name, 1.20, time.Now()); err != nil || !res.Exhausted {
			t.Fatalf("usage of %s: %+v, %v", agent.Name, res, err)
		}
	}
	for _, actor := range []authn.Principal{lead, w} {
		wantKind(t, e.as(actor, func(tx pgx.Tx) error {
			_, err := work.SetTaskBudget(e.ctx, tx, actor, held.ID, work.Limits{MaxCost: cost(5)})
			return err
		}), kerr.Forbidden, "raise by "+actor.Name)
	}
	if b := e.budget(held.ID); b.Task.State != "EXHAUSTED" || *b.Task.Limits.MaxCost != usd(1) {
		t.Fatalf("a refused raise changed the budget: %+v", b.Task)
	}
	e.must(e.rt(func(tx pgx.Tx) error { // V's lease ends while its task waits
		_, err := work.ReleaseLease(e.ctx, tx, v, gone.ID)
		return err
	}))
	for id, want := range map[string]string{held.ID: "RUNNING", gone.ID: "RUNNABLE"} {
		e.must(e.as(e.owner, func(tx pgx.Tx) error {
			b, err := work.SetTaskBudget(e.ctx, tx, e.owner, id, work.Limits{MaxCost: cost(2)})
			if err == nil && (b.Task.State != "OK" || b.Effective != "OK") {
				t.Fatalf("budget after raise = %+v", b)
			}
			return err
		}))
		if st := e.view(id).Task.State; st != want {
			t.Fatalf("task is %s after the raise, want %s", st, want)
		}
	}
	if n := scalar[int](e, `SELECT count(*) FROM journal WHERE type = 'BUDGET_RAISED' AND project_id = $1`, a.ID); n != 2 {
		t.Fatalf("%d BUDGET_RAISED events, want 2", n)
	}
}

// T-H-03 and T-H-04 as the verify flow and the post-tool hook drive them (AC-H-040, AC-H-043, task side).
func TestT_H_03_T_H_04_VerifiedAndStale(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 100, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	evidence := func(check, candidate, result string, exit int) {
		e.must(e.rt(func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `INSERT INTO evidence (project_id, task_id, task_revision, candidate_hash, check_id,
					command, exit_code, result, runner_id, started_at, finished_at)
				SELECT project_id, id, revision, $2, $3, 'true', $5, $4, 'test-runner', clock_timestamp(), clock_timestamp()
				FROM task WHERE id = $1`, task.ID, candidate, check, result, exit)
			return err
		}))
	}
	verify := func(candidate string) (out work.Task, err error) {
		err = e.rt(func(tx pgx.Tx) (err error) {
			out, err = work.MarkVerified(e.ctx, tx, task.ID, candidate)
			return err
		})
		return out, err
	}
	stale := func() (out work.Task) {
		e.must(e.rt(func(tx pgx.Tx) (err error) {
			out, err = work.MarkStale(e.ctx, tx, task.ID)
			return err
		}))
		return out
	}
	// T-H-03 needs PASS for every required check and for "scope", on this candidate.
	evidence("unit", "c1", "PASS", 0)
	_, err := verify("c1")
	wantKind(t, err, kerr.Conflict, "no scope evidence")
	evidence("scope", "c1", "FAIL", 1)
	_, err = verify("c1")
	wantKind(t, err, kerr.Conflict, "scope FAIL")
	evidence("scope", "c1", "PASS", 0)
	_, err = verify("c2")
	wantKind(t, err, kerr.Conflict, "evidence is on another candidate")
	if st := e.view(task.ID).Task.State; st != "RUNNING" {
		t.Fatalf("task is %s after refused verifies", st)
	}
	if got := stale(); got.State != "RUNNING" { // T-H-04 only applies to VERIFIED
		t.Fatalf("stale on a RUNNING task: %s", got.State)
	}
	done, err := verify("c1")
	e.must(err)
	again, err := verify("c1")
	e.must(err)
	if done.State != "VERIFIED" || done.VerifiedCandidate != "c1" || again.UpdatedAt != done.UpdatedAt {
		t.Fatalf("verified: %+v", done)
	}
	_, err = verify("c2")
	wantKind(t, err, kerr.Conflict, "another candidate while VERIFIED")

	// The holder writes a file: back to RUNNING, the verified candidate is gone.
	if got := stale(); got.State != "RUNNING" || got.VerifiedCandidate != "" {
		t.Fatalf("stale with a lease: %+v", got)
	}
	// VERIFIED does not change when the budget runs out (T-H-06), but going stale then waits for the owner.
	_, err = verify("c1")
	e.must(err)
	if res, err := e.usage(w, "rec-1", 2.00, time.Now()); err != nil || res.Exhausted || res.TaskBudget != "EXHAUSTED" {
		t.Fatalf("usage on a VERIFIED task: %+v, %v", res, err)
	}
	if st := e.view(task.ID).Task.State; st != "VERIFIED" {
		t.Fatalf("task is %s, want VERIFIED", st)
	}
	if got := stale(); got.State != "WAITING_DECISION" {
		t.Fatalf("stale with an exhausted budget: %s", got.State)
	}
	// Without a lease a stale task is RUNNABLE.
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetTaskBudget(e.ctx, tx, e.owner, task.ID, work.Limits{MaxCost: cost(50)})
		return err
	}))
	_, err = verify("c1")
	e.must(err)
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := work.ReleaseLease(e.ctx, tx, w, task.ID)
		return err
	}))
	if got := stale(); got.State != "RUNNABLE" || got.VerifiedCandidate != "" {
		t.Fatalf("stale without a lease: %+v", got)
	}
	if n := scalar[int](e, `SELECT count(*) FROM journal WHERE type = 'EVIDENCE_STALE' AND task_id = $1`, task.ID); n != 3 {
		t.Fatalf("%d EVIDENCE_STALE events, want 3", n)
	}
}
