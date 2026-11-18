package verify

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// REV-003 #1: nothing an agent writes under .git can hide a change from the
// candidate hash or from the scope check.
func TestSnapshotIgnoresRepositoryLocalGitConfig(t *testing.T) {
	repo, _ := newRepo(t)
	clean := hashOf(t, repo)

	// The agent hides a directory through .git/info/exclude and neutralises a
	// tracked file through a clean filter configured in .git/config.
	if err := os.MkdirAll(filepath.Join(repo, ".git", "info"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "exclude"), []byte("hidden/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "info", "attributes"), []byte("* filter=blank\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "config", "filter.blank.clean", "git show HEAD:%f 2>/dev/null || cat")
	write(t, repo, "hidden/leak.txt", "written outside the scope\n")
	tracked := firstTracked(t, repo)
	write(t, repo, tracked, "tampered content\n")

	if after := hashOf(t, repo); after == clean {
		t.Fatal("candidate hash did not change although files changed")
	}
	changed := changedFiles(t, repo)
	for _, want := range []string{"hidden/leak.txt", tracked} {
		if !slices.Contains(changed, want) {
			t.Fatalf("%s missing from changed files %v", want, changed)
		}
	}
	if v := ScopeCheck(changed, []string{"src/**"}, nil); len(v) == 0 {
		t.Fatalf("scope check passed with changes %v", changed)
	}
}

// firstTracked returns one file tracked at HEAD.
func firstTracked(t *testing.T, repo string) string {
	t.Helper()
	out := gitRun(t, repo, "ls-files")
	for _, line := range splitLines(out) {
		if line != "" {
			return line
		}
	}
	t.Fatal("repository has no tracked file")
	return ""
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(r)
	}
	return append(out, cur)
}
