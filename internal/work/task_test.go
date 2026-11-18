package work_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AC-H-010: the owner creates, readies and assigns a task.
func TestAC_H_010_CreateReadyAssign(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	e.requirement(a.ID, "REQ-1", "AUTHORITATIVE", 1)
	dir, head := e.gitRepo()
	var task work.Task
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		root, err := work.RegisterRepo(e.ctx, tx, e.owner, a.ID, dir)
		if err != nil {
			return err
		}
		if task, err = work.CreateTask(e.ctx, tx, e.owner, a.ID, newTask("T-1", root, "REQ-1")); err != nil {
			return err
		}
		task, err = work.ReadyTask(e.ctx, tx, e.owner, task.ID)
		return err
	}))
	if task.State != "RUNNABLE" || task.DependencyVersions["REQ-1"] != 1 || len(task.DependencyVersions) != 1 {
		t.Fatalf("after ready: state %s, pinned %v", task.State, task.DependencyVersions)
	}
	ch, err := e.assign(e.owner, task.ID, w.ID, "")
	e.must(err)
	l := ch.Granted
	if ch.Task.State != "RUNNING" || l == nil || l.State != "ACTIVE" || l.Epoch != 1 || l.PrincipalID != w.ID ||
		l.Mode != "FULL" || l.ContextAcked || l.TaskRevision != 1 || l.ProjectID != a.ID || l.RepoPath != task.RepoPath {
		t.Fatalf("after assign: task %s, lease %+v", ch.Task.State, l)
	}
	if ttl := l.ExpiresAt.Sub(l.GrantedAt); ttl != work.LeaseTTL {
		t.Fatalf("lease lives %v, want %v", ttl, work.LeaseTTL)
	}
	if ch.Task.BaseCommit != head {
		t.Fatalf("base_commit %q, want HEAD %q", ch.Task.BaseCommit, head)
	}
	evs := e.events(a.ID)
	if !slices.Contains(evs, "TASK_READY") || !slices.Contains(evs, "LEASE_GRANTED") {
		t.Fatalf("journal = %v", evs)
	}
	// The holder sees its task; a worker without the lease does not (PM-H-12).
	v := e.agent("v", a.ID, "worker", "HOOK")
	e.must(e.as(w, func(tx pgx.Tx) error {
		got, err := work.GetTask(e.ctx, tx, w, task.ID)
		if err == nil && (got.Lease == nil || got.Budget.Task.Limits.MaxCost == nil) {
			t.Fatalf("holder view = %+v", got)
		}
		return err
	}))
	wantKind(t, e.as(v, func(tx pgx.Tx) error {
		_, err := work.GetTask(e.ctx, tx, v, task.ID)
		return err
	}), kerr.NotFound, "worker without the lease reads the task")
}

