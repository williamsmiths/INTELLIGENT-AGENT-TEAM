package guard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func link(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

// AC-S2-053: a path is resolved the way the kernel resolves it, so no chain of
// symbolic links, ".." or NUL byte gets a write judged at another place than
// the one where the file would be created.
func TestCheckWrite_AC_S2_053(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo, other, outside, tmp := filepath.Join(base, "repo"), filepath.Join(base, "other-repo"), filepath.Join(base, "outside"), filepath.Join(base, "tmp")
	for _, d := range []string{"repo/src", "repo/docs", "repo/tests/acceptance", "other-repo", "outside", "tmp/a/b"} {
		mk(t, filepath.Join(base, d))
	}
	link(t, "../docs", filepath.Join(repo, "src", "d"))                        // relative link to a sibling directory
	link(t, "../tests/acceptance/x_test.go", filepath.Join(repo, "docs/dang")) // dangling, relative to docs/
	link(t, "../..", filepath.Join(tmp, "a", "b", "q"))                        // tmp/a/b/q is tmp itself
	link(t, "../other-repo/planted.go", filepath.Join(tmp, "dang"))            // dangling, into another registered repo
	link(t, outside, filepath.Join(repo, "src", "odir"))                       // directory link out of the repo
	link(t, "newdir/file.go", filepath.Join(repo, "src", "dang-in"))           // dangling, target inside the allowed scope
	link(t, filepath.Join(tmp, "later.txt"), filepath.Join(tmp, "dang-tmp"))   // dangling, target inside an extra root
	link(t, "loop-b", filepath.Join(repo, "src", "loop-a"))                    // a link loop
	link(t, "loop-a", filepath.Join(repo, "src", "loop-b"))                    //
	link(t, filepath.Join(repo, "src"), filepath.Join(tmp, "into-repo"))       // extra root link into the repo
	r := Rules{Repo: repo, Allowed: []string{"src/**"}, Protected: []string{"tests/acceptance/**"}, ExtraRoots: []string{tmp}}

	cases := []struct {
		name, path, cwd string
		allow           bool
		rule, at        string // at: where the write must be judged ("" = not asserted)
	}{
		{"AC-S2-053 relative dangling link behind a directory link", filepath.Join(repo, "src/d/dang"), "", false, "protected",
			filepath.Join(repo, "tests/acceptance/x_test.go")},
		{"AC-S2-053 same, relative path", "src/d/dang", repo, false, "protected", filepath.Join(repo, "tests/acceptance/x_test.go")},
		{"AC-S2-053 dangling link from an extra root into another repo", filepath.Join(tmp, "dang"), "", false, "outside_scope",
			filepath.Join(other, "planted.go")},
		{"AC-S2-053 same, reached through a link to its own directory", filepath.Join(tmp, "a/b/q/dang"), "", false, "outside_scope",
			filepath.Join(other, "planted.go")},
		{"AC-S2-053 dotdot after a directory link", filepath.Join(repo, "src/odir") + "/../x.go", "", false, "symlink_escape",
			filepath.Join(base, "x.go")},
		{"AC-S2-053 dotdot after a directory link, relative", "src/odir/../x.go", repo, false, "symlink_escape", filepath.Join(base, "x.go")},
		{"AC-S2-053 dotdot after a link back into the repo", filepath.Join(repo, "src/d") + "/../tests/acceptance/y_test.go", "", false, "protected",
			filepath.Join(repo, "tests/acceptance/y_test.go")},
		{"AC-S2-053 NUL in the path", filepath.Join(repo, "src/a.go") + "\x00.md", "", false, "bad_path", ""},
		{"AC-S2-053 NUL in the cwd", "src/a.go", repo + "\x00", false, "bad_path", ""},
		{"AC-S2-053 dangling link to an allowed place", filepath.Join(repo, "src/dang-in"), "", false, "dangling_symlink",
			filepath.Join(repo, "src/newdir/file.go")},
		{"AC-S2-053 dangling link inside an extra root", filepath.Join(tmp, "dang-tmp"), "", false, "dangling_symlink", filepath.Join(tmp, "later.txt")},
		{"AC-S2-053 link loop", filepath.Join(repo, "src/loop-a/x.go"), "", false, "bad_path", ""},
		{"AC-S2-053 extra root link into a protected place", filepath.Join(tmp, "into-repo") + "/../tests/acceptance/z_test.go", "", false, "protected",
			filepath.Join(repo, "tests/acceptance/z_test.go")},
		{"AC-S2-053 git control file", filepath.Join(repo, "src/.gitignore"), "", false, "protected", ""},
		{"AC-S2-053 git attributes file", filepath.Join(repo, ".gitattributes"), "", false, "protected", ""},
		{"AC-S2-053 git modules file", filepath.Join(repo, ".gitmodules"), "", false, "protected", ""},
		{"AC-S2-053 new file in a new real directory", filepath.Join(repo, "src/new/deep/dir/a.go"), "", true, "allowed",
			filepath.Join(repo, "src/new/deep/dir/a.go")},
		{"AC-S2-053 dotdot out of a directory that does not exist yet", filepath.Join(repo, "src/new") + "/../b.go", "", true, "allowed",
			filepath.Join(repo, "src/b.go")},
		{"AC-S2-053 existing directory link that stays in scope", filepath.Join(tmp, "into-repo/c.go"), "", true, "allowed", filepath.Join(repo, "src/c.go")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := r.CheckWrite(c.path, c.cwd)
			if d.Allow != c.allow || d.Rule != c.rule {
				t.Fatalf("got allow=%v rule=%s (%s), want allow=%v rule=%s", d.Allow, d.Rule, d.Reason, c.allow, c.rule)
			}
			if c.at != "" && d.Path != c.at {
				t.Fatalf("judged at %s, want %s", d.Path, c.at)
			}
			if strings.ContainsRune(d.Reason, 0) {
				t.Fatal("the reason echoes a NUL byte")
			}
		})
	}
}

// AC-S2-053: resolution agrees with the kernel on every path that exists.
func TestResolve_AC_S2_053_AgreesWithKernel(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk(t, filepath.Join(base, "a/b/c"))
	mk(t, filepath.Join(base, "x/y"))
	if err := os.WriteFile(filepath.Join(base, "x/y/f"), []byte("f"), 0o644); err != nil {
		t.Fatal(err)
	}
	link(t, "../../x", filepath.Join(base, "a/b/up"))                   // relative, climbs
	link(t, "b/up/y", filepath.Join(base, "a/chain"))                   // link to a path that holds a link
	link(t, filepath.Join(base, "a/chain"), filepath.Join(base, "abs")) // absolute link to a link
	for _, p := range []string{"a/b/up/y/f", "a/chain/f", "abs/f", "a/b/up/../a/b/c", "abs/../y/f", "a/./b//c/../../chain/f"} {
		full := base + "/" + p
		want, err := os.Stat(full) // the kernel resolves the raw string; filepath.EvalSymlinks would clean it first
		if err != nil {
			t.Fatalf("%s: fixture path does not exist: %v", p, err)
		}
		got, err := Resolve(full, "")
		if err != nil || got.Dangling {
			t.Fatalf("%s: %+v %v", p, got, err)
		}
		if st, err := os.Stat(got.Path); err != nil || !os.SameFile(want, st) {
			t.Fatalf("%s: resolved to %s, which is not the file the kernel opens", p, got.Path)
		}
		if real, err := filepath.EvalSymlinks(got.Path); err != nil || real != got.Path {
			t.Fatalf("%s: resolved path %s still holds a link or a dot segment (%s, %v)", p, got.Path, real, err)
		}
	}
}
