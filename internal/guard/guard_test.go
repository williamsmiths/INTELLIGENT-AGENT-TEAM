package guard

import (
	"os"
	"path/filepath"
	"testing"
)

func mk(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

// AC-H-022, AC-H-023: every path trick is refused, a new file in a new directory is accepted.
func TestCheckWrite(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(base, "repo")
	outside := filepath.Join(base, "outside")
	tmp := filepath.Join(base, "tmp")
	for _, d := range []string{"src/gen", "tests/acceptance", "docs", ".git", ".claude"} {
		mk(t, filepath.Join(repo, d))
	}
	mk(t, outside)
	mk(t, tmp)
	if err := os.Symlink(outside, filepath.Join(repo, "src", "out")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repo, "tests", "acceptance"), filepath.Join(repo, "src", "p")); err != nil {
		t.Fatal(err)
	}
	// Links whose targets do not exist yet: writing through them would create the target (REV-003 #3).
	if err := os.Symlink(filepath.Join(repo, "tests", "acceptance", "new_test.go"), filepath.Join(repo, "src", "dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "new.txt"), filepath.Join(repo, "src", "dangling-out")); err != nil {
		t.Fatal(err)
	}
	r := Rules{Repo: repo, Allowed: []string{"src/**"}, Protected: []string{"src/gen/**", "tests/acceptance/**"}, ExtraRoots: []string{tmp}}

	cases := []struct {
		name, path, cwd string
		allow           bool
		rule            string
	}{
		{"in scope", filepath.Join(repo, "src/a.go"), "", true, "allowed"},
		{"new file in new dir", filepath.Join(repo, "src/new/dir/a.go"), "", true, "allowed"},
		{"relative in scope", "src/b.go", repo, true, "allowed"},
		{"outside allowed", filepath.Join(repo, "docs/x.md"), "", false, "not_allowed"},
		{"protected test", filepath.Join(repo, "tests/acceptance/a_test.go"), "", false, "protected"},
		{"protected wins over allowed", filepath.Join(repo, "src/gen/a.go"), "", false, "protected"},
		{"dotdot", filepath.Join(repo, "src") + "/../docs/x.md", "", false, "not_allowed"},
		{"symlink out of repo", filepath.Join(repo, "src/out/y"), "", false, "symlink_escape"},
		{"symlink into protected", filepath.Join(repo, "src/p/a_test.go"), "", false, "protected"},
		{"dangling link into protected", filepath.Join(repo, "src/dangling"), "", false, "protected"},
		{"dangling link out of repo", filepath.Join(repo, "src/dangling-out"), "", false, "symlink_escape"},
		{"git internals", filepath.Join(repo, ".git/config"), "", false, "protected"},
		{"hook file", filepath.Join(repo, ".claude/settings.json"), "", false, "protected"},
		{"relative escape", "../other/x", repo, false, "outside_scope"},
		{"temp root", filepath.Join(tmp, "scratch.txt"), "", true, "extra_root"},
		{"elsewhere", filepath.Join(outside, "z"), "", false, "outside_scope"},
		{"empty", "", repo, false, "bad_path"},
	}
	for _, c := range cases {
		d := r.CheckWrite(c.path, c.cwd)
		if d.Allow != c.allow || d.Rule != c.rule {
			t.Errorf("%s: got allow=%v rule=%s (%s), want allow=%v rule=%s", c.name, d.Allow, d.Rule, d.Reason, c.allow, c.rule)
		}
	}
	if d := (Rules{Repo: repo, ExtraRoots: []string{tmp}}).CheckWrite(filepath.Join(repo, "src/a.go"), ""); d.Allow {
		t.Error("empty allowed_paths must allow nothing inside the repo")
	}
}

func TestMatch(t *testing.T) {
	for _, c := range []struct {
		pat, rel string
		want     bool
	}{
		{"src/**", "src/a.go", true}, {"src/**", "src/x/y/z.go", true}, {"src/**", "srcx/a.go", false},
		{"**/*_test.go", "a_test.go", true}, {"**/*_test.go", "pkg/x/a_test.go", true}, {"**/*_test.go", "pkg/a.go", false},
		{"*.md", "README.md", true}, {"*.md", "docs/README.md", false}, {"src/?.go", "src/a.go", true}, {"src/?.go", "src/ab.go", false},
		{"go.mod", "go.mod", true}, {"go.mod", "x/go.mod", false},
	} {
		if got := Match(c.pat, c.rel); got != c.want {
			t.Errorf("Match(%q,%q)=%v want %v", c.pat, c.rel, got, c.want)
		}
	}
}
