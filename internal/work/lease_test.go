package work_test

import (
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AC-H-012: a Worker cannot assign; a Lead of another project does not even see the task.
// AC-H-017: a Lead cannot assign a task to itself.
func TestAC_H_012_AC_H_017_WhoMayAssign(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	b := e.project("beta", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	w := e.agent("w", a.ID, "worker", "HOOK")
	leadB := e.agent("lead-b", b.ID, "lead", "HOOK")
	outsider := e.agent("x", b.ID, "worker", "HOOK")
	task := e.runnable(a.ID, "T-1")

	_, err := e.assign(w, task.ID, w.ID, "")
	wantKind(t, err, kerr.Forbidden, "AC-H-012 worker assigns")
	_, err = e.assign(leadB, task.ID, w.ID, "")
	wantKind(t, err, kerr.NotFound, "AC-H-012 lead of another project assigns")
	_, err = e.assign(lead, task.ID, lead.ID, "")
	wantKind(t, err, kerr.Forbidden, "AC-H-017 lead assigns to itself")
	_, err = e.assign(lead, task.ID, outsider.ID, "")
	wantKind(t, err, kerr.Invalid, "assignee is not a member of the project")
	_, err = e.assign(lead, task.ID, w.ID, "TURBO")
	wantKind(t, err, kerr.Invalid, "unknown mode")
	if v := e.view(task.ID); v.Task.State != "RUNNABLE" || v.Lease != nil {
		t.Fatalf("a refused assign changed the task: %+v", v)
	}
	ch, err := e.assign(lead, task.ID, w.ID, "CONTEXT")
	e.must(err)
	if ch.Task.State != "RUNNING" || ch.Granted.PrincipalID != w.ID || ch.Granted.Mode != "CONTEXT" {
		t.Fatalf("AC-H-017 lead assigns to a worker: %+v", ch)
	}
}

// AC-H-013: an agent holds one lease at a time; assigning a running task again changes nothing.
func TestAC_H_013_OneLeasePerAgent(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	b := e.project("beta", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.AddMembership(e.ctx, tx, e.owner, w.ID, b.ID, "worker")
		return err
	}))
	task := e.running(a.ID, "T-1", w)
	for _, p := range []string{a.ID, b.ID} { // also across projects: one lease in the whole system (§0)
		u := e.runnable(p, "U-1")
		_, err := e.assign(e.owner, u.ID, w.ID, "")
		wantKind(t, err, kerr.Conflict, "assign a second task to a busy agent")
		if st := e.view(u.ID).Task.State; st != "RUNNABLE" {
			t.Fatalf("refused task became %s", st)
		}
	}
	_, err := e.assign(e.owner, task.ID, w.ID, "")
	wantKind(t, err, kerr.Conflict, "assign a running task again")
	if n := scalar[int](e, `SELECT max(epoch) FROM lease WHERE task_id = $1`, task.ID); n != 1 {
		t.Fatalf("epoch grew to %d", n)
	}
}

// AC-H-014: two assigns of one task at the same time: exactly one wins.
func TestAC_H_014_ConcurrentAssign(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 100, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	for round := range 5 {
		name := string(rune('a' + round))
		w := e.agent("w"+name, a.ID, "worker", "HOOK")
		v := e.agent("v"+name, a.ID, "worker", "HOOK")
		task := e.runnable(a.ID, "T-"+name)
		errs := make([]error, 2)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i, c := range []struct{ actor, to authn.Principal }{{e.owner, w}, {lead, v}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = e.assign(c.actor, task.ID, c.to.ID, "")
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for _, err := range errs {
			if err == nil {
				won++
			} else {
				wantKind(t, err, kerr.Conflict, "losing assign")
			}
		}
		active := scalar[int](e, `SELECT count(*) FROM lease WHERE task_id = $1 AND state = 'ACTIVE'`, task.ID)
		total := scalar[int](e, `SELECT count(*) FROM lease WHERE task_id = $1`, task.ID)
		if won != 1 || active != 1 || total != 1 || e.view(task.ID).Task.State != "RUNNING" {
			t.Fatalf("round %d: %d winners, %d active of %d leases, errors %v", round, won, active, total, errs)
		}
	}
}

// AC-H-016: only the holder releases; the task goes back to RUNNABLE.
func TestAC_H_016_Release(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	task := e.running(a.ID, "T-1", w)
	release := func(p authn.Principal) (ch work.Change, err error) {
		err = e.rt(func(tx pgx.Tx) (err error) {
			ch, err = work.ReleaseLease(e.ctx, tx, p, task.ID)
			return err
		})
		return ch, err
	}
	for _, p := range []authn.Principal{v, lead, e.owner} {
		_, err := release(p)
		wantKind(t, err, kerr.NotFound, "release by "+p.Name)
	}
	ch, err := release(w)
	e.must(err)
	if ch.Ended.State != "RELEASED" || ch.Ended.EndedAt == nil || ch.Task.State != "RUNNABLE" {
		t.Fatalf("release: %+v", ch)
	}
	_, err = release(w)
	wantKind(t, err, kerr.NotFound, "release twice")
	if evs := e.events(a.ID); evs[len(evs)-1] != "LEASE_ENDED" {
		t.Fatalf("journal = %v", evs)
	}
}

// AC-H-018: one lease per repo root; the project's max_active_leases bounds its work in progress.
func TestAC_H_018_OneLeasePerRepoAndWIP(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	var u work.Task
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		if u, err = work.CreateTask(e.ctx, tx, e.owner, a.ID, newTask("U-1", task.RepoPath)); err != nil {
			return err
		}
		u, err = work.ReadyTask(e.ctx, tx, e.owner, u.ID)
		return err
	}))
	_, err := e.assign(e.owner, u.ID, v.ID, "")
	wantKind(t, err, kerr.Conflict, "second lease on one repo root")

	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetProjectLimits(e.ctx, tx, e.owner, a.ID, work.ProjectLimits{MaxActiveLeases: ptr(1),
			DefaultTaskMaxCost: cost(1), Budget: work.Limits{MaxCost: cost(10)}})
		return err
	}))
	elsewhere := e.runnable(a.ID, "U-2")
	_, err = e.assign(e.owner, elsewhere.ID, v.ID, "")
	wantKind(t, err, kerr.Conflict, "WIP limit of the project")
	if n := scalar[int](e, `SELECT count(*) FROM lease WHERE state = 'ACTIVE'`); n != 1 {
		t.Fatalf("%d active leases, want 1", n)
	}
	// Once the first lease ends, the limit has room again.
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := work.ReleaseLease(e.ctx, tx, w, task.ID)
		return err
	}))
	_, err = e.assign(e.owner, elsewhere.ID, v.ID, "")
	e.must(err)
}
