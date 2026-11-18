package api_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// gitIn runs git in a project repo the way an agent with a shell would.
func (e *env) gitIn(project string, args ...string) string {
	e.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = e.repo[project]
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		e.t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (e *env) bash(agent, id string) string {
	return e.hook(agent, "post-tool", map[string]any{"tool_name": "Bash", "tool_use_id": id})
}

func (e *env) events(project, typ string) int {
	return countType(e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id[project]+"/events?limit=500", nil), typ)
}

// evidence returns the newest evidence row of one check of a task.
func (e *env) evidence(tok, task, check string) map[string]any {
	e.t.Helper()
	out := e.must(http.StatusOK, tok, "GET", "/v1/tasks/"+task+"/evidence", nil)
	rows, _ := out["evidence"].([]any)
	for _, raw := range rows {
		if row, ok := raw.(map[string]any); ok && row["CheckID"] == check {
			return row
		}
	}
	e.t.Fatalf("no evidence for check %q: %s", check, out["_raw"])
	return nil
}

// Spec S2 part D over HTTP: real PostgreSQL, real git repositories, real bwrap.
func TestHardening(t *testing.T) {
	e := newEnv(t)
	e.seed()
	o, T, U := e.owner, e.id["T"], e.id["U"]
	alpha := e.repo["alpha"]
	e.must(http.StatusOK, e.tok["L"], "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]})
	taskState := func() string { return str(e.must(http.StatusOK, o, "GET", "/v1/tasks/"+T, nil), "task", "state") }

	t.Run("AC-S2-050 changes hidden from git are caught after Bash and at verify; no repo program runs", func(t *testing.T) {
		plain, err := os.MkdirTemp("", "kchfsm") // git starts the program through a shell: no test name in its path
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(plain) })
		marker := filepath.Join(plain, "ran")
		e.write(filepath.Join(plain, "fsmonitor.sh"), "#!/bin/sh\ntouch '"+marker+"'\n")
		if err := os.Chmod(filepath.Join(plain, "fsmonitor.sh"), 0o755); err != nil {
			t.Fatal(err)
		}
		e.gitIn("alpha", "config", "core.fsmonitor", filepath.Join(plain, "fsmonitor.sh"))
		e.gitIn("alpha", "config", "status.showUntrackedFiles", "no")
		e.write(e.in("alpha", "docs", ".gitignore"), "*\n")
		e.write(e.in("alpha", "docs", "x.py"), "print('outside the scope')\n")
		e.write(e.in("alpha", "docs", "readme.md"), "edited outside the scope\n")
		e.gitIn("alpha", "update-index", "--skip-worktree", "docs/readme.md")
		e.write(e.in("alpha", "src", "ok.txt"), "ok\n")
		if out := e.gitIn("alpha", "status", "--porcelain"); strings.Contains(out, "docs/") {
			t.Fatalf("fixture: git status shows %q", out)
		}
		if _, err := os.Stat(marker); err != nil {
			t.Fatalf("fixture: git status does not run the fsmonitor script: %v", err)
		}
		if err := os.Remove(marker); err != nil {
			t.Fatal(err)
		}

		has(t, e.bash("W", "h-1"), "block", "docs/.gitignore", "docs/readme.md", "docs/x.py")
		out := e.must(http.StatusOK, e.tok["W"], "POST", "/v1/tasks/"+T+"/verify", nil)
		if out["all_pass"] != false || out["task_state"] != "RUNNING" {
			t.Fatalf("verify passed although files outside the scope changed: %s", out["_raw"])
		}
		scope := e.evidence(e.tok["W"], T, "scope")
		raw, _ := json.Marshal(scope["Detail"])
		if scope["Result"] != "FAIL" {
			t.Fatalf("scope evidence: %v", scope)
		}
		has(t, string(raw), "docs/.gitignore", "docs/readme.md", "docs/x.py")
		// AC-S2-010: the required check ran inside the sandbox, and the evidence says so.
		unit := e.evidence(e.tok["W"], T, "unit")
		if d, _ := unit["Detail"].(map[string]any); unit["Result"] != "PASS" || d["sandbox"] != "bwrap" {
			t.Fatalf("unit evidence: %v", unit)
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatal("harnessd executed the repository's core.fsmonitor script")
		}

		e.gitIn("alpha", "config", "--unset", "core.fsmonitor")
		e.gitIn("alpha", "update-index", "--no-skip-worktree", "docs/readme.md")
		e.gitIn("alpha", "checkout", "-q", "--", "docs/readme.md")
		for _, p := range []string{"docs/.gitignore", "docs/x.py"} {
			if err := os.Remove(filepath.Join(alpha, p)); err != nil {
				t.Fatal(err)
			}
		}
		if out := e.bash("W", "h-2"); out != "" {
			t.Fatalf("still blocked after the revert: %s", out)
		}
	})

	t.Run("AC-S2-051 unreadable tree: post-tool blocks, SNAPSHOT_FAILED once per run, verify is ERROR", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every directory")
		}
		locked := e.in("alpha", "docs", "locked")
		e.write(filepath.Join(locked, "hidden.md"), "outside the scope\n")
		lock := func(mode os.FileMode) {
			if err := os.Chmod(locked, mode); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		lock(0)
		for _, id := range []string{"h-3", "h-4"} {
			has(t, e.bash("W", id), "block", "không đọc được", "docs/locked")
		}
		if n := e.events("alpha", "SNAPSHOT_FAILED"); n != 1 {
			t.Fatalf("SNAPSHOT_FAILED journaled %d times for one run of failures, want 1", n)
		}
		has(t, e.hook("W", "stop", map[string]any{"stop_hook_active": false}), "block", "kch verify")
		out := e.must(http.StatusOK, e.tok["W"], "POST", "/v1/tasks/"+T+"/verify", nil)
		if out["all_pass"] != false || out["candidate"] != "" || out["task_state"] != "RUNNING" || taskState() != "RUNNING" {
			t.Fatalf("verify on an unreadable tree: %s", out["_raw"])
		}
		for _, check := range []string{"unit", "scope"} {
			row := e.evidence(e.tok["W"], T, check)
			if d, _ := row["Detail"].(map[string]any); row["Result"] != "ERROR" || d["reason"] != "snapshot failed" {
				t.Fatalf("%s evidence: %v", check, row)
			}
		}
		// Readable again: the violation is reported as such; the next failure starts a new run.
		lock(0o755)
		has(t, e.bash("W", "h-5"), "block", "docs/locked/hidden.md")
		lock(0)
		has(t, e.bash("W", "h-6"), "block", "không đọc được")
		if n := e.events("alpha", "SNAPSHOT_FAILED"); n != 2 {
			t.Fatalf("SNAPSHOT_FAILED journaled %d times for two runs of failures, want 2", n)
		}
		lock(0o755)
		if err := os.RemoveAll(locked); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("AC-S2-051 core.bare=true and a nested repository do not blind the post-tool check", func(t *testing.T) {
		e.gitIn("alpha", "config", "core.bare", "true")
		e.write(e.in("alpha", "docs", "nested", "y.md"), "outside the scope\n")
		e.gitIn("alpha", "init", "-q", e.in("alpha", "docs", "nested"))
		has(t, e.bash("W", "h-7"), "block", "docs/nested/y.md")
		e.gitIn("alpha", "config", "--unset", "core.bare")
		if err := os.RemoveAll(e.in("alpha", "docs", "nested")); err != nil {
			t.Fatal(err)
		}
		if out := e.bash("W", "h-8"); out != "" {
			t.Fatalf("still blocked after the revert: %s", out)
		}
		out := e.must(http.StatusOK, e.tok["W"], "POST", "/v1/tasks/"+T+"/verify", nil)
		if out["all_pass"] != true || out["task_state"] != "VERIFIED" {
			t.Fatalf("clean tree does not verify: %s", out["_raw"])
		}
	})

	t.Run("AC-S2-052 trusted ignore list: owner-only endpoint, dirty root refused until the owner lists it", func(t *testing.T) {
		assign := func() (int, map[string]any) {
			return e.call(o, "POST", "/v1/tasks/"+U+"/assign", map[string]any{"principal": e.id["V"]})
		}
		e.write(e.in("beta", "node_modules", "dep", "index.js"), "dependency\n")
		e.write(e.in("beta", ".gitignore"), "node_modules/\n") // in the working tree only: not trusted
		if status, out := assign(); status != http.StatusConflict {
			t.Fatalf("assign on a dirty root: %d %s", status, out["_raw"])
		}
		if err := os.Remove(e.in("beta", ".gitignore")); err != nil {
			t.Fatal(err)
		}
		if status, out := assign(); status != http.StatusConflict || !strings.Contains(out["_raw"].(string), "node_modules/dep/index.js") {
			t.Fatalf("assign on a dirty root: %d %s", status, out["_raw"])
		}
		path := "/v1/projects/" + e.id["beta"] + "/repos/ignore"
		body := map[string]any{"path": e.repo["beta"], "patterns": []string{"# dependencies", "node_modules/"}}
		e.must(http.StatusForbidden, e.tok["V"], "PUT", path, body)
		e.must(http.StatusForbidden, e.tok["L"], "PUT", path, body)
		e.must(http.StatusUnprocessableEntity, o, "PUT", path, map[string]any{"path": e.repo["alpha"], "patterns": []string{"x"}})
		e.must(http.StatusUnprocessableEntity, o, "PUT", path, map[string]any{"patterns": []string{"a\nb"}})
		out := e.must(http.StatusOK, o, "PUT", path, body)
		if got, _ := json.Marshal(out["ignore_patterns"]); string(got) != `["node_modules/"]` || out["root"] != e.repo["beta"] {
			t.Fatalf("stored list: %s", out["_raw"])
		}
		if status, out := assign(); status != http.StatusOK {
			t.Fatalf("assign after the owner listed node_modules/: %d %s", status, out["_raw"])
		}
		if n := e.events("beta", "REPO_IGNORE_SET"); n != 1 {
			t.Fatalf("REPO_IGNORE_SET journaled %d times", n)
		}
	})

	t.Run("AC-S2-010 a check with network: true keeps the flag from the task to the evidence", func(t *testing.T) {
		e.must(http.StatusOK, e.tok["V"], "POST", "/v1/tasks/"+U+"/release", nil)
		hostNet, err := os.Readlink("/proc/self/ns/net")
		if err != nil {
			t.Fatal(err)
		}
		same := `test "$(readlink /proc/self/ns/net)" = "` + hostNet + `"`
		created := e.must(http.StatusCreated, o, "POST", "/v1/projects/"+e.id["beta"]+"/tasks", map[string]any{
			"key": "NET-1", "title": "net", "desired_outcome": "done", "scope": "feature:editor", "repo_path": e.repo["beta"],
			"allowed_paths": []string{"src/**"}, "requirement_keys": []string{"REQ-1"},
			"required_checks": []map[string]any{{"id": "online", "command": same, "network": true}, {"id": "offline", "command": "! " + same}}})
		N := str(created, "id")
		e.must(http.StatusOK, o, "POST", "/v1/tasks/"+N+"/ready", nil)
		e.must(http.StatusOK, o, "POST", "/v1/tasks/"+N+"/assign", map[string]any{"principal": e.id["V"]})
		out := e.must(http.StatusOK, e.tok["V"], "POST", "/v1/tasks/"+N+"/verify", nil)
		if out["all_pass"] != true {
			t.Fatalf("verify: %s", out["_raw"])
		}
		for check, network := range map[string]bool{"online": true, "offline": false} {
			row := e.evidence(e.tok["V"], N, check)
			if d, _ := row["Detail"].(map[string]any); d["sandbox"] != "bwrap" || d["network"] != network {
				t.Fatalf("%s evidence: %v", check, row)
			}
		}
	})

	t.Run("AC-S2-054 a row left in flight is taken over after two minutes", func(t *testing.T) {
		body := map[string]any{"tool_name": "Read", "tool_use_id": "idem-1"}
		raw, _ := json.Marshal(body)
		sum := sha256.Sum256(raw)
		route, key := "POST /v1/hooks/claude/post-tool", "crashed-request"
		seedRow := func(age string) {
			if err := store.InScope(context.Background(), e.pool, store.Runtime, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), `INSERT INTO idempotency (principal_id, route, key, body_hash, status, body, created_at)
					VALUES ($1, $2, $3, $4, 0, 'null', now() - $5::interval)
					ON CONFLICT (principal_id, route, key) DO UPDATE SET status = 0, body = 'null', created_at = now() - $5::interval`,
					e.id["W"], route, key, hex.EncodeToString(sum[:]), age)
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		send := func() (int, map[string]any) {
			return e.call(e.tok["W"], "POST", "/v1/hooks/claude/post-tool", body, "Idempotency-Key", key)
		}
		seedRow("30 seconds") // a first request that may still be running
		if status, out := send(); status != http.StatusConflict {
			t.Fatalf("a fresh in-flight row answered %d %s", status, out["_raw"])
		}
		seedRow("121 seconds") // the process that claimed the key is gone
		if status, out := send(); status != http.StatusOK {
			t.Fatalf("a stale in-flight row answered %d %s, want the request to be handled", status, out["_raw"])
		}
		var status int
		if err := store.InScope(context.Background(), e.pool, store.Runtime, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT status FROM idempotency WHERE principal_id = $1 AND route = $2 AND key = $3`,
				e.id["W"], route, key).Scan(&status)
		}); err != nil || status != http.StatusOK {
			t.Fatalf("stored status %d, %v", status, err)
		}
		if status, out := send(); status != http.StatusOK { // and now it replays
			t.Fatalf("replay answered %d %s", status, out["_raw"])
		}
	})
}
