package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

// lock makes a path unreadable for the test and restores it so the temp dir can be removed.
func lock(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads every file; the unreadable-tree cases need an ordinary user")
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o755) })
}

// AC-S2-051: a working tree that cannot be read completely is a failure, never
// "nothing changed"; a run on it records ERROR evidence and is not AllPass.
func TestSnapshot_AC_S2_051(t *testing.T) {
	ctx := context.Background()
	failed := func(t *testing.T, repo, base string) {
		t.Helper()
		if got, err := ChangedFiles(ctx, treeOf(t, repo)); err == nil {
			t.Fatalf("ChangedFiles returned %v without an error", got)
		}
		if h, err := CandidateHash(ctx, treeOf(t, repo)); err == nil {
			t.Fatalf("CandidateHash returned %s without an error", h)
		}
		res := run(t, request(t, repo, base, Check{ID: "unit", Command: "true"}))
		if res.AllPass || res.Candidate != "" || len(res.Items) != 2 {
			t.Fatalf("allPass=%v candidate=%q items=%+v", res.AllPass, res.Candidate, res.Items)
		}
		for _, it := range res.Items {
			if it.Result != ResultError || it.Detail["reason"] != ReasonSnapshotFailed || it.Detail["error"] == "" {
				t.Fatalf("%s: %s %v", it.CheckID, it.Result, it.Detail)
			}
		}
	}
	t.Run("AC-S2-051 unreadable subdirectory", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "docs/locked/hidden.md", "outside the scope\n")
		lock(t, filepath.Join(repo, "docs/locked"))
		failed(t, repo, base)
	})
	t.Run("AC-S2-051 unreadable tracked file", func(t *testing.T) {
		repo, base := newRepo(t)
		lock(t, filepath.Join(repo, "docs/old.md"))
		failed(t, repo, base)
	})
	t.Run("AC-S2-051 tree becomes unreadable while the checks run", func(t *testing.T) {
		repo, base := newRepo(t)
		t.Cleanup(func() { _ = os.Chmod(filepath.Join(repo, "src/locked"), 0o755) })
		res := run(t, request(t, repo, base, Check{ID: "unit", Command: "mkdir src/locked && touch src/locked/f && chmod 000 src/locked"}))
		if res.AllPass || res.Candidate == "" || len(res.Items) != 2 {
			t.Fatalf("allPass=%v candidate=%q items=%+v", res.AllPass, res.Candidate, res.Items)
		}
		for _, it := range res.Items {
			if it.Result != ResultError || it.Detail["reason"] != ReasonSnapshotFailed {
				t.Fatalf("%s: %s %v", it.CheckID, it.Result, it.Detail)
			}
		}
		if got := item(t, res, "unit").Detail["observed_result"]; got != ResultPass {
			t.Fatalf("observed_result = %v", got)
		}
	})
	t.Run("AC-S2-051 core.bare=true no longer reaches the snapshot", func(t *testing.T) {
		repo, base := newRepo(t)
		write(t, repo, "docs/x.md", "outside the scope\n")
		gitRun(t, repo, "config", "core.bare", "true")
		if got := changedFiles(t, repo); !reflect.DeepEqual(got, []string{"docs/x.md"}) {
			t.Fatalf("changed = %v", got)
		}
		res := run(t, request(t, repo, base, Check{ID: "unit", Command: "true"}))
		if scope := item(t, res, ScopeCheckID); scope.Result != ResultFail || res.AllPass {
			t.Fatalf("scope: %s %v", scope.Result, scope.Detail)
		}
	})
	t.Run("AC-S2-051 base that was never pinned", func(t *testing.T) {
		repo, base := newRepo(t)
		tree := treeOf(t, repo)
		tree.Store = Store{Dir: filepath.Join(t.TempDir(), "empty")}
		if _, err := ChangedFiles(ctx, tree); !errors.Is(err, ErrNotPinned) {
			t.Fatalf("err = %v, want ErrNotPinned", err)
		}
		req := request(t, repo, base, Check{ID: "unit", Command: "true"})
		req.BaseStore = tree.Store
		if res := run(t, req); res.AllPass || item(t, res, ScopeCheckID).Detail["reason"] != ReasonSnapshotFailed {
			t.Fatalf("%+v", res)
		}
	})
}

