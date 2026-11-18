package work_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// git runs git in a test repository with a fixed identity.
func (e *env) git(dir string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.invalid"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) file(repo, rel, content string) {
	e.t.Helper()
	path := filepath.Join(repo, rel)
	e.must(os.MkdirAll(filepath.Dir(path), 0o755))
	e.must(os.WriteFile(path, []byte(content), 0o644))
}

// sourceRepo makes a repository whose HEAD holds a root .gitignore ("build/",
// "*.log"), registers it and returns a RUNNABLE task on it with its HEAD.
func (e *env) sourceRepo(projectID, key string) (repo string, head string, task work.Task) {
	e.t.Helper()
	dir, _ := e.gitRepo()
	e.file(dir, ".gitignore", "# build output\nbuild/\n\n*.log\n")
	e.file(dir, "src/a.go", "package a\n")
	e.file(dir, "docs/readme.md", "docs\n")
	e.file(dir, "docs/.gitignore", "drafts/\n") // nested: never part of the trusted list
	e.git(dir, "add", "-A")
	e.git(dir, "commit", "-q", "-m", "base")
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		if repo, err = work.RegisterRepo(e.ctx, tx, e.owner, projectID, dir); err != nil {
			return err
		}
		if task, err = work.CreateTask(e.ctx, tx, e.owner, projectID, newTask(key, repo)); err != nil {
			return err
		}
		task, err = work.ReadyTask(e.ctx, tx, e.owner, task.ID)
		return err
	}))
	return repo, e.git(repo, "rev-parse", "HEAD"), task
}

func (e *env) ignoreOf(root string) []string {
	e.t.Helper()
	var out []string
	e.must(e.rt(func(tx pgx.Tx) (err error) {
		out, err = work.RepoIgnore(e.ctx, tx, root)
		return err
	}))
	return out
}

