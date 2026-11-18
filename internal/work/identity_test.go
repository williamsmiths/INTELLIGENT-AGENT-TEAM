package work_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// PM-H-01, PM-H-02, PM-H-03: projects, principals, memberships.
func TestPM_H_01_02_03_IdentityAdmin(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	b := e.project("beta", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")

	// The token is returned once and only its hash is stored (PM-H-02).
	if w.Kind != "agent" || w.Enforcement != "HOOK" || w.OpenBotAgentID != "ob-w" || w.Roles[a.ID] != "worker" {
		t.Fatalf("authenticated principal = %+v", w)
	}
	if n := scalar[int](e, `SELECT count(*) FROM principal WHERE token_hash = $1`, []byte("kch_")); n != 0 {
		t.Fatal("token stored in clear")
	}

	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.CreateProject(e.ctx, tx, e.owner, "alpha", "again")
		return err
	}), kerr.Conflict, "duplicate slug")
	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, _, err := work.CreatePrincipal(e.ctx, tx, e.owner, work.NewPrincipal{Name: "x", OpenBotAgentID: "ob-w",
			ProjectID: a.ID, Role: "worker"})
		return err
	}), kerr.Conflict, "duplicate openbot agent")
	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, _, err := work.CreatePrincipal(e.ctx, tx, e.owner, work.NewPrincipal{Name: "x", ProjectID: a.ID, Role: "boss"})
		return err
	}), kerr.Invalid, "bad role")

	// A member sees its project with the PROJECT budget; an outsider gets 404 (PM-H-03).
	e.must(e.as(w, func(tx pgx.Tx) error {
		p, err := work.GetProject(e.ctx, tx, w, a.ID)
		if err == nil && (p.Budget.Limits.MaxCost == nil || *p.Budget.Limits.MaxCost != usd(10)) {
			t.Fatalf("project budget = %+v", p.Budget)
		}
		return err
	}))
	wantKind(t, e.as(w, func(tx pgx.Tx) error {
		_, err := work.GetProject(e.ctx, tx, w, b.ID)
		return err
	}), kerr.NotFound, "outsider reads project")

	// Adding a membership makes the second project visible after the next authentication.
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		who, err := work.AddMembership(e.ctx, tx, e.owner, w.ID, b.ID, "lead")
		if err == nil && len(who.Memberships) != 2 {
			t.Fatalf("memberships = %+v", who.Memberships)
		}
		return err
	}))
	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.AddMembership(e.ctx, tx, e.owner, w.ID, b.ID, "lead")
		return err
	}), kerr.Conflict, "duplicate membership")
	e.must(e.rt(func(tx pgx.Tx) error {
		p, err := work.LookupPrincipal(e.ctx, tx, w.ID)
		if err == nil && (p.Roles[b.ID] != "lead" || p.OpenBotAgentID != "ob-w") {
			t.Fatalf("lookup = %+v", p)
		}
		return err
	}))
}

// REV-002 B10: every owner-only operation answers 403 to a Lead and to a Worker.
func TestOwnerOnlyOperationsAreForbiddenToAgents(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	ops := map[string]func(tx pgx.Tx, p authn.Principal) error{
		"CreateProject": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.CreateProject(e.ctx, tx, p, "gamma", "g")
			return err
		},
		"SetProjectLimits": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.SetProjectLimits(e.ctx, tx, p, a.ID, work.ProjectLimits{})
			return err
		},
		"RegisterRepo": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.RegisterRepo(e.ctx, tx, p, a.ID, t.TempDir())
			return err
		},
		"CreatePrincipal": func(tx pgx.Tx, p authn.Principal) error {
			_, _, err := work.CreatePrincipal(e.ctx, tx, p, work.NewPrincipal{Name: "x", ProjectID: a.ID, Role: "lead"})
			return err
		},
		"AddMembership": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.AddMembership(e.ctx, tx, p, w.ID, a.ID, "lead")
			return err
		},
		"SetSystemBudget": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.SetSystemBudget(e.ctx, tx, p, work.Limits{})
			return err
		},
		"SetTaskBudget": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.SetTaskBudget(e.ctx, tx, p, task.ID, work.Limits{MaxCost: cost(99)})
			return err
		},
		"RevokeLease": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.RevokeLease(e.ctx, tx, p, task.ID)
			return err
		},
		"Feedback": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.Feedback(e.ctx, tx, p, task.ID, "do it differently")
			return err
		},
		"ListSystemEvents": func(tx pgx.Tx, p authn.Principal) error {
			_, err := work.ListSystemEvents(e.ctx, tx, p, 0, 10)
			return err
		},
	}
	for name, op := range ops {
		for _, p := range []authn.Principal{lead, w} {
			wantKind(t, e.as(p, func(tx pgx.Tx) error { return op(tx, p) }), kerr.Forbidden, name+" by "+p.Name)
		}
	}
	if v := e.view(task.ID); v.Task.Revision != 1 || v.Lease == nil || *v.Budget.Task.Limits.MaxCost != usd(1) {
		t.Fatalf("a refused operation changed the task: %+v", v)
	}
}

// AC-H-108 (first part) and §0.4: a repo root is a realpath, belongs to one
// project and is never nested; a task may only use a root of its own project.
func TestAC_H_108_RepoBelongsToProject(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	b := e.project("beta", 10, 1)
	leadA := e.agent("lead-a", a.ID, "lead", "HOOK")
	rb := e.repo(b.ID)

	register := func(projectID, path string) error {
		return e.as(e.owner, func(tx pgx.Tx) error {
			_, err := work.RegisterRepo(e.ctx, tx, e.owner, projectID, path)
			return err
		})
	}
	inner := filepath.Join(rb, "pkg")
	e.must(os.Mkdir(inner, 0o755))
	link := filepath.Join(t.TempDir(), "link")
	e.must(os.Symlink(rb, link))
	for what, path := range map[string]string{
		"root nested in a registered root":    inner,
		"root that contains a registered one": filepath.Dir(rb),
		"same root for another project":       rb,
		"symlink to a registered root":        link,
		"missing directory":                   filepath.Join(rb, "..", "does-not-exist"),
		"relative path":                       "repo",
	} {
		wantKind(t, register(a.ID, path), kerr.Invalid, what)
	}
	if n := scalar[int](e, `SELECT count(*) FROM project_repo`); n != 1 {
		t.Fatalf("%d repo roots registered, want 1", n)
	}

	// The Lead of A cannot create a task on B's repo root.
	wantKind(t, e.as(leadA, func(tx pgx.Tx) error {
		_, err := work.CreateTask(e.ctx, tx, leadA, a.ID, newTask("T-1", rb))
		return err
	}), kerr.Invalid, "task of A on a repo root of B")
	ra := e.repo(a.ID)
	e.must(e.as(leadA, func(tx pgx.Tx) error {
		task, err := work.CreateTask(e.ctx, tx, leadA, a.ID, newTask("T-1", ra))
		if err == nil && (task.State != "PROPOSED" || task.RepoPath != ra) {
			t.Fatalf("task = %+v", task)
		}
		return err
	}))
}