// Spec S2 D.1: a directory with its own .git hides nothing — with or without a
// commit inside, its files are read one by one as plain files.
func TestSnapshot_S2_D1_NestedRepositoryIsPlainFiles(t *testing.T) {
	repo, _ := newRepo(t)
	clean := hashOf(t, repo)
	write(t, repo, "docs/sub/evil.md", "outside the scope\n")
	gitRun(t, repo, "init", "-q", filepath.Join(repo, "docs/sub"))
	if got := changedFiles(t, repo); !reflect.DeepEqual(got, []string{"docs/sub/evil.md"}) {
		t.Fatalf("nested repository without a commit: changed = %v", got)
	}
	gitRun(t, filepath.Join(repo, "docs/sub"), "add", "-A")
	gitRun(t, filepath.Join(repo, "docs/sub"), "commit", "-q", "-m", "inside")
	committed := hashOf(t, repo)
	write(t, repo, "docs/sub/deep/more.md", "uncommitted inside the nested repository\n")
	write(t, repo, "docs/sub/ignored.log", "the trusted ignore list does not apply inside a nested repository\n")
	want := []string{"docs/sub/deep/more.md", "docs/sub/evil.md", "docs/sub/ignored.log"}
	if got := changedFiles(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("nested repository with a commit: changed = %v, want %v", got, want)
	}
	if after := hashOf(t, repo); after == clean || after == committed {
		t.Fatal("a file inside the nested repository did not change the candidate")
	}
	// A .git placed inside a tracked directory hides nothing either.
	gitRun(t, repo, "init", "-q", filepath.Join(repo, "src"))
	write(t, repo, "src/inner.go", "package a // next to a planted .git\n")
	if got := changedFiles(t, repo); !slices.Contains(got, "src/inner.go") {
		t.Fatalf("file next to a planted .git is missing: %v", got)
	}
}

// Spec S2 D.1: a submodule directory with files in it is refused, not passed as "one link".
func TestSnapshot_S2_D1_SubmoduleWithFilesIsRefused(t *testing.T) {
	inner := t.TempDir()
	gitRun(t, inner, "init", "-q", "-b", "main")
	write(t, inner, "lib.go", "package lib\n")
	gitRun(t, inner, "add", "-A")
	gitRun(t, inner, "commit", "-q", "-m", "lib")
	repo := t.TempDir()
	gitRun(t, repo, "init", "-q", "-b", "main")
	write(t, repo, "src/a.go", "package a\n")
	gitRun(t, repo, "add", "-A")
	gitRun(t, repo, "update-index", "--add", "--cacheinfo", "160000,"+gitRun(t, inner, "rev-parse", "HEAD")+",vendor/lib")
	gitRun(t, repo, "commit", "-q", "-m", "base with a submodule link")
	tree := pin(t, repo, gitRun(t, repo, "rev-parse", "HEAD"))

	if err := os.MkdirAll(filepath.Join(repo, "vendor/lib"), 0o755); err != nil { // what a checkout leaves for a submodule
		t.Fatal(err)
	}
	if got := changedFiles(t, repo); len(got) != 0 {
		t.Fatalf("an empty submodule directory is not a change: %v", got)
	}
	write(t, repo, "vendor/lib/planted.go", "package lib // hidden behind the link\n")
	if _, err := ChangedFiles(context.Background(), tree); !errors.Is(err, ErrSubmodule) {
		t.Fatalf("err = %v, want ErrSubmodule", err)
	}
}

// AC-H-046: a file replaced by a directory, a directory replaced by a file and
// a file replaced by a symbolic link are all reported under both names.
func TestSnapshot_AC_H_046_FileDirectoryAndLinkSwaps(t *testing.T) {
	repo, _ := newRepo(t)
	for _, p := range []string{"README.md", "docs"} {
		if err := os.RemoveAll(filepath.Join(repo, p)); err != nil {
			t.Fatal(err)
		}
	}
	write(t, repo, "README.md/inside.txt", "a directory now\n")
	write(t, repo, "docs", "a file now\n")
	if err := os.Remove(filepath.Join(repo, "src/b.go")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.go", filepath.Join(repo, "src/b.go")); err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "README.md/inside.txt", "docs", "docs/old.md", "src/b.go"}
	if got := changedFiles(t, repo); !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %v\nwant      %v", got, want)
	}
}
