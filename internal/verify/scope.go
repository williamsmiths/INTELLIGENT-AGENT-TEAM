package verify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
)

// maxListed bounds how many paths one evidence detail carries.
const maxListed = 200

// ScopeCheck returns the changed paths that a task was not allowed to touch:
// a path violates when it matches a protected pattern (the always-protected
// ones included) or matches no allowed pattern. Paths are slash-separated and
// relative to the repo, as ChangedFiles returns them.
func ScopeCheck(changed []string, allowed, protected []string) (violations []string) {
	seen := map[string]bool{}
	for _, path := range changed {
		if seen[path] || !violates(path, allowed, protected) {
			continue
		}
		seen[path] = true
		violations = append(violations, path)
	}
	sort.Strings(violations)
	return violations
}

func violates(path string, allowed, protected []string) bool {
	for _, p := range guard.AlwaysProtected { // one list for the guard and the scope check (S2 D.1)
		if guard.Match(p, path) {
			return true
		}
	}
	for _, p := range protected {
		if guard.Match(p, path) {
			return true
		}
	}
	for _, p := range allowed {
		if guard.Match(p, path) {
			return false
		}
	}
	return true
}

// scopeItem is the built-in check "scope": every change since base_commit,
// committed or not, must stay inside the task's scope (AC-H-041, AC-H-046,
// limitation L4). It judges the same snapshot whose hash closes the run.
func scopeItem(ctx context.Context, req Request, s *snapshot) Item {
	it := Item{
		ID: newID(), CheckID: ScopeCheckID, Command: "scope: diff since " + req.BaseCommit,
		Result: ResultError, Detail: map[string]any{"base_commit": req.BaseCommit}, StartedAt: time.Now().UTC(),
	}
	fail := func(reason string, err error) Item {
		it.Detail["reason"] = reason
		it.Detail["error"] = err.Error()
		it.FinishedAt = time.Now().UTC()
		return it
	}
	changed, err := s.changed(ctx)
	if err != nil {
		return fail("diff failed", err)
	}
	violations := ScopeCheck(changed, req.Allowed, req.Protected)

	log, err := openLog(req.LogDir, req.TaskID, ScopeCheckID, it.StartedAt)
	if err != nil {
		return fail("log file unavailable", err)
	}
	it.LogPath = log.path
	var b strings.Builder
	fmt.Fprintf(&b, "scope check\nbase_commit: %s\ncandidate: %s\nchanged: %d\nviolations: %d\n", req.BaseCommit, s.tree, len(changed), len(violations))
	for _, p := range changed {
		fmt.Fprintf(&b, "changed\t%q\n", p)
	}
	for _, p := range violations {
		fmt.Fprintf(&b, "violation\t%q\n", p)
	}
	_, _ = log.Write([]byte(b.String()))
	sum, err := log.finish()
	it.LogSHA256 = sum
	if err != nil {
		return fail("log write failed", err)
	}

	it.Detail["changed_count"] = len(changed)
	code := 0
	it.Result = ResultPass
	if len(violations) > 0 {
		code = 1
		it.Result = ResultFail
		it.Detail["violation_count"] = len(violations)
		if len(violations) > maxListed {
			violations = violations[:maxListed] // the log file keeps the full list
		}
		it.Detail["violations"] = violations
	}
	it.ExitCode = &code
	it.FinishedAt = time.Now().UTC()
	return it
}