// AC-S2-052: a dirty repo root cannot become a base, whatever the repository's
// own state says about it; files on the trusted ignore list do not count.
func TestAssign_AC_S2_052(t *testing.T) {
	e := newEnv(t)
	p := e.project("alpha", 10, 2)
	worker := e.agent("w", p.ID, "worker", "HOOK")
	refused := func(t *testing.T, task work.Task, wantInMessage string) {
		t.Helper()
		_, err := e.assign(e.owner, task.ID, worker.ID, "")
		wantKind(t, err, kerr.Conflict, "assign on a dirty repo root")
		if !strings.Contains(err.Error(), wantInMessage) {
			t.Fatalf("message %q does not name %q", err, wantInMessage)
		}
		var state, base string
		e.must(e.rt(func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, `SELECT state, base_commit FROM task WHERE id = $1`, task.ID).Scan(&state, &base)
		}))
		if state != "RUNNABLE" || base != "" {
			t.Fatalf("task is %s with base %q after a refused assign", state, base)
		}
	}

	t.Run("AC-S2-052 untracked file outside the trusted ignore list", func(t *testing.T) {
		repo, _, task := e.sourceRepo(p.ID, "T-1")
		e.file(repo, "docs/leftover.md", "left by an earlier task\n")
		refused(t, task, "docs/leftover.md")
	})
	t.Run("AC-S2-052 untracked file hidden by .git/info/exclude and showUntrackedFiles=no", func(t *testing.T) {
		repo, _, task := e.sourceRepo(p.ID, "T-2")
		e.file(repo, "docs/hidden.md", "hidden\n")
		e.file(repo, ".git/info/exclude", "docs/hidden.md\n")
		e.git(repo, "config", "status.showUntrackedFiles", "no")
		if out := e.git(repo, "status", "--porcelain"); out != "" {
			t.Fatalf("fixture: git status shows %q", out)
		}
		refused(t, task, "docs/hidden.md")
	})
	t.Run("AC-S2-052 tracked edit hidden by skip-worktree and assume-unchanged", func(t *testing.T) {
		repo, _, task := e.sourceRepo(p.ID, "T-3")
		e.file(repo, "src/a.go", "package a // edited before the first assignment\n")
		e.git(repo, "update-index", "--skip-worktree", "src/a.go")
		e.file(repo, "docs/readme.md", "edited\n")
		e.git(repo, "update-index", "--assume-unchanged", "docs/readme.md")
		if out := e.git(repo, "status", "--porcelain"); out != "" {
			t.Fatalf("fixture: git status shows %q", out)
		}
		refused(t, task, "2 uncommitted")
	})
	t.Run("AC-S2-052 file hidden by a self-ignoring nested .gitignore", func(t *testing.T) {
		repo, _, task := e.sourceRepo(p.ID, "T-4")
		e.file(repo, "tools/.gitignore", "*\n")
		e.file(repo, "tools/x.py", "print('hidden')\n")
		e.file(repo, "docs/drafts/d.md", "ignored only by the nested docs/.gitignore\n")
		if out := e.git(repo, "status", "--porcelain"); out != "" {
			t.Fatalf("fixture: git status shows %q", out)
		}
		refused(t, task, "3 uncommitted")
	})
	t.Run("AC-S2-052 only files on the trusted ignore list: assigned, base pinned, no repo program runs", func(t *testing.T) {
		repo, head, task := e.sourceRepo(p.ID, "T-5")
		if got := e.ignoreOf(repo); !reflect.DeepEqual(got, []string{"build/", "*.log"}) {
			t.Fatalf("default trusted ignore list = %v", got)
		}
		e.file(repo, "build/out.bin", "build output\n")
		e.file(repo, "src/debug.log", "log\n")
		plain, err := os.MkdirTemp("", "kchfsm")
		e.must(err)
		t.Cleanup(func() { _ = os.RemoveAll(plain) })
		marker := filepath.Join(plain, "ran")
		e.must(os.WriteFile(filepath.Join(plain, "fsmonitor.sh"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755))
		e.git(repo, "config", "core.fsmonitor", filepath.Join(plain, "fsmonitor.sh"))
		e.git(repo, "status", "--porcelain")
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("fixture: git status does not run the fsmonitor script: %v", err)
		}
		e.must(os.Remove(marker))

		ch, err := e.assign(e.owner, task.ID, worker.ID, "")
		e.must(err)
		if ch.Task.State != "RUNNING" || ch.Task.BaseCommit != head {
			t.Fatalf("task %s base %q, want RUNNING on %s", ch.Task.State, ch.Task.BaseCommit, head)
		}
		if ok, err := e.bases.Has(e.ctx, repo, head); err != nil || !ok {
			t.Fatalf("base not in the trusted store: %v %v", ok, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("AssignTask executed the repository's core.fsmonitor script (AC-S2-050 e)")
		}
	})
	t.Run("AC-S2-052 the owner extends the trusted list; the working tree's .gitignore is not trusted", func(t *testing.T) {
		repo, _, task := e.sourceRepo(p.ID, "T-6")
		worker = e.agent("w6", p.ID, "worker", "HOOK") // the first worker holds the lease of T-5
		e.file(repo, "node_modules/x/index.js", "dependency\n")
		e.file(repo, ".gitignore", "build/\n*.log\nnode_modules/\n") // edited in the tree, not committed, not registered
		refused(t, task, "uncommitted")
		e.git(repo, "checkout", "-q", "--", ".gitignore")
		refused(t, task, "node_modules/x/index.js")

		lead := e.agent("lead6", p.ID, "lead", "HOOK")
		err := e.as(lead, func(tx pgx.Tx) error {
			_, _, err := work.SetRepoIgnore(e.ctx, tx, lead, p.ID, repo, []string{"node_modules/"})
			return err
		})
		wantKind(t, err, kerr.Forbidden, "a lead sets the trusted ignore list")
		err = e.as(e.owner, func(tx pgx.Tx) error {
			_, _, err := work.SetRepoIgnore(e.ctx, tx, e.owner, p.ID, repo, []string{"bad\npattern"})
			return err
		})
		wantKind(t, err, kerr.Invalid, "a pattern with a line break")
		e.must(e.as(e.owner, func(tx pgx.Tx) error {
			_, got, err := work.SetRepoIgnore(e.ctx, tx, e.owner, p.ID, repo, []string{"# deps", "build/", "*.log", "", "node_modules/"})
			if err == nil && !reflect.DeepEqual(got, []string{"build/", "*.log", "node_modules/"}) {
				t.Fatalf("stored list = %v", got)
			}
			return err
		}))
		if _, err := e.assign(e.owner, task.ID, worker.ID, ""); err != nil {
			t.Fatalf("assign after the owner extended the list: %v", err)
		}
	})
}
