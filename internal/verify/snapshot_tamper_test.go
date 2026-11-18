package verify

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// tamperCase checks what AC-S2-050 asks of every sub-case: ChangedFiles lists
// exactly the changed files, the candidate differs from the clean tree, and
// the built-in scope check of a real run FAILs with those files.
func tamperCase(t *testing.T, repo, base, clean string, want []string) {
	t.Helper()
	changed := changedFiles(t, repo)
	if got := ScopeCheck(changed, []string{"src/**"}, nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("changed = %v\nout of scope = %v\nwant           %v", changed, got, want)
	}
	if hashOf(t, repo) == clean {
		t.Fatal("the candidate hash equals the clean tree")
	}
	res := run(t, request(t, repo, base, Check{ID: "unit", Command: "true"}))
	scope := item(t, res, ScopeCheckID)
	if scope.Result != ResultFail || res.AllPass || !reflect.DeepEqual(scope.Detail["violations"], want) {
		t.Fatalf("scope: %s allPass=%v detail=%v", scope.Result, res.AllPass, scope.Detail)
	}
}

func porcelain(t *testing.T, repo string) string { return gitRun(t, repo, "status", "--porcelain") }

// AC-S2-050: nothing an agent with a shell can write — in .git, in the tree or
// in the OS user's git configuration — hides a change from the snapshot.
func TestSnapshot_AC_S2_050(t *testing.T) {
	t.Run("AC-S2-050 (a) base tree object overwritten in .git/objects", func(t *testing.T) {
		repo, base := newRepo(t)
		clean := hashOf(t, repo)
		write(t, repo, "docs/evil.md", "outside the scope\n")
		// The agent makes the base commit's tree object say what the dirty tree says.
		index, err := os.ReadFile(filepath.Join(repo, ".git", "index"))
		if err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "add", "-A")
		dirty, baseTree := gitRun(t, repo, "write-tree"), gitRun(t, repo, "rev-parse", base+"^{tree}")
		if err := os.WriteFile(filepath.Join(repo, ".git", "index"), index, 0o644); err != nil {
			t.Fatal(err)
		}
		loose := func(id string) string { return filepath.Join(repo, ".git", "objects", id[:2], id[2:]) }
		forged, err := os.ReadFile(loose(dirty))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(loose(baseTree), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(loose(baseTree), forged, 0o644); err != nil {
			t.Fatal(err)
		}
		// The fixture is a real attack: a diff read from the repository's own objects is empty.
		if out := gitRun(t, repo, "diff-tree", "-r", "--name-only", base, dirty); out != "" {
			t.Fatalf("fixture: the repository's own objects still show %q", out)
		}
		tamperCase(t, repo, base, clean, []string{"docs/evil.md"})

		// A base pinned only now, from the forged objects, is refused: the store never holds a tree it did not hash.
		late := Store{Dir: filepath.Join(t.TempDir(), "late")}
		if err := late.PinBase(context.Background(), repo, base); err == nil {
			t.Fatal("PinBase accepted a commit whose tree object was replaced")
		}
		if ok, _ := late.Has(context.Background(), repo, base); ok {
			t.Fatal("the forged commit is in the store")
		}
	})
	t.Run("AC-S2-050 (b) self-ignoring .gitignore in a subdirectory", func(t *testing.T) {
		repo, base := newRepo(t)
		clean := hashOf(t, repo)
		write(t, repo, "docs/.gitignore", "*\n")
		write(t, repo, "docs/x.py", "print('outside the scope')\n")
		if out := porcelain(t, repo); out != "" {
			t.Fatalf("fixture: git status still shows %q", out)
		}
		tamperCase(t, repo, base, clean, []string{"docs/.gitignore", "docs/x.py"})

		// The root .gitignore of the working tree is not trusted either: only the registered list is.
		write(t, repo, ".gitignore", "ignored.log\nbuild/\ndocs/\n")
		if got, want := changedFiles(t, repo), []string{".gitignore", "docs/.gitignore", "docs/x.py"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("changed = %v, want %v", got, want)
		}
	})
	t.Run("AC-S2-050 (c) untracked .gitattributes: filter, eol, ident, working-tree-encoding", func(t *testing.T) {
		repo := t.TempDir()
		gitRun(t, repo, "init", "-q", "-b", "main")
		write(t, repo, "src/a.go", "package a\n")
		write(t, repo, "docs/old.md", "old\n")
		write(t, repo, "docs/eol.txt", "one\ntwo\n")
		write(t, repo, "docs/id.txt", "$Id$\n")
		write(t, repo, "docs/enc.csv", "hello\n")
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "commit", "-q", "-m", "base")
		base := gitRun(t, repo, "rev-parse", "HEAD")
		pin(t, repo, base)
		clean := hashOf(t, repo)

		write(t, repo, ".gitattributes", "*.md filter=blank\n*.txt text eol=lf ident\n*.csv working-tree-encoding=UTF-16\n")
		gitRun(t, repo, "config", "filter.blank.clean", "git show HEAD:%f")
		write(t, repo, "docs/old.md", "edited outside the scope\n")
		write(t, repo, "docs/eol.txt", "one\r\ntwo\r\n")
		write(t, repo, "docs/id.txt", "$Id: payload hidden in the keyword $\n")
		write(t, repo, "docs/enc.csv", "\xff\xfeh\x00e\x00l\x00l\x00o\x00\n\x00") // "hello\n" as UTF-16LE with a BOM
		// The fixture is a real attack: `git add -A` into a scratch index stores the base content again.
		scratch := "GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")
		gitRunEnv(t, repo, []string{scratch}, "read-tree", "HEAD")
		gitRunEnv(t, repo, []string{scratch}, "add", "-A")
		if out := gitRunEnv(t, repo, []string{scratch}, "diff-index", "--cached", "--name-only", "HEAD"); out != ".gitattributes" {
			t.Fatalf("fixture: git add -A still sees %q", out)
		}
		tamperCase(t, repo, base, clean, []string{".gitattributes", "docs/enc.csv", "docs/eol.txt", "docs/id.txt", "docs/old.md"})
	})
	t.Run("AC-S2-050 (d) ignore and attributes files of the OS user", func(t *testing.T) {
		repo, base := newRepo(t)
		clean := hashOf(t, repo)
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
		write(t, home, ".config/git/ignore", "docs/\n")
		write(t, home, ".config/git/attributes", "*.go filter=blank\n")
		write(t, home, ".gitconfig", "[filter \"blank\"]\n\tclean = git show HEAD:%f\n[core]\n\texcludesFile = "+filepath.Join(home, ".config/git/ignore")+"\n")
		write(t, repo, "docs/x.py", "print('outside the scope')\n")
		write(t, repo, "README.md", "edited outside the scope\n")
		write(t, repo, "src/a.go", "package a // edit hidden by the user's filter\n")
		// The fixture is a real attack: git under this user's configuration reports nothing in docs/ and no edit of src/a.go.
		user := []string{"GIT_CONFIG_GLOBAL=" + filepath.Join(home, ".gitconfig")}
		if out := gitRunEnv(t, repo, user, "status", "--porcelain"); strings.Contains(out, "docs/") {
			t.Fatalf("fixture: git status still shows %q", out)
		}
		tamperCase(t, repo, base, clean, []string{"README.md", "docs/x.py"})
		if got := changedFiles(t, repo); !reflect.DeepEqual(got, []string{"README.md", "docs/x.py", "src/a.go"}) {
			t.Fatalf("changed = %v: the edit behind the user's filter is missing", got)
		}
	})
	t.Run("AC-S2-050 (e) fsmonitor, showUntrackedFiles, skip-worktree, assume-unchanged, info/exclude", func(t *testing.T) {
		repo, base := newRepo(t)
		clean := hashOf(t, repo)
		plain, err := os.MkdirTemp("", "kchfsm") // git starts the program through a shell: no test name in its path
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(plain) })
		marker, script := filepath.Join(plain, "fsmonitor-ran"), filepath.Join(plain, "fsmonitor.sh")
		if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		gitRun(t, repo, "config", "core.fsmonitor", script)
		gitRun(t, repo, "config", "status.showUntrackedFiles", "no")
		write(t, repo, "docs/old.md", "edited, skip-worktree\n")
		gitRun(t, repo, "update-index", "--skip-worktree", "docs/old.md")
		write(t, repo, "README.md", "edited, assume-unchanged\n")
		gitRun(t, repo, "update-index", "--assume-unchanged", "README.md")
		write(t, repo, ".git/info/exclude", "docs/secret.txt\n")
		write(t, repo, "docs/secret.txt", "excluded by .git/info/exclude\n")
		write(t, repo, "docs/new.md", "hidden by showUntrackedFiles\n")
		// The fixture is a real attack: git status in the repository runs the script and reports a clean tree.
		_ = os.Remove(marker)
		if out := porcelain(t, repo); strings.Contains(out, "docs/") || strings.Contains(out, "README.md") {
			t.Fatalf("fixture: git status still shows %q", out)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("fixture: git status did not run the fsmonitor script: %v", err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}

		tamperCase(t, repo, base, clean, []string{"README.md", "docs/new.md", "docs/old.md", "docs/secret.txt"})
		// Every other git command the harness runs on or against this repository.
		ctx := context.Background()
		if head, err := HeadCommit(ctx, repo); err != nil || head != base {
			t.Fatalf("HeadCommit = %q, %v", head, err)
		}
		if ignore, err := RootIgnoreAtHead(ctx, repo); err != nil || !reflect.DeepEqual(ignore, []string{"ignored.log", "build/"}) {
			t.Fatalf("RootIgnoreAtHead = %v, %v", ignore, err)
		}
		fresh := Store{Dir: filepath.Join(t.TempDir(), "fresh")}
		if err := fresh.PinBase(ctx, repo, base); err != nil {
			t.Fatalf("PinBase: %v", err)
		}
		if ok, err := fresh.Has(ctx, repo, base); err != nil || !ok {
			t.Fatalf("Has = %v, %v", ok, err)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("a git command of the harness executed the repository's core.fsmonitor script")
		}
	})
}
