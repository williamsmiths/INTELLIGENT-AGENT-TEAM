package verify

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// gitRun runs git in dir with a fixed identity and without the user's global config.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return gitRunEnv(t, dir, nil, args...)
}

// gitRunEnv is gitRun with extra environment variables.
func gitRunEnv(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, repo, rel, content string) {
	t.Helper()
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo creates a real git repository with one commit and returns its root and that commit.
func newRepo(t *testing.T) (repo, base string) {
	t.Helper()
	repo = t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	write(t, repo, ".gitignore", "ignored.log\nbuild/\n")
	write(t, repo, "README.md", "readme\n")
	write(t, repo, "src/a.go", "package a\n")
	write(t, repo, "src/b.go", "package a // b\n")
	write(t, repo, "docs/old.md", "old\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "commit", "-q", "-m", "base")
	base = gitRun(t, repo, "rev-parse", "HEAD")
	pin(t, repo, base)
	return repo, base
}

// trees remembers, per test repository, the trusted base store, the pinned
// base and the trusted ignore list, the way harnessd keeps them outside the repo.
var trees sync.Map // repo -> Tree

// pin does what the harness does at registration and at the first assignment:
// the ignore list is the root .gitignore of HEAD, the base goes into a store
// of its own.
func pin(t *testing.T, repo, base string) Tree {
	t.Helper()
	ctx := context.Background()
	ignore, err := RootIgnoreAtHead(ctx, repo)
	if err != nil {
		t.Fatalf("RootIgnoreAtHead: %v", err)
	}
	tree := Tree{Repo: repo, Base: base, Store: Store{Dir: filepath.Join(t.TempDir(), "bases")}, Ignore: ignore}
	if err := tree.Store.PinBase(ctx, repo, base); err != nil {
		t.Fatalf("PinBase: %v", err)
	}
	trees.Store(repo, tree)
	return tree
}

// treeOf returns what pin stored for repo.
func treeOf(t *testing.T, repo string) Tree {
	t.Helper()
	v, ok := trees.Load(repo)
	if !ok {
		t.Fatalf("no pinned base for %s", repo)
	}
	return v.(Tree)
}

// changedFiles is ChangedFiles on the pinned tree of repo.
func changedFiles(t *testing.T, repo string) []string {
	t.Helper()
	got, err := ChangedFiles(context.Background(), treeOf(t, repo))
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	return got
}

// request is a valid Request for repo: the task may only touch src/**.
func request(t *testing.T, repo, base string, checks ...Check) Request {
	t.Helper()
	tree := treeOf(t, repo)
	return Request{
		ProjectID: "project-1", TaskID: "task-1", Revision: 1, Repo: repo, BaseCommit: base,
		BaseStore: tree.Store, IgnorePatterns: tree.Ignore,
		Allowed: []string{"src/**"}, Checks: checks, LogDir: t.TempDir(), RunnerID: "test-runner",
	}
}

func item(t *testing.T, res Result, checkID string) Item {
	t.Helper()
	for _, it := range res.Items {
		if it.CheckID == checkID {
			return it
		}
	}
	t.Fatalf("no item %q in %+v", checkID, res.Items)
	return Item{}
}

// seed creates an owner, two projects with one task each, and one agent.
func seed(t *testing.T, pool *pgxpool.Pool) (projectA, taskA, projectB, taskB, agent string) {
	t.Helper()
	ctx := context.Background()
	var owner string
	err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO principal (kind,name,token_hash) VALUES ('owner','owner','t0') RETURNING id`).Scan(&owner); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO principal (kind,name,token_hash) VALUES ('agent','w','t1') RETURNING id`).Scan(&agent)
	})
	if err != nil {
		t.Fatalf("seed principals: %v", err)
	}
	err = store.InScope(ctx, pool, store.Scope{Role: "owner", Principal: owner}, func(tx pgx.Tx) error {
		for _, p := range []struct {
			slug          string
			project, task *string
		}{{"alpha", &projectA, &taskA}, {"beta", &projectB, &taskB}} {
			if err := tx.QueryRow(ctx, `INSERT INTO project (slug,name) VALUES ($1,$1) RETURNING id`, p.slug).Scan(p.project); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `INSERT INTO task (project_id,key,title,state,created_by) VALUES ($1,'T-1','t','RUNNING',$2) RETURNING id`,
				*p.project, owner).Scan(p.task); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	return projectA, taskA, projectB, taskB, agent
}
