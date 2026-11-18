package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

func hashOf(t *testing.T, repo string) string {
	t.Helper()
	h, err := CandidateHash(context.Background(), treeOf(t, repo))
	if err != nil {
		t.Fatalf("CandidateHash: %v", err)
	}
	return h
}

func looseObjects(t *testing.T, repo string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(repo, ".git", "objects"), func(_ string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// Spec §2.1: candidate = tree hash of tracked files plus untracked files that are not ignored.
func TestCandidateHash(t *testing.T) {
	repo, _ := newRepo(t)
	clean := hashOf(t, repo)

	t.Run("AC-H-046 clean tree equals the committed tree and is stable", func(t *testing.T) {
		if head := gitRun(t, repo, "rev-parse", "HEAD^{tree}"); clean != head {
			t.Fatalf("candidate %s, HEAD tree %s", clean, head)
		}
		if again := hashOf(t, repo); again != clean {
			t.Fatalf("hash not stable: %s then %s", clean, again)
		}
	})
	t.Run("AC-H-046 hashing writes nothing into the repository", func(t *testing.T) {
		write(t, repo, "src/untracked.go", "package a // new\n")
		status, objects := gitRun(t, repo, "status", "--porcelain"), looseObjects(t, repo)
		index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
		if err != nil {
			t.Fatal(err)
		}
		hashOf(t, repo)
		after, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
		if err != nil {
			t.Fatal(err)
		}
		if string(index) != string(after) {
			t.Fatal("the real index changed")
		}
		if got := gitRun(t, repo, "status", "--porcelain"); got != status {
			t.Fatalf("status changed: %q -> %q", status, got)
		}
		if got := looseObjects(t, repo); got != objects {
			t.Fatalf("object store grew from %d to %d files", objects, got)
		}
		if err := os.Remove(filepath.Join(repo, "src/untracked.go")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AC-H-046 one byte in a tracked file changes the hash", func(t *testing.T) {
		write(t, repo, "src/a.go", "package b\n")
		if hashOf(t, repo) == clean {
			t.Fatal("hash did not change")
		}
		write(t, repo, "src/a.go", "package a\n")
		if hashOf(t, repo) != clean {
			t.Fatal("hash did not return after restoring the byte")
		}
	})
	t.Run("AC-H-046 untracked file is counted, staged or not", func(t *testing.T) {
		write(t, repo, "notes.txt", "x")
		unstaged := hashOf(t, repo)
		if unstaged == clean {
			t.Fatal("untracked file not counted")
		}
		write(t, repo, "notes.txt", "y")
		changed := hashOf(t, repo)
		if changed == unstaged {
			t.Fatal("one byte in an untracked file did not change the hash")
		}
		gitRun(t, repo, "add", "notes.txt")
		if hashOf(t, repo) != changed {
			t.Fatal("staging changed the candidate")
		}
		gitRun(t, repo, "rm", "-q", "-f", "--cached", "notes.txt")
		if err := os.Remove(filepath.Join(repo, "notes.txt")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AC-H-046 ignored file is not counted", func(t *testing.T) {
		write(t, repo, "ignored.log", "noise")
		write(t, repo, "build/out.bin", "noise")
		if hashOf(t, repo) != clean {
			t.Fatal("ignored files changed the hash")
		}
	})
	t.Run("not a repository root is refused", func(t *testing.T) {
		sub, plain := treeOf(t, repo), treeOf(t, repo)
		sub.Repo, plain.Repo = filepath.Join(repo, "src"), t.TempDir()
		if _, err := CandidateHash(context.Background(), sub); !errors.Is(err, ErrNotPinned) {
			t.Fatalf("a subdirectory was accepted as a repo root: %v", err)
		}
		if _, err := CandidateHash(context.Background(), plain); !errors.Is(err, ErrNotPinned) {
			t.Fatalf("a plain directory was accepted: %v", err)
		}
	})
}

// AC-H-046 / L4: the diff is against base_commit, committed or not.
func TestChangedFiles_AC_H_046(t *testing.T) {
	repo, base := newRepo(t)
	ctx := context.Background()
	if got := changedFiles(t, repo); len(got) != 0 {
		t.Fatalf("clean tree: %v", got)
	}
	write(t, repo, "README.md", "committed after base\n")
	gitRun(t, repo, "commit", "-q", "-am", "after base")
	write(t, repo, "src/a.go", "package a // edited, not committed\n")
	write(t, repo, "src/new.go", "package a // untracked\n")
	write(t, repo, "ignored.log", "ignored")
	if err := os.Remove(filepath.Join(repo, "docs/old.md")); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "mv", "src/b.go", "src/c.go")

	got := changedFiles(t, repo)
	want := []string{"README.md", "docs/old.md", "src/a.go", "src/b.go", "src/c.go", "src/new.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %v\nwant      %v", got, want)
	}
	bad := treeOf(t, repo)
	bad.Base = "--output=/tmp/x"
	if _, err := ChangedFiles(ctx, bad); !kerr.Is(err, kerr.Invalid) {
		t.Fatalf("a base that is not a commit hash was accepted: %v", err)
	}
	bad.Base = base[:12]
	if _, err := ChangedFiles(ctx, bad); !kerr.Is(err, kerr.Invalid) {
		t.Fatalf("an abbreviated base was accepted: %v", err)
	}
	bad.Base = "0123456789abcdef0123456789abcdef01234567"
	if _, err := ChangedFiles(ctx, bad); !errors.Is(err, ErrNotPinned) {
		t.Fatalf("a base that was never pinned was accepted: %v", err)
	}
}

func TestScopeCheck_AC_H_041(t *testing.T) {
	changed := []string{"src/a.go", "src/deep/x/y.go", "README.md", "src/secret/key.go", "tests/acceptance_test.go",
		".claude/settings.json", ".git/config", "README.md"}
	got := ScopeCheck(changed, []string{"src/**", "tests/**"}, []string{"src/secret/**", "tests/acceptance_test.go"})
	want := []string{".claude/settings.json", ".git/config", "README.md", "src/secret/key.go", "tests/acceptance_test.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("violations = %v\nwant         %v", got, want)
	}
	if v := ScopeCheck([]string{"src/a.go"}, []string{"src/**"}, nil); len(v) != 0 {
		t.Fatalf("allowed path reported: %v", v)
	}
	if v := ScopeCheck([]string{"src/a.go"}, nil, nil); len(v) != 1 {
		t.Fatalf("no allowed pattern must mean nothing is allowed: %v", v)
	}

	// L6: the always-protected paths are the ones guard refuses at write time.
	repo := t.TempDir()
	rules := guard.Rules{Repo: repo, Allowed: []string{"**"}}
	for _, rel := range []string{".git", ".git/config", ".git/hooks/pre-commit", ".claude", ".claude/settings.json", "src/a.go", "a.claude", ".gitignore",
		"src/.gitignore", ".gitattributes", "src/deep/.gitattributes", ".gitmodules", "src/.gitmodules", "src/x.gitignore"} {
		guarded := rules.CheckWrite(filepath.Join(repo, rel), repo).Rule == "protected"
		violated := len(ScopeCheck([]string{rel}, []string{"**"}, nil)) == 1
		if guarded != violated {
			t.Errorf("%s: guard protected=%v, scope violation=%v", rel, guarded, violated)
		}
	}
}
