package work_test

import (
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AC-H-053 (and the task side of AC-H-106): a changed requirement refreshes
// only the tasks that pin it.
func TestAC_H_053_DependencyChangeRefreshesOnlyPinningTasks(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 100, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")
	for _, k := range []string{"REQ-1", "REQ-7", "REQ-9"} {
		e.requirement(a.ID, k, "AUTHORITATIVE", 1)
	}
	pinning, other := e.runnable(a.ID, "T-1", "REQ-1"), e.runnable(a.ID, "U-1", "REQ-9")
	_, err := e.assign(e.owner, pinning.ID, w.ID, "")
	e.must(err)
	_, err = e.assign(e.owner, other.ID, v.ID, "")
	e.must(err)
	done := e.verified(e.view(pinning.ID).Task, "tree-c1")

	// The memory package superseded REQ-1 to v2 in the owner's transaction and reports the pinning tasks.
	var changes []work.Change
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		changes, err = work.OnDependencyChanged(e.ctx, tx, []string{done.ID, other.ID}, "REQ-1", "REQ-1", 2, "REQ-1 v2: nút nằm trong Part")
		return err
	}))
	if len(changes) != 1 {
		t.Fatalf("%d tasks changed, want only the pinning one", len(changes))
	}
	ch := changes[0]
	if ch.Task.ID != pinning.ID || ch.Task.DependencyVersions["REQ-1"] != 2 || ch.Task.Revision != 2 ||
		ch.Task.State != "RUNNING" || ch.Task.VerifiedCandidate != "" {
		t.Fatalf("pinning task: %+v", ch.Task)
	}
	if ch.Ended.Reason != "dependency_changed" || ch.Ended.State != "REVOKED" || ch.Granted.Epoch != 2 ||
		ch.Granted.PrincipalID != w.ID || ch.Granted.ContextAcked || ch.Granted.PendingNotice == "" || ch.Granted.TaskRevision != 2 {
		t.Fatalf("lease change: ended %+v, granted %+v", ch.Ended, ch.Granted)
	}
	if got := e.view(other.ID); got.Task.Revision != 1 || got.Lease.Epoch != 1 || got.Task.DependencyVersions["REQ-9"] != 1 {
		t.Fatalf("task that pins another key changed: %+v", got)
	}
	// Invalidate-and-merge (T-H-34): REQ-1 is replaced by REQ-7; the task pins REQ-7 from now on.
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		changes, err = work.OnDependencyChanged(e.ctx, tx, []string{pinning.ID}, "REQ-1", "REQ-7", 1, "REQ-1 gộp vào REQ-7")
		return err
	}))
	got := changes[0].Task
	if !slices.Equal(got.RequirementKeys, []string{"REQ-7"}) || len(got.DependencyVersions) != 1 ||
		got.DependencyVersions["REQ-7"] != 1 || got.Revision != 3 || changes[0].Granted.Epoch != 3 {
		t.Fatalf("after merge: keys %v, pinned %v, rev %d", got.RequirementKeys, got.DependencyVersions, got.Revision)
	}
}

