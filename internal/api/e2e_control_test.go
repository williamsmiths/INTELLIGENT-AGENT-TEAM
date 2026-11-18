package api_test

import (
	"net/http"
	"strings"
	"testing"
)

// Feedback, budget, idempotency and isolation over HTTP.
func TestControlAndIsolation(t *testing.T) {
	e := newEnv(t)
	e.seed()
	o, T, U := e.owner, e.id["T"], e.id["U"]
	e.must(http.StatusOK, o, "POST", "/v1/tasks/"+T+"/assign", map[string]any{"principal": e.id["W"]})
	e.must(http.StatusOK, o, "POST", "/v1/tasks/"+U+"/assign", map[string]any{"principal": e.id["V"]})
	e.hook("W", "prompt-submit", nil)
	e.hook("V", "prompt-submit", nil)
	editBeta := func() string {
		return e.hook("V", "pre-tool", map[string]any{"tool_name": "Edit", "tool_input": map[string]any{"file_path": e.in("beta", "src", "b.go")}})
	}
	revision := func(id string) float64 {
		return e.must(http.StatusOK, o, "GET", "/v1/tasks/"+id, nil)["task"].(map[string]any)["revision"].(float64)
	}

	t.Run("AC-H-050 AC-H-052 AC-H-054 AC-H-056 feedback mid-run", func(t *testing.T) {
		sentBefore := len(e.host.sent)
		fb := map[string]any{"text": "Nút thêm phải nằm ngay trong Part, các nơi khác giữ nguyên."}
		e.must(http.StatusForbidden, e.tok["W"], "POST", "/v1/tasks/"+T+"/feedback", fb)
		e.must(http.StatusCreated, o, "POST", "/v1/tasks/"+T+"/feedback", fb, "Idempotency-Key", "fb-1")
		e.must(http.StatusOK, o, "POST", "/v1/tasks/"+T+"/feedback", fb, "Idempotency-Key", "fb-1") // replay
		e.must(http.StatusUnprocessableEntity, o, "POST", "/v1/tasks/"+T+"/feedback", map[string]any{"text": "khác"}, "Idempotency-Key", "fb-1")
		if revision(T) != 2 || revision(U) != 1 {
			t.Fatalf("revisions T=%v U=%v, want 2 and 1", revision(T), revision(U))
		}
		if len(e.host.sent) != sentBefore+1 || !strings.Contains(e.host.sent[len(e.host.sent)-1], "agent-W") {
			t.Fatalf("feedback delivery: %v", e.host.sent[sentBefore:])
		}
		// The next write is refused once and carries the owner's words; then work continues.
		has(t, e.edit("W", e.in("alpha", "src", "a.go")), "deny", "Nút thêm phải nằm ngay trong Part", "rev 2")
		if out := e.edit("W", e.in("alpha", "src", "a.go")); out != "" {
			t.Fatalf("second write still refused: %s", out)
		}
		// The independent task in the other project never noticed.
		if out := editBeta(); out != "" {
			t.Fatalf("project beta was affected: %s", out)
		}
		has(t, e.hook("W", "prompt-submit", nil), "ALPHA-RULE")
	})

	t.Run("AC-H-053 AC-H-106 superseding a pinned requirement refreshes only its tasks", func(t *testing.T) {
		hits := e.must(http.StatusOK, o, "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q=REQ-1", nil)["hits"].([]any)
		req := hits[0].(map[string]any)["Item"].(map[string]any)["ID"].(string)
		e.must(http.StatusConflict, o, "POST", "/v1/knowledge/"+req+"/supersede", map[string]any{"expected_version": 1,
			"aspect": "TO_BE", "trust": "CANDIDATE", "basis": "proposed", "scope": "feature:editor", "title": "x", "body": "y"})
		e.must(http.StatusCreated, o, "POST", "/v1/knowledge/"+req+"/supersede", map[string]any{"expected_version": 1,
			"aspect": "TO_BE", "trust": "AUTHORITATIVE", "scope": "feature:editor", "title": "Xóa nhóm câu hỏi", "body": "ALPHA-RULE-V2: xóa phải xác nhận hai lần."})
		has(t, e.edit("W", e.in("alpha", "src", "a.go")), "deny", "REQ-1")
		has(t, e.hook("W", "prompt-submit", nil), "ALPHA-RULE-V2")
		if out := editBeta(); out != "" || revision(U) != 1 {
			t.Fatalf("beta changed: %s rev %v", out, revision(U))
		}
	})

	t.Run("AC-H-015 AC-H-034 AC-H-082 budget exhaustion stays inside the task", func(t *testing.T) {
		e.hook("W", "post-tool", map[string]any{"tool_name": "Read", "tool_use_id": "b-1"})
		zero := int64(1)
		limits := map[string]any{"max_cost_microusd": 1_000_000, "max_tool_calls": zero}
		e.must(http.StatusForbidden, e.tok["W"], "PUT", "/v1/tasks/"+T+"/budget", limits)
		e.must(http.StatusForbidden, e.tok["L"], "PUT", "/v1/tasks/"+T+"/budget", limits)
		e.must(http.StatusOK, o, "PUT", "/v1/tasks/"+T+"/budget", limits)
		has(t, e.hook("W", "pre-tool", map[string]any{"tool_name": "Bash"}), "deny", "ngân sách")
		has(t, e.edit("W", e.in("alpha", "src", "a.go")), "deny", "ngân sách")
		if out := editBeta(); out != "" {
			t.Fatalf("beta blocked by alpha's budget: %s", out)
		}
		if st := str(e.must(http.StatusOK, o, "GET", "/v1/tasks/"+T, nil), "task", "state"); st != "WAITING_DECISION" {
			t.Fatalf("state %s, want WAITING_DECISION", st)
		}
		e.must(http.StatusOK, o, "PUT", "/v1/tasks/"+T+"/budget", map[string]any{"max_cost_microusd": 1_000_000, "max_tool_calls": 500})
		if st := str(e.must(http.StatusOK, o, "GET", "/v1/tasks/"+T, nil), "task", "state"); st != "RUNNING" {
			t.Fatalf("state %s, want RUNNING after the owner raised the limit", st)
		}
		if out := e.hook("W", "pre-tool", map[string]any{"tool_name": "Bash"}); out != "" {
			t.Fatalf("still blocked: %s", out)
		}
	})

	t.Run("AC-H-080 AC-H-082 AC-H-004 nothing crosses projects", func(t *testing.T) {
		a, b := e.id["alpha"], e.id["beta"]
		for _, path := range []string{"/v1/projects/" + b, "/v1/projects/" + b + "/knowledge?q=REQ-1", "/v1/projects/" + b + "/blackboard",
			"/v1/projects/" + b + "/events", "/v1/tasks/" + U, "/v1/tasks/" + U + "/context", "/v1/tasks/" + U + "/evidence", "/v1/tasks/" + U + "/budget"} {
			e.must(http.StatusNotFound, e.tok["W"], "GET", path, nil)
		}
		e.must(http.StatusNotFound, e.tok["W"], "POST", "/v1/projects/"+b+"/knowledge", map[string]any{"key": "X", "kind": "rule", "aspect": "TO_BE", "trust": "CANDIDATE", "basis": "inferred", "title": "t"})
		raw := e.must(http.StatusOK, e.tok["W"], "GET", "/v1/projects/"+a+"/knowledge?q=xoa+nhom+cau+hoi", nil)["_raw"].(string)
		has(t, raw, "ALPHA-RULE")
		lacks(t, raw, "BETA-RULE")
		lacks(t, e.must(http.StatusOK, o, "GET", "/v1/projects/"+b+"/events?limit=500", nil)["_raw"].(string), T)
		e.must(http.StatusUnauthorized, "", "GET", "/v1/me", nil)
	})

	t.Run("AC-H-083 an agent stays in the project of its lease", func(t *testing.T) {
		e.must(http.StatusOK, e.tok["Z"], "GET", "/v1/projects/"+e.id["beta"]+"/knowledge?q=REQ-1", nil)
		e.must(http.StatusOK, o, "POST", "/v1/tasks/"+U+"/revoke", nil)
		e.must(http.StatusOK, o, "POST", "/v1/tasks/"+U+"/assign", map[string]any{"principal": e.id["Z"]})
		e.must(http.StatusConflict, e.tok["Z"], "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q=REQ-1", nil)
		me := e.must(http.StatusOK, e.tok["Z"], "GET", "/v1/me", nil)
		if str(me, "lease", "project_id") != e.id["beta"] {
			t.Fatalf("me: %s", me["_raw"])
		}
		out := e.hook("Z", "prompt-submit", nil)
		has(t, out, "BETA-RULE")
		lacks(t, out, "ALPHA-RULE")
	})

	t.Run("owner-only endpoints refuse agents", func(t *testing.T) {
		for _, c := range [][2]string{{"POST", "/v1/projects"}, {"PUT", "/v1/projects/" + e.id["alpha"] + "/limits"},
			{"POST", "/v1/projects/" + e.id["alpha"] + "/repos"}, {"POST", "/v1/principals"},
			{"POST", "/v1/principals/" + e.id["W"] + "/memberships"}, {"POST", "/v1/principals/" + e.id["W"] + "/hook-install"},
			{"PUT", "/v1/system/budget"}, {"GET", "/v1/system/events"}, {"POST", "/v1/tasks/" + T + "/revoke"},
			{"POST", "/v1/tasks/" + T + "/feedback"}, {"PUT", "/v1/tasks/" + T + "/budget"}} {
			for _, who := range []string{"W", "L"} {
				e.must(http.StatusForbidden, e.tok[who], c[0], c[1], map[string]any{})
			}
		}
	})
}
