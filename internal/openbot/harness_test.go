package openbot

import (
	"context"
	"os"
	"os/exec"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// harness is a fresh harness database with one project and one agent that
// holds the lease of one running task, set up through the work package.
type harness struct {
	t                          *testing.T
	ctx                        context.Context
	pool                       *pgxpool.Pool
	agentID, taskID, projectID string // OpenBot agent id of the holder; its task; its project
}

func (e *harness) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatalf("unexpected error: %v", err)
	}
}

func (e *harness) rt(fn func(tx pgx.Tx) error) error {
	return store.InScope(e.ctx, e.pool, store.Runtime, fn)
}

// row reads four integers with the runtime scope.
func (e *harness) row(sql string, args ...any) (out [4]int64) {
	e.t.Helper()
	e.must(e.rt(func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, sql, args...).Scan(&out[0], &out[1], &out[2], &out[3])
	}))
	return out
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	e := &harness{t: t, ctx: context.Background(), pool: testdb.New(t), agentID: "agent-a"}
	token, hash, err := authn.NewToken()
	e.must(err)
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ('owner', 'owner', $1)`, hash)
		return err
	}))
	owner, err := authn.Authenticate(e.ctx, e.pool, token)
	e.must(err)

	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	limit := int64(100_000_000)
	var holder work.Identity
	e.must(store.InScope(e.ctx, e.pool, owner.Scope(), func(tx pgx.Tx) error {
		p, err := work.CreateProject(e.ctx, tx, owner, "alpha", "Alpha")
		if err != nil {
			return err
		}
		e.projectID = p.ID
		if _, err := work.SetProjectLimits(e.ctx, tx, owner, p.ID, work.ProjectLimits{DefaultTaskMaxCost: &limit,
			Budget: work.Limits{MaxCost: &limit}}); err != nil {
			return err
		}
		root, err := work.RegisterRepo(e.ctx, tx, owner, p.ID, repo)
		if err != nil {
			return err
		}
		if holder, _, err = work.CreatePrincipal(e.ctx, tx, owner, work.NewPrincipal{Name: "a", OpenBotAgentID: e.agentID,
			Enforcement: "HOOK", ProjectID: p.ID, Role: "worker"}); err != nil {
			return err
		}
		task, err := work.CreateTask(e.ctx, tx, owner, p.ID, work.NewTask{Key: "T-1", Title: "t", DesiredOutcome: "done",
			RepoPath: root, AllowedPaths: []string{"src/**"}, RequiredChecks: []work.Check{{ID: "unit", Command: "true"}}})
		if err != nil {
			return err
		}
		e.taskID = task.ID
		_, err = work.ReadyTask(e.ctx, tx, owner, task.ID)
		return err
	}))
	e.must(e.rt(func(tx pgx.Tx) error {
		// A FULL assignment of a HOOK principal needs a registered hook install (AC-S2-011).
		if _, err := tx.Exec(e.ctx, `INSERT INTO hook_install (principal_id, settings_path, sha256) VALUES ($1, '/test/settings.json', repeat('0', 64))`, holder.ID); err != nil {
			return err
		}
		_, err := work.AssignTask(e.ctx, tx, verify.Store{Dir: t.TempDir()}, owner, e.taskID, holder.ID, "")
		return err
	}))
	return e
}
