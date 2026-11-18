package api_test

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// AC-S2-079 (spec E.1-13): the paths a Lead writes become repo-relative globs.
// Team pilot evidence: "/repo/prototype/" refused every write into prototype/.
func TestAC_S2_079_AllowedPathsNormalized(t *testing.T) {
	e := newTeam(t)
	repo := e.repo["alpha"]
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := e.delegateMust(http.StatusCreated, "PL", map[string]any{"allowed_paths": []string{
		repo + "/prototype/", "./src/**", "docs", "README.md", repo, "././lib//"}})
	if got := fmt.Sprint(out["allowed_paths"]); got != "[prototype/** src/** docs/** README.md ** lib/**]" {
		t.Fatalf("allowed_paths %s", got)
	}
	if notice := e.lastSent("EL"); !strings.Contains(notice, "prototype/**") || strings.Contains(notice, repo+"/prototype/") {
		t.Fatalf("the notice must show the normalized scope:\n%s", notice)
	}
	if code, o := e.kch("EL", "release"); code != http.StatusOK {
		t.Fatalf("release: %d %s", code, o["_raw"])
	}
	before := e.tasks("alpha")
	for _, bad := range []string{"/etc/passwd", "../other/**", "src/../../x"} {
		refused(t, e.delegateMust(http.StatusUnprocessableEntity, "PL", map[string]any{"allowed_paths": []string{bad}}), "allowed_paths")
	}
	if after := e.tasks("alpha"); after != before {
		t.Fatalf("a refused delegate left a task: %d → %d", before, after)
	}
}