// AC-H-082: feedback, events and budgets of project A never reach project B.
// AC-H-083 (data): the project and task of an agent come from its lease.
func TestAC_H_082_AC_H_083_ProjectsDoNotLeak(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 1, 5)
	b := e.project("beta", 1, 5)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", b.ID, "worker", "HOOK")
	z := e.agent("z", a.ID, "worker", "HOOK")
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.AddMembership(e.ctx, tx, e.owner, z.ID, b.ID, "lead")
		return err
	}))
	ta, tb := e.running(a.ID, "T-1", w), e.running(b.ID, "T-1", v)
	// AC-H-083: Z is a member of A and B; its lease, not a request body, says where it works.
	tz := e.running(a.ID, "T-2", z)
	e.must(e.as(z, func(tx pgx.Tx) error {
		me, err := work.Me(e.ctx, tx, z)
		if err == nil && (len(me.Memberships) != 2 || me.Lease == nil || me.Lease.ProjectID != a.ID || me.Lease.TaskID != tz.ID) {
			t.Fatalf("me = %+v", me)
		}
		return err
	}))
	e.must(e.rt(func(tx pgx.Tx) error {
		held, err := work.ActiveLease(e.ctx, tx, z.ID)
		if err == nil && (held.Lease.ProjectID != a.ID || held.Task.ProjectID != a.ID || held.Task.RepoPath != tz.RepoPath) {
			t.Fatalf("held = %+v", held)
		}
		return err
	}))

	before := e.view(tb.ID)
	seqB := scalar[int64](e, `SELECT COALESCE(max(seq), 0) FROM journal WHERE project_id = $1`, b.ID)

	// The owner steers A's task, and A's PROJECT budget runs out.
	e.feedback(ta.ID, "đổi hướng")
	res, err := e.usage(w, "rec-a", 1.00, time.Now())
	e.must(err)
	if res.ProjectState != "EXHAUSTED" || res.SystemState != "OK" {
		t.Fatalf("usage in A: %+v", res)
	}
	after := e.view(tb.ID)
	if after.Task.Revision != before.Task.Revision || after.Task.State != "RUNNING" || after.Lease.ID != before.Lease.ID ||
		after.Lease.Epoch != 1 || after.Lease.PendingNotice != "" || after.Budget.Effective != "OK" ||
		after.Budget.Task.Used.Cost != 0 || after.Budget.ProjectState != "OK" {
		t.Fatalf("project B changed: %+v", after)
	}
	if e.view(ta.ID).Budget.Effective != "EXHAUSTED" {
		t.Fatal("A's task is not blocked by its PROJECT budget")
	}
	if n := scalar[int64](e, `SELECT COALESCE(max(seq), 0) FROM journal WHERE project_id = $1`, b.ID); n != seqB {
		t.Fatal("events of A were written to the journal of B")
	}
	// A member of B reads B's events; A's member gets 404 on B, and no row of A appears for B's member.
	e.must(e.as(v, func(tx pgx.Tx) error {
		evs, err := work.ListEvents(e.ctx, tx, v, b.ID, 0, 0)
		for _, ev := range evs {
			if ev.ProjectID != b.ID || ev.TaskID == ta.ID {
				t.Fatalf("event of another project in B's journal: %+v", ev)
			}
		}
		var foreign int
		if err == nil { // a query with no project filter at all, under V's scope
			err = tx.QueryRow(e.ctx, `SELECT (SELECT count(*) FROM journal WHERE project_id <> $1)
				+ (SELECT count(*) FROM task WHERE project_id <> $1) + (SELECT count(*) FROM lease WHERE project_id <> $1)
				+ (SELECT count(*) FROM budget WHERE project_id <> $1)`, b.ID).Scan(&foreign)
		}
		if foreign != 0 {
			t.Fatalf("%d rows of other projects visible to a member of B", foreign)
		}
		return err
	}))
	wantKind(t, e.as(w, func(tx pgx.Tx) error {
		_, err := work.ListEvents(e.ctx, tx, w, b.ID, 0, 0)
		return err
	}), kerr.NotFound, "member of A lists events of B")
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		evs, err := work.ListSystemEvents(e.ctx, tx, e.owner, 0, 0)
		if len(evs) != 0 {
			t.Fatalf("project events in the system journal: %+v", evs)
		}
		return err
	}))

	// Usage of W outside every lease (other work in kongCowork) adds to no PROJECT and not to SYSTEM.
	system := e.used(`level = 'SYSTEM'`)
	res, err = e.usage(w, "rec-other", 9.00, time.Now().Add(-time.Hour))
	e.must(err)
	if !res.Recorded || res.TaskID != "" || e.used(`level = 'SYSTEM'`) != system ||
		e.used(`level = 'PROJECT' AND project_id = $1`, a.ID) != usd(1) || e.used(`level = 'PROJECT' AND project_id = $1`, b.ID) != 0 {
		t.Fatalf("usage outside a lease moved a budget: %+v", res)
	}

	// The sum of the PROJECT cost limits may not exceed the SYSTEM cost limit, from either side.
	setProject := func(max float64) error {
		return e.as(e.owner, func(tx pgx.Tx) error {
			_, err := work.SetProjectLimits(e.ctx, tx, e.owner, b.ID, work.ProjectLimits{Budget: work.Limits{MaxCost: cost(max)}})
			return err
		})
	}
	setSystem := func(max float64) error {
		return e.as(e.owner, func(tx pgx.Tx) error {
			_, err := work.SetSystemBudget(e.ctx, tx, e.owner, work.Limits{MaxCost: cost(max)})
			return err
		})
	}
	e.must(setSystem(3))
	wantKind(t, setProject(2.01), kerr.Invalid, "PROJECT limits above the SYSTEM limit")
	e.must(setProject(2))
	wantKind(t, setSystem(2.99), kerr.Invalid, "SYSTEM limit below the PROJECT limits")
	if got := scalar[int64](e, `SELECT max_cost_microusd FROM budget WHERE level = 'PROJECT' AND project_id = $1`, b.ID); got != usd(2) {
		t.Fatalf("refused limit was stored: %d", got)
	}
}