// AC-H-011: a task that misses a condition of T-H-01 stays PROPOSED and the 409 names the condition.
func TestAC_H_011_NotReady(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	repo := e.repo(a.ID)
	e.requirement(a.ID, "REQ-2", "CANDIDATE", 1)

	// A project without default_task_max_cost gives its tasks no cost limit.
	bare := e.project("bare", 10, 1)
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetProjectLimits(e.ctx, tx, e.owner, bare.ID, work.ProjectLimits{Budget: work.Limits{MaxCost: cost(10)}})
		return err
	}))
	bareRepo := e.repo(bare.ID)

	cases := []struct {
		name, projectID, names string
		mutate                 func(*work.NewTask)
	}{
		{"candidate requirement", a.ID, "REQ-2", func(n *work.NewTask) { n.RequirementKeys = []string{"REQ-2"} }},
		{"unknown requirement", a.ID, "REQ-404", func(n *work.NewTask) { n.RequirementKeys = []string{"REQ-404"} }},
		{"no required_checks", a.ID, "required_checks", func(n *work.NewTask) { n.RequiredChecks = nil }},
		{"no allowed_paths", a.ID, "allowed_paths", func(n *work.NewTask) { n.AllowedPaths = nil }},
		{"no repo_path", a.ID, "repo_path", func(n *work.NewTask) { n.RepoPath = "" }},
		{"no desired_outcome", a.ID, "desired_outcome", func(n *work.NewTask) { n.DesiredOutcome = " " }},
		{"no TASK max_cost", bare.ID, "TASK budget max_cost", func(n *work.NewTask) { n.RepoPath = bareRepo }},
	}
	for i, c := range cases {
		in := newTask("U-"+string(rune('a'+i)), repo)
		c.mutate(&in)
		var task work.Task
		e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
			task, err = work.CreateTask(e.ctx, tx, e.owner, c.projectID, in)
			return err
		}))
		for _, actor := range []authn.Principal{e.owner, lead} {
			if actor.ID == lead.ID && c.projectID != a.ID {
				continue // the lead is not a member of the other project
			}
			err := e.as(actor, func(tx pgx.Tx) error {
				_, err := work.ReadyTask(e.ctx, tx, actor, task.ID)
				return err
			})
			wantKind(t, err, kerr.Conflict, c.name)
			if !strings.Contains(err.Error(), c.names) {
				t.Fatalf("%s: 409 does not name the condition: %v", c.name, err)
			}
		}
		if got := e.view(task.ID).Task; got.State != "PROPOSED" || len(got.DependencyVersions) != 0 {
			t.Fatalf("%s: task became %s with %v", c.name, got.State, got.DependencyVersions)
		}
	}
	// A project without a PROJECT cost limit blocks every task of it.
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetProjectLimits(e.ctx, tx, e.owner, a.ID, work.ProjectLimits{DefaultTaskMaxCost: cost(1)})
		return err
	}))
	var task work.Task
	err := e.as(lead, func(tx pgx.Tx) (err error) {
		if task, err = work.CreateTask(e.ctx, tx, lead, a.ID, newTask("U-ok", repo)); err != nil {
			return err
		}
		_, err = work.ReadyTask(e.ctx, tx, lead, task.ID)
		return err
	})
	wantKind(t, err, kerr.Conflict, "no PROJECT max_cost")
	if !strings.Contains(err.Error(), "PROJECT budget max_cost") {
		t.Fatalf("409 does not name the PROJECT budget: %v", err)
	}
}

// AC-H-104: superseding a running task revokes its lease for good (T-H-08).
func TestAC_H_104_SupersedeRunningTask(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)

	wantKind(t, e.as(w, func(tx pgx.Tx) error {
		_, err := work.SupersedeTask(e.ctx, tx, w, task.ID)
		return err
	}), kerr.Forbidden, "worker supersedes")
	var ch work.Change
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		ch, err = work.SupersedeTask(e.ctx, tx, e.owner, task.ID)
		return err
	}))
	if ch.Task.State != "SUPERSEDED" || ch.Ended == nil || ch.Ended.State != "REVOKED" || ch.Ended.Reason != "superseded" || ch.Granted != nil {
		t.Fatalf("supersede: %+v", ch)
	}
	if n := scalar[int](e, `SELECT count(*) FROM lease WHERE task_id = $1 AND state = 'ACTIVE'`, task.ID); n != 0 {
		t.Fatalf("%d active leases after supersede", n)
	}
	// The repo has no open task left: this is the data the pre-tool hook allows on.
	if n := scalar[int](e, `SELECT count(*) FROM task WHERE repo_path = $1 AND state <> 'SUPERSEDED'`, task.RepoPath); n != 0 {
		t.Fatalf("%d open tasks left on the repo", n)
	}
	e.must(e.rt(func(tx pgx.Tx) error {
		held, err := work.ActiveLease(e.ctx, tx, w.ID)
		if held != nil {
			t.Fatalf("holder still has a lease: %+v", held.Lease)
		}
		return err
	}))
	// A closed task cannot be superseded again, assigned, or readied; a Lead may supersede in its project.
	wantKind(t, e.as(lead, func(tx pgx.Tx) error {
		_, err := work.SupersedeTask(e.ctx, tx, lead, task.ID)
		return err
	}), kerr.Conflict, "supersede twice")
	_, err := e.assign(e.owner, task.ID, w.ID, "")
	wantKind(t, err, kerr.Conflict, "assign a superseded task")
	other := e.runnable(a.ID, "T-2")
	e.must(e.as(lead, func(tx pgx.Tx) error {
		ch, err := work.SupersedeTask(e.ctx, tx, lead, other.ID)
		if err == nil && (ch.Task.State != "SUPERSEDED" || ch.Ended != nil) {
			t.Fatalf("lead supersede: %+v", ch)
		}
		return err
	}))
}
