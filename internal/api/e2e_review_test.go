package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// Regression tests for the findings of the independent code review REV-003
// and for the acceptance criteria it found untested.
func TestReviewFindings(t *testing.T) {
	e := newEnv(t)
	e.seed()
	o, T, U := e.owner, e.id["T"], e.id["U"]
	write := func(agent, path string) string {
		return e.hook(agent, "pre-tool", map[string]any{"tool_name": "Write", "cwd": e.repo["alpha"], "tool_input": map[string]any{"file_path": path}})
	}

	t.Run("REV-003 #5 a dirty repo cannot be the base of a task", func(t *testing.T) {
		e.write(e.in("alpha", "src", "leftover.txt"), "from an earlier task\n")
		e.must(http.StatusConflict, o, "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]})
		e.commit("alpha")
	})

	for _, mode := range []string{"OBSERVE", "CONTEXT"} {
		t.Run("AC-H-028 AC-H-081 REV-003 #4 lease in mode "+mode, func(t *testing.T) {
			e.must(http.StatusOK, o, "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["Z"], "mode": mode})
			ctx := e.hook("Z", "prompt-submit", nil)
			if (mode == "OBSERVE") != (ctx == "") {
				t.Fatalf("mode %s: context %q", mode, ctx)
			}
			if mode == "CONTEXT" {
				has(t, e.hook("Z", "session-start", map[string]any{"source": "resume"}), "SessionStart", "ALPHA-RULE", "EDITOR-1") // AC-H-029
			}
			// The task scope is not enforced in these modes...
			if out := write("Z", e.in("alpha", "docs", "x.md")); out != "" {
				t.Fatalf("mode %s: out-of-scope write in the task repo refused: %s", mode, out)
			}
			e.write(e.in("alpha", "src", "ok.txt"), "x\n")
			e.hook("Z", "post-tool", map[string]any{"tool_name": "Write", "tool_use_id": "m-" + mode, "tool_input": map[string]any{"file_path": e.in("alpha", "src", "ok.txt")}})
			if out := e.hook("Z", "stop", map[string]any{}); out != "" {
				t.Fatalf("mode %s: stop blocked: %s", mode, out)
			}
			// ...but another registered repo stays closed, even one of a project Z belongs to.
			has(t, write("Z", e.in("beta", "src", "a.go")), "deny", "repo khác")
			has(t, write("W", e.in("beta", "src", "a.go")), "deny", "project khác")
			e.must(http.StatusOK, e.tok["Z"], "POST", "/v1/tasks/"+T+"/release", nil)
			e.reset("alpha")
		})
	}

	t.Run("REV-003 #6 the lease lock covers routes without a project in the path", func(t *testing.T) {
		e.must(http.StatusOK, o, "POST", "/v1/tasks/"+U+"/assign", map[string]any{"principal": e.id["Z"]})
		e.must(http.StatusConflict, e.tok["Z"], "GET", "/v1/tasks/"+T+"/context", nil)
		e.must(http.StatusConflict, e.tok["Z"], "GET", "/v1/tasks/"+T+"/evidence", nil)
		e.must(http.StatusOK, e.tok["Z"], "GET", "/v1/tasks/"+U+"/context", nil)
		e.must(http.StatusNotFound, e.tok["W"], "GET", "/v1/tasks/"+U+"/context", nil) // not a member: still 404, not 409
	})

	t.Run("REV-003 #7 a token is never stored for replay; AC-H-006 AC-H-013 replays", func(t *testing.T) {
		body := map[string]any{"name": "N", "openbot_agent_id": "agent-N", "enforcement": "HOOK", "project_id": e.id["alpha"], "role": "worker"}
		first := e.must(http.StatusCreated, o, "POST", "/v1/principals", body, "Idempotency-Key", "p-1")
		if !strings.HasPrefix(str(first, "token"), "kch_") {
			t.Fatalf("no token: %s", first["_raw"])
		}
		// Not replayed: the second call is a new attempt and fails on the unique agent id.
		if status, out := e.call(o, "POST", "/v1/principals", body, "Idempotency-Key", "p-1"); status == http.StatusOK || strings.Contains(out["_raw"].(string), "kch_") {
			t.Fatalf("token replayed: %d %s", status, out["_raw"])
		}
		// Assign replay returns the same lease (AC-H-013).
		a := e.must(http.StatusOK, o, "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]}, "Idempotency-Key", "as-1")
		b := e.must(http.StatusOK, o, "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]}, "Idempotency-Key", "as-1")
		if str(a, "granted_lease", "id") == "" || str(a, "granted_lease", "id") != str(b, "granted_lease", "id") {
			t.Fatalf("assign replay: %s vs %s", a["_raw"], b["_raw"])
		}
	})

	t.Run("REV-003 #12 the check id scope is reserved", func(t *testing.T) {
		e.must(http.StatusUnprocessableEntity, o, "POST", "/v1/projects/"+e.id["alpha"]+"/tasks", map[string]any{"key": "X-1", "title": "x",
			"repo_path": e.repo["alpha"], "required_checks": []map[string]any{{"id": "scope", "command": "true"}}})
	})
}
