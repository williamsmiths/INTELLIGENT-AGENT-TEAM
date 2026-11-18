package api_test

import (
	"net/http"
	"os"
	"testing"
)

// One real run of the vertical slice over HTTP: two projects, real PostgreSQL,
// real git repositories and the real verification runner.
func TestVerticalSlice(t *testing.T) {
	e := newEnv(t)
	e.seed()
	o, T := e.owner, e.id["T"]
	taskState := func() string { return str(e.must(http.StatusOK, o, "GET", "/v1/tasks/"+T, nil), "task", "state") }

	t.Run("AC-H-017 lead may not assign to itself, AC-H-010 assign", func(t *testing.T) {
		e.must(http.StatusForbidden, e.tok["L"], "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["L"]})
		e.must(http.StatusForbidden, e.tok["W"], "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]})
		e.must(http.StatusOK, e.tok["L"], "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]})
		if taskState() != "RUNNING" {
			t.Fatalf("state %s", taskState())
		}
		if len(e.host.sent) != 1 {
			t.Fatalf("assignment was not delivered through the host: %v", e.host.sent)
		}
	})

	t.Run("AC-H-020 AC-H-080 AC-H-009 context before the model call", func(t *testing.T) {
		out := e.hook("W", "prompt-submit", map[string]any{"prompt": "bắt đầu"})
		has(t, out, "additionalContext", "EDITOR-1", "ALPHA-RULE", "src/**", "Ngân sách", "context_hash", "Nút thêm nằm ngoài Part", "Nút thêm nằm trong Part", "kch verify")
		lacks(t, out, "BETA-RULE")
	})

	t.Run("AC-H-021 no lease, no context", func(t *testing.T) {
		if out := e.hook("V", "prompt-submit", map[string]any{"prompt": "x"}); out != "" {
			t.Fatalf("got %s", out)
		}
	})

	t.Run("AC-H-022 AC-H-023 AC-H-081 AC-H-083 write scope", func(t *testing.T) {
		if out := e.edit("W", e.in("alpha", "src", "a.go")); out != "" {
			t.Fatalf("in-scope write refused: %s", out)
		}
		has(t, e.edit("W", e.in("alpha", "docs", "x.md")), "deny", "allowed_paths")
		has(t, e.edit("W", e.in("alpha", "tests", "acceptance", "oracle.txt")), "deny", "bảo vệ")
		has(t, e.edit("W", e.in("alpha", ".claude", "settings.json")), "deny")
		has(t, e.edit("W", e.in("beta", "src", "a.go")), "deny", "project khác")
		// A project id typed into the body changes nothing (AC-H-083, AC-H-025).
		out := e.hook("W", "pre-tool", map[string]any{"tool_name": "Write", "project_id": e.id["beta"], "principal_id": e.id["V"],
			"tool_input": map[string]any{"file_path": e.in("beta", "src", "a.go")}})
		has(t, out, "deny", "project khác")
	})

	t.Run("AC-H-024 governed repo needs a lease, scratch does not", func(t *testing.T) {
		has(t, e.hook("V", "pre-tool", map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": e.in("beta", "src", "x.go")}}), "deny", "lease")
		if out := e.hook("V", "pre-tool", map[string]any{"tool_name": "Write", "tool_input": map[string]any{"file_path": e.tmp + "/n.txt"}}); out != "" {
			t.Fatalf("scratch write refused: %s", out)
		}
	})

	t.Run("AC-H-026 tool call counted once", func(t *testing.T) {
		for range 2 {
			e.hook("W", "post-tool", map[string]any{"tool_name": "Read", "tool_use_id": "tu-1"})
		}
		b := e.must(http.StatusOK, e.tok["W"], "GET", "/v1/tasks/"+T+"/budget", nil)
		if n := b["task"].(map[string]any)["used"].(map[string]any)["tool_calls"].(float64); n != 1 {
			t.Fatalf("tool_calls = %v, want 1", n)
		}
	})

	t.Run("AC-H-027 stop without evidence is blocked once", func(t *testing.T) {
		e.write(e.in("alpha", "src", "ok.txt"), "ok\n")
		e.hook("W", "post-tool", map[string]any{"tool_name": "Write", "tool_use_id": "tu-2", "tool_input": map[string]any{"file_path": e.in("alpha", "src", "ok.txt")}})
		has(t, e.hook("W", "stop", map[string]any{"stop_hook_active": false}), "block", "kch verify")
		if out := e.hook("W", "stop", map[string]any{"stop_hook_active": true}); out != "" {
			t.Fatalf("second stop blocked: %s", out)
		}
	})

	t.Run("AC-H-040 AC-H-042 AC-H-044 runner evidence", func(t *testing.T) {
		e.must(http.StatusNotFound, e.tok["V"], "POST", "/v1/tasks/"+T+"/verify", nil)
		out := e.must(http.StatusOK, e.tok["W"], "POST", "/v1/tasks/"+T+"/verify", map[string]any{"result": "PASS", "all_pass": true})
		if out["all_pass"] != true || out["task_state"] != "VERIFIED" {
			t.Fatalf("verify: %s", out["_raw"])
		}
		if status, _ := e.call(e.tok["W"], "POST", "/v1/tasks/"+T+"/evidence", map[string]any{"result": "PASS"}); status != 404 && status != 405 {
			t.Fatalf("evidence write endpoint answered %d", status)
		}
		if out := e.hook("W", "stop", map[string]any{}); out != "" {
			t.Fatalf("stop blocked after PASS: %s", out)
		}
	})

	t.Run("AC-H-043 a change after VERIFIED makes the evidence stale", func(t *testing.T) {
		e.write(e.in("alpha", "src", "ok.txt"), "changed\n")
		e.hook("W", "post-tool", map[string]any{"tool_name": "Edit", "tool_use_id": "tu-3", "tool_input": map[string]any{"file_path": e.in("alpha", "src", "ok.txt")}})
		if taskState() != "RUNNING" {
			t.Fatalf("state %s, want RUNNING", taskState())
		}
	})

	t.Run("AC-H-109 AC-H-041 Bash write outside the scope", func(t *testing.T) {
		e.write(e.in("alpha", "docs", "x.md"), "leak\n")
		for i, id := range []string{"tu-4", "tu-5"} {
			has(t, e.hook("W", "post-tool", map[string]any{"tool_name": "Bash", "tool_use_id": id}), "block", "docs/x.md")
			_ = i
		}
		ev := e.must(http.StatusOK, o, "GET", "/v1/projects/"+e.id["alpha"]+"/events?limit=500", nil)
		if n := countType(ev, "SCOPE_VIOLATION"); n != 1 {
			t.Fatalf("SCOPE_VIOLATION journaled %d times, want 1", n)
		}
		out := e.must(http.StatusOK, e.tok["W"], "POST", "/v1/tasks/"+T+"/verify", nil)
		if out["all_pass"] != false || out["task_state"] != "RUNNING" {
			t.Fatalf("verify passed with a scope violation: %s", out["_raw"])
		}
		if err := os.Remove(e.in("alpha", "docs", "x.md")); err != nil {
			t.Fatal(err)
		}
		if out := e.hook("W", "post-tool", map[string]any{"tool_name": "Bash", "tool_use_id": "tu-6"}); out != "" {
			t.Fatalf("still blocked after revert: %s", out)
		}
	})
}

func countType(events map[string]any, typ string) int {
	n := 0
	list, _ := events["events"].([]any)
	for _, raw := range list {
		if m, ok := raw.(map[string]any); ok && m["type"] == typ {
			n++
		}
	}
	return n
}
