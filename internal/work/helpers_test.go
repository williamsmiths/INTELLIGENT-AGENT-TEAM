package work_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// env is one fresh database with an owner. Every call goes through the package
// API under the real scope of its actor; only fixtures that belong to other
// packages (knowledge, evidence) and clock tricks are seeded with SQL.
type env struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	owner authn.Principal
	bases verify.Store // trusted base store of this test (spec S2 D.1)
}

func usd(v float64) int64   { return int64(v*1e6 + 0.5) }
func ptr[T any](v T) *T     { return &v }
func cost(v float64) *int64 { return ptr(usd(v)) }

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), pool: testdb.New(t), bases: verify.Store{Dir: filepath.Join(t.TempDir(), "bases")}}
	token, hash, err := authn.NewToken()
	e.must(err)
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ('owner', 'owner', $1)`, hash)
		return err
	}))
	e.owner, err = authn.Authenticate(e.ctx, e.pool, token)
	e.must(err)
	return e
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
}

// as runs fn under the row-level-security scope of the actor.
func (e *env) as(p authn.Principal, fn func(tx pgx.Tx) error) error {
	return store.InScope(e.ctx, e.pool, p.Scope(), fn)
}

// rt runs fn under the runtime scope.
func (e *env) rt(fn func(tx pgx.Tx) error) error {
	return store.InScope(e.ctx, e.pool, store.Runtime, fn)
}

func wantKind(t *testing.T, err error, kind kerr.Kind, what string) {
	t.Helper()
	if !kerr.Is(err, kind) {
		t.Fatalf("%s: got error %v, want kind %d", what, err, kind)
	}
}

// project creates a project with a PROJECT cost limit and a default task cost limit.
func (e *env) project(slug string, projectMax, taskMax float64) work.Project {
	e.t.Helper()
	var p work.Project
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		if p, err = work.CreateProject(e.ctx, tx, e.owner, slug, slug); err != nil {
			return err
		}
		p, err = work.SetProjectLimits(e.ctx, tx, e.owner, p.ID, work.ProjectLimits{
			DefaultTaskMaxCost: cost(taskMax), Budget: work.Limits{MaxCost: cost(projectMax)}})
		return err
	}))
	return p
}

// agent creates an agent principal and authenticates it with its one-time token.
func (e *env) agent(name, projectID, role, enforcement string) authn.Principal {
	e.t.Helper()
	var token string
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		_, token, err = work.CreatePrincipal(e.ctx, tx, e.owner, work.NewPrincipal{Name: name,
			OpenBotAgentID: "ob-" + name, Enforcement: enforcement, ProjectID: projectID, Role: role})
		return err
	}))
	p, err := authn.Authenticate(e.ctx, e.pool, token)
	e.must(err)
	if enforcement == "HOOK" { // a FULL assignment needs a registered hook install (AC-S2-011)
		e.must(e.rt(func(tx pgx.Tx) error {
			_, err := tx.Exec(e.ctx, `INSERT INTO hook_install (principal_id, settings_path, sha256) VALUES ($1, '/test/settings.json', repeat('0', 64))`, p.ID)
			return err
		}))
	}
	return p
}

// gitRepo makes a real repository with one commit and returns its path and HEAD.
func (e *env) gitRepo() (string, string) {
	e.t.Helper()
	dir := e.t.TempDir()
	var head string
	for _, args := range [][]string{{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init"},
		{"rev-parse", "HEAD"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			e.t.Fatalf("git %v: %v: %s", args, err, out)
		}
		head = strings.TrimSpace(string(out))
	}
	return dir, head
}

// repo registers a fresh git repository as a root of the project.
func (e *env) repo(projectID string) string {
	e.t.Helper()
	dir, _ := e.gitRepo()
	var root string
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		root, err = work.RegisterRepo(e.ctx, tx, e.owner, projectID, dir)
		return err
	}))
	return root
}

// requirement seeds one knowledge row the way the memory package stores it.
func (e *env) requirement(projectID, key, trust string, version int) {
	e.t.Helper()
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO knowledge (level, project_id, key, version, kind, aspect, trust, basis,
				title, created_by, content_hash)
			VALUES ('PROJECT', $1, $2::text, $3::int, 'requirement', 'TO_BE', $4::text,
				CASE WHEN $4::text = 'CANDIDATE' THEN 'proposed' END, $2::text, $5, $2::text || ($3::int)::text)`,
			projectID, key, version, trust, e.owner.ID)
		return err
	}))
}

func newTask(key, repo string, reqs ...string) work.NewTask {
	return work.NewTask{Key: key, Title: key, DesiredOutcome: "done", Scope: "feature:x", RepoPath: repo,
		AllowedPaths: []string{"src/**"}, RequiredChecks: []work.Check{{ID: "unit", Command: "true"}},
		RequirementKeys: reqs}
}

// runnable creates a task in its own registered repo and makes it RUNNABLE.
func (e *env) runnable(projectID, key string, reqs ...string) work.Task {
	e.t.Helper()
	repo := e.repo(projectID)
	var t work.Task
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		if t, err = work.CreateTask(e.ctx, tx, e.owner, projectID, newTask(key, repo, reqs...)); err != nil {
			return err
		}
		t, err = work.ReadyTask(e.ctx, tx, e.owner, t.ID)
		return err
	}))
	return t
}

// assign grants the lease under the trusted scope AssignTask asks for.
func (e *env) assign(actor authn.Principal, taskID, assigneeID, mode string) (work.Change, error) {
	var ch work.Change
	err := e.rt(func(tx pgx.Tx) (err error) {
		ch, err = work.AssignTask(e.ctx, tx, e.bases, actor, taskID, assigneeID, mode)
		return err
	})
	return ch, err
}

// running creates a RUNNABLE task and assigns it to the agent.
func (e *env) running(projectID, key string, holder authn.Principal) work.Task {
	e.t.Helper()
	t := e.runnable(projectID, key)
	ch, err := e.assign(e.owner, t.ID, holder.ID, "")
	e.must(err)
	return ch.Task
}
