package api_test

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// Spec S2 part C over HTTP with real PostgreSQL, real git repositories and the
// real runner. Only the host's messaging is the fake notifier of e2e_setup_test.go.

// agentP creates a HOOK agent with a registered install and, optionally, a specialty.
func (e *env) agentP(name, project, role, specialty string) {
	e.t.Helper()
	out := e.must(http.StatusCreated, e.owner, "POST", "/v1/principals", map[string]any{"name": name, "openbot_agent_id": "agent-" + name,
		"enforcement": "HOOK", "project_id": e.id[project], "role": role})
	e.id[name], e.tok[name] = str(out, "principal", "id"), str(out, "token")
	e.sum[name] = fmt.Sprintf("%064x", 1000+len(e.sum))
	e.must(http.StatusOK, e.owner, "POST", "/v1/principals/"+e.id[name]+"/hook-install", map[string]any{
		"settings_path": filepath.Join(filepath.Dir(e.tmp), "agents", "agent-"+name, ".claude", "settings.json"), "sha256": e.sum[name]})
	if specialty != "" {
		e.must(http.StatusOK, e.owner, "PUT", "/v1/principals/"+e.id[name], map[string]any{"specialty": specialty})
	}
}

// repoP creates and registers one more repo root of a project.
func (e *env) repoP(project, name string) string {
	e.t.Helper()
	dir := e.gitRepo(filepath.Join(filepath.Dir(e.tmp), "repo-"+name))
	e.repo[name] = str(e.must(http.StatusCreated, e.owner, "POST", "/v1/projects/"+e.id[project]+"/repos", map[string]any{"path": dir}), "root")
	return e.repo[name]
}

// know creates a knowledge item as the owner and returns its id.
func (e *env) know(project, key, kind, trust, scope string) string {
	e.t.Helper()
	body := map[string]any{"key": key, "kind": kind, "aspect": "TO_BE", "trust": trust, "scope": scope, "title": "Tiêu đề " + key, "body": "Nội dung " + key}
	if trust == "CANDIDATE" {
		body["basis"] = "unknown"
	}
	return str(e.must(http.StatusCreated, e.owner, "POST", "/v1/projects/"+e.id[project]+"/knowledge", body), "ID")
}

func (e *env) relate(project, from, to string) {
	e.t.Helper()
	e.must(http.StatusCreated, e.owner, "POST", "/v1/projects/"+e.id[project]+"/relations", map[string]any{"fromkey": from, "tokey": to, "type": "depends_on"})
}

// taskP creates a RUNNABLE task of the owner in its own new repo.
func (e *env) taskP(project, key, scope, check string, reqs ...string) string {
	e.t.Helper()
	if check == "" {
		check = "test -f src/ok.txt"
	}
	repo := e.repoP(project, strings.ToLower(key))
	t := e.must(http.StatusCreated, e.owner, "POST", "/v1/projects/"+e.id[project]+"/tasks", map[string]any{"key": key, "title": "Việc " + key,
		"desired_outcome": "Hoàn thành " + key, "scope": scope, "repo_path": repo, "allowed_paths": []string{"src/**"},
		"requirement_keys": reqs, "required_checks": []map[string]any{{"id": "unit", "command": check}}})
	id := str(t, "id")
	e.id[key] = id
	e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+id+"/ready", nil)
	return id
}

// give assigns a task (owner) with extra fields such as intel_mode and returns the lease.
func (e *env) give(task, agent string, extra map[string]any) map[string]any {
	e.t.Helper()
	// The mechanism tests switch every feature on, the gate included: since Task 2 the default
	// keeps the gate for DEEP only (TestAC_S2_040_GateDefaultDeepOnly checks the default).
	body := map[string]any{"principal": e.id[agent], "intel_features": []string{"gate", "questions", "consult", "convergence", "lesson"}}
	for k, v := range extra {
		body[k] = v
	}
	e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+task+"/assign", body)
	return e.leaseOf(task)
}

// AC-S2-040 (spec S2 C.1, Task 2): without intel_features the understanding gate is on only in DEEP.
func TestAC_S2_040_GateDefaultDeepOnly(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("A1", "alpha", "worker", "")
	for _, tc := range []struct{ mode, gate string }{{"ASSISTED", "off"}, {"DEEP", "on"}, {"DIRECT", "off"}} {
		task := e.taskP("alpha", "GD-"+tc.mode, "feature:gd-"+strings.ToLower(tc.mode), "")
		e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+task+"/assign", map[string]any{"principal": e.id["A1"], "intel_mode": tc.mode})
		l := e.leaseOf(task)
		has := strings.Contains(fmt.Sprint(l["intel_features"]), "gate")
		if (tc.gate == "on") != has {
			t.Fatalf("%s: intel_features %v, gate want %s", tc.mode, l["intel_features"], tc.gate)
		}
		e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+task+"/supersede", nil)
	}
	// An explicit "gate" holds in every mode, and survives a re-grant after feedback.
	task := e.taskP("alpha", "GD-EXPLICIT", "feature:gd-explicit", "")
	e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+task+"/assign", map[string]any{"principal": e.id["A1"], "intel_mode": "DIRECT",
		"intel_features": []string{"gate", "questions"}})
	e.must(http.StatusCreated, e.owner, "POST", "/v1/tasks/"+task+"/feedback", map[string]any{"text": "đổi một chút"})
	if f := fmt.Sprint(e.leaseOf(task)["intel_features"]); f != "[gate questions]" {
		t.Fatalf("explicit features after re-grant: %s", f)
	}
}

func (e *env) leaseOf(task string) map[string]any {
	l, _ := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+task, nil)["lease"].(map[string]any)
	return l
}

func (e *env) stateOf(task string) string {
	return str(e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+task, nil), "task", "state")
}

// verb runs an agent command the way the pre-tool hook forwards it.
func (e *env) verb(agent, verb string, body any) (int, map[string]any) {
	return e.call(e.tok[agent], "POST", "/v1/agent/"+verb, body)
}

func (e *env) kch(agent, verb string, args ...string) (int, map[string]any) {
	return e.verb(agent, verb, map[string]any{"args": args})
}

// writeIn is a pre-tool Write of an agent into a repo.
func (e *env) writeIn(agent, repo, rel string) string {
	return e.hook(agent, "pre-tool", map[string]any{"tool_name": "Write", "cwd": e.repo[repo],
		"tool_input": map[string]any{"file_path": filepath.Join(e.repo[repo], rel)}})
}

// sqlAs runs fn under a scope; admin runs SQL as the database owner (clock tricks only).
func (e *env) sqlAs(sc store.Scope, fn func(tx pgx.Tx) error) {
	e.t.Helper()
	if err := store.InScope(context.Background(), e.pool, sc, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) admin(sql string, args ...any) {
	e.sqlAs(store.Runtime, func(tx pgx.Tx) error {
		if _, err := tx.Exec(context.Background(), "SET LOCAL ROLE NONE"); err != nil {
			return err
		}
		_, err := tx.Exec(context.Background(), sql, args...)
		return err
	})
}

func (e *env) count(sc store.Scope, sql string, args ...any) int {
	var n int
	e.sqlAs(sc, func(tx pgx.Tx) error { return tx.QueryRow(context.Background(), sql, args...).Scan(&n) })
	return n
}

func (e *env) scopeOf(agent string) store.Scope {
	p, err := authn.Authenticate(context.Background(), e.pool, e.tok[agent])
	if err != nil {
		e.t.Fatal(err)
	}
	return p.Scope()
}

// gated seeds a task whose router signals give ASSISTED: one pinned
// requirement, one related item, one CANDIDATE of its scope (F=1, U=1).
func (e *env) gated(key string) string {
	e.t.Helper()
	scope := "feature:" + strings.ToLower(key)
	e.know("alpha", "REQ-"+key, "requirement", "AUTHORITATIVE", scope)
	e.know("alpha", "REL-"+key, "rule", "AUTHORITATIVE", "feature:other")
	e.relate("alpha", "REQ-"+key, "REL-"+key)
	e.know("alpha", "CAND-"+key, "ui", "CANDIDATE", scope)
	return e.taskP("alpha", key, scope, "", "REQ-"+key)
}

// complete is an understanding that addresses everything gated() creates.
func complete(key string) map[string]any {
	return map[string]any{"goal": "Làm " + key, "preserve": []string{"REQ-" + key},
		"not_applicable": []map[string]any{{"key": "REL-" + key, "reason": "quy tắc này thuộc màn hình khác, không đổi"}},
		"assumptions":    []map[string]any{{"text": "CAND-" + key + " giữ như hiện tại", "basis": "inferred", "evidence": "màn hình hiện tại"}}}
}

func TestAC_S2_030_Router(t *testing.T) {
	e := newEnv(t)
	e.seed()
	for _, n := range []string{"A1", "A2", "A3"} {
		e.agentP(n, "alpha", "worker", "")
	}
	e.know("alpha", "REQ-R1", "requirement", "AUTHORITATIVE", "feature:r1")
	e.know("alpha", "REQ-R2", "requirement", "AUTHORITATIVE", "feature:r2")
	e.know("alpha", "R-A", "rule", "AUTHORITATIVE", "feature:other")
	e.know("alpha", "R-B", "api", "AUTHORITATIVE", "feature:other")
	e.relate("alpha", "REQ-R2", "R-A")
	e.relate("alpha", "R-B", "REQ-R2")
	e.know("alpha", "R2-C", "ui", "CANDIDATE", "feature:r2")
	for i := range 3 {
		e.know("alpha", fmt.Sprintf("R3-C%d", i), "ui", "CANDIDATE", "feature:r3")
	}
	r1, r2, r3 := e.taskP("alpha", "R1", "feature:r1", "", "REQ-R1"), e.taskP("alpha", "R2", "feature:r2", "", "REQ-R2"), e.taskP("alpha", "R3", "feature:r3", "")
	mode := func(l map[string]any) string { return fmt.Sprint(l["intel_mode"]) }
	t.Run("AC-S2-030 one pinned requirement, no relation, no CANDIDATE → DIRECT", func(t *testing.T) {
		l := e.give(r1, "A1", nil)
		if mode(l) != "DIRECT" || fmt.Sprint(l["intel_signals"].(map[string]any)["score"]) != "0" {
			t.Fatalf("lease %v", l)
		}
	})
	t.Run("AC-S2-030 two related items and one CANDIDATE → ASSISTED", func(t *testing.T) {
		l := e.give(r2, "A2", nil)
		sig := l["intel_signals"].(map[string]any)
		if mode(l) != "ASSISTED" || sig["F"] != 2.0 || sig["U"] != 1.0 || sig["B"] != 0.0 {
			t.Fatalf("lease %v", l)
		}
	})
	t.Run("AC-S2-030 brief only with three CANDIDATE → DEEP", func(t *testing.T) {
		l := e.give(r3, "A3", nil)
		if sig := l["intel_signals"].(map[string]any); mode(l) != "DEEP" || sig["B"] != 2.0 || sig["U"] != 3.0 {
			t.Fatalf("lease %v", l)
		}
	})
	t.Run("AC-S2-030 the owner forces the mode at assign; a lead cannot", func(t *testing.T) {
		e.must(http.StatusOK, e.tok["A1"], "POST", "/v1/tasks/"+r1+"/release", nil)
		e.must(http.StatusForbidden, e.tok["L"], "POST", "/v1/tasks/"+r1+"/assign", map[string]any{"principal": e.id["A1"], "intel_mode": "DEEP"})
		e.must(http.StatusUnprocessableEntity, e.owner, "POST", "/v1/tasks/"+r1+"/assign", map[string]any{"principal": e.id["A1"], "intel_mode": "HUGE"})
		l := e.give(r1, "A1", map[string]any{"intel_mode": "DEEP"})
		if mode(l) != "DEEP" || l["intel_forced"] != true || l["intel_signals"].(map[string]any)["computed"] != "DIRECT" {
			t.Fatalf("lease %v", l)
		}
		e.must(http.StatusOK, e.tok["A1"], "POST", "/v1/tasks/"+r1+"/release", nil)
	})
	t.Run("AC-S2-030 a requirement change that changes the signals routes the new lease again", func(t *testing.T) {
		before := e.give(r1, "A1", nil)
		e.relate("alpha", "REQ-R1", "R-A")
		e.relate("alpha", "REQ-R1", "R-B")
		e.know("alpha", "R1-C", "ui", "CANDIDATE", "feature:r1")
		hits := e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q=REQ-R1", nil)["hits"].([]any)
		req := hits[0].(map[string]any)["Item"].(map[string]any)["ID"].(string)
		e.must(http.StatusCreated, e.owner, "POST", "/v1/knowledge/"+req+"/supersede", map[string]any{"expected_version": 1,
			"aspect": "TO_BE", "trust": "AUTHORITATIVE", "scope": "feature:r1", "title": "REQ-R1 v2", "body": "mới"})
		after := e.leaseOf(r1)
		if mode(before) != "DIRECT" || mode(after) != "ASSISTED" || after["epoch"] == before["epoch"] {
			t.Fatalf("before %v after %v", before, after)
		}
	})
}

func TestAC_S2_031_UnderstandingGate(t *testing.T) {
	e := newEnv(t)
	e.seed()
	for _, n := range []string{"G1", "G2", "G3", "G4", "G5"} {
		e.agentP(n, "alpha", "worker", "")
	}
	g := e.gated("G")
	l := e.give(g, "G1", nil)
	if l["intel_mode"] != "ASSISTED" || l["intel_phase"] != "DISCOVERY" {
		t.Fatalf("lease %v", l)
	}
	has(t, e.hook("G1", "prompt-submit", nil), "kch understand")
	t.Run("AC-S2-031 a FULL ASSISTED write before the understanding is denied with the items to address", func(t *testing.T) {
		out := e.writeIn("G1", "g", "src/a.go")
		has(t, out, `"permissionDecision":"deny"`, "REQ-G (Tiêu đề REQ-G)", "REL-G (Tiêu đề REL-G)", "CAND-G (Tiêu đề CAND-G)", "kch understand \\u003c\\u003c'END'")
		lacks(t, out, "BETA")
	})
	t.Run("AC-S2-031 a Bash write before the understanding is blocked after the call; verify answers 409", func(t *testing.T) {
		e.write(filepath.Join(e.repo["g"], "src", "x.txt"), "x\n")
		has(t, e.bash("G1", "g-bash-1"), `"decision":"block"`, "trước khi bản hiểu được nhận")
		if status, out := e.kch("G1", "verify"); status != http.StatusConflict {
			t.Fatalf("verify before the understanding: %d %s", status, out["_raw"])
		}
		if err := os.Remove(filepath.Join(e.repo["g"], "src", "x.txt")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AC-S2-031 a submission missing a related item is 422 with key and title; a complete one is 201", func(t *testing.T) {
		partial := complete("G")
		delete(partial, "not_applicable")
		status, out := e.verb("G1", "understand", partial)
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("partial: %d %s", status, out["_raw"])
		}
		has(t, out["text"].(string), "REL-G (Tiêu đề REL-G)", "kch understand")
		status, out = e.verb("G1", "understand", complete("G"))
		if status != http.StatusCreated || out["status"] != "ACCEPTED" {
			t.Fatalf("complete: %d %s", status, out["_raw"])
		}
		if res := e.writeIn("G1", "g", "src/a.go"); res != "" {
			t.Fatalf("write after the understanding: %s", res)
		}
		if status, out := e.kch("G1", "verify"); status != http.StatusOK {
			t.Fatalf("verify after the understanding: %d %s", status, out["_raw"])
		}
		if ph := e.leaseOf(g)["intel_phase"]; ph == "DISCOVERY" {
			t.Fatalf("phase still %v", ph)
		}
	})
	t.Run("AC-S2-031 after two rejections the third submission is taken with unaddressed and UNDERSTANDING_FORCED", func(t *testing.T) {
		g2 := e.gated("GF")
		e.give(g2, "G2", nil)
		e.hook("G2", "prompt-submit", nil)
		for range 2 {
			if status, _ := e.verb("G2", "understand", map[string]any{"goal": "x"}); status != http.StatusUnprocessableEntity {
				t.Fatalf("rejection: %d", status)
			}
		}
		status, out := e.verb("G2", "understand", map[string]any{"goal": "x"})
		if status != http.StatusCreated || out["status"] != "FORCED" || len(out["unaddressed"].([]any)) != 3 {
			t.Fatalf("third: %d %s", status, out["_raw"])
		}
		if n := e.events("alpha", "UNDERSTANDING_FORCED"); n != 1 {
			t.Fatalf("UNDERSTANDING_FORCED %d", n)
		}
		rows := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+g2+"/understanding", nil)["understanding"].([]any)
		if len(rows) != 3 {
			t.Fatalf("stored submissions: %d", len(rows))
		}
		if res := e.writeIn("G2", "gf", "src/a.go"); res != "" {
			t.Fatalf("write after a forced understanding: %s", res)
		}
	})
	t.Run("AC-S2-031 DIRECT, CONTEXT and OBSERVE have no gate; a switched-off gate neither blocks nor nags", func(t *testing.T) {
		for agent, extra := range map[string]map[string]any{"G3": {"mode": "CONTEXT"}, "G4": {"mode": "OBSERVE"}, "G5": {"intel_mode": "DIRECT"}} {
			task := e.gated("N" + agent)
			e.give(task, agent, extra)
			e.hook(agent, "prompt-submit", nil)
			if res := e.writeIn(agent, "n"+strings.ToLower(agent), "src/a.go"); res != "" {
				t.Fatalf("%s %v: %s", agent, extra, res)
			}
		}
		off := e.gated("OFF")
		e.must(http.StatusOK, e.tok["G5"], "POST", "/v1/tasks/"+e.id["NG5"]+"/release", nil)
		l := e.give(off, "G5", map[string]any{"intel_features": []string{}})
		lacks(t, e.hook("G5", "prompt-submit", nil), "kch understand", "kch consult")
		if res := e.writeIn("G5", "off", "src/a.go"); res != "" || l["intel_mode"] != "ASSISTED" || l["intel_phase"] != "BUILD" {
			t.Fatalf("gate off: %s %v", res, l)
		}
		if status, _ := e.kch("G5", "verify"); status != http.StatusOK {
			t.Fatalf("verify with the gate off: %d", status)
		}
		status, _ := e.verb("G5", "ask", map[string]any{"text": "Màu?", "options": []string{"xanh", "đỏ"}, "default": "xanh", "impact": "low", "blocking": true})
		if status != http.StatusCreated || e.stateOf(off) != "RUNNING" {
			t.Fatalf("questions off: %d %s", status, e.stateOf(off))
		}
		if status, _ := e.kch("G5", "consult", "product", "câu hỏi"); status != http.StatusConflict {
			t.Fatalf("consult off: %d", status)
		}
		for range 3 {
			_, out := e.kch("G5", "verify")
			if out["convergence"] != nil {
				t.Fatalf("convergence off: %s", out["_raw"])
			}
		}
		if e.stateOf(off) != "RUNNING" {
			t.Fatal("convergence off parked the task")
		}
		lacks(t, e.hook("G5", "stop", map[string]any{"stop_hook_active": false}), "Nêu cho chủ sản phẩm", "kch lesson")
	})
}

func TestAC_S2_032_AssumptionsAndQuestions(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("Q1", "alpha", "worker", "")
	q := e.gated("Q")
	e.give(q, "Q1", nil)
	e.hook("Q1", "prompt-submit", nil)
	ask := func(blocking bool, text string) string {
		t.Helper()
		status, out := e.verb("Q1", "ask", map[string]any{"text": text, "options": []string{"trong Part", "ngoài Part"},
			"default": "trong Part", "impact": "medium", "blocking": blocking})
		if status != http.StatusCreated {
			t.Fatalf("ask: %d %s", status, out["_raw"])
		}
		return str(out, "id")
	}
	answer := func(id, option string) map[string]any {
		return e.must(http.StatusOK, e.owner, "POST", "/v1/questions/"+id+"/answer", map[string]any{"option": option})
	}
	t.Run("AC-S2-032 two assumptions and one non-blocking question: 2 CANDIDATE with their basis, 1 OPEN question, task RUNNING", func(t *testing.T) {
		u := complete("Q")
		u["assumptions"] = []map[string]any{{"text": "CAND-Q giữ màu cũ", "basis": "inferred", "evidence": "ảnh"}, {"text": "Nút đặt cuối Part", "basis": "proposed"}}
		u["questions"] = []map[string]any{{"text": "Thứ tự?", "options": []string{"cuối", "đầu"}, "default": "cuối", "impact": "low", "blocking": false}}
		if status, out := e.verb("Q1", "understand", u); status != http.StatusCreated {
			t.Fatalf("understand: %d %s", status, out["_raw"])
		}
		for key, basis := range map[string]string{"ASM-Q-1": "inferred", "ASM-Q-2": "proposed"} {
			hit := e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q="+key, nil)["hits"].([]any)[0].(map[string]any)["Item"].(map[string]any)
			if hit["Key"] != key || hit["Trust"] != "CANDIDATE" || hit["Basis"] != basis || hit["Kind"] != "decision" || hit["Scope"] != "feature:q" {
				t.Fatalf("%s: %v", key, hit)
			}
		}
		qs := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+q+"/questions", nil)["questions"].([]any)
		if len(qs) != 1 || qs[0].(map[string]any)["status"] != "OPEN" || e.stateOf(q) != "RUNNING" {
			t.Fatalf("questions %v state %s", qs, e.stateOf(q))
		}
	})
	t.Run("AC-S2-032 C.2 stop blocks once per turn with evidence, open questions and assumptions in one message", func(t *testing.T) {
		e.write(filepath.Join(e.repo["q"], "src", "a.go"), "package a\n")
		e.hook("Q1", "post-tool", map[string]any{"tool_name": "Write", "tool_use_id": "q-w1", "tool_input": map[string]any{"file_path": filepath.Join(e.repo["q"], "src", "a.go")}})
		out := e.hook("Q1", "stop", map[string]any{"stop_hook_active": false})
		if strings.Count(out, `"decision":"block"`) != 1 {
			t.Fatalf("stop: %s", out)
		}
		has(t, out, "kch verify", "câu hỏi còn mở", "#1 Thứ tự?", "giả định chưa được xác nhận", "ASM-Q-1", "ASM-Q-2")
		if out := e.hook("Q1", "stop", map[string]any{"stop_hook_active": true}); out != "" {
			t.Fatalf("second stop of the turn: %s", out)
		}
		next := e.hook("Q1", "stop", map[string]any{"stop_hook_active": false}) // next turn: what was put to the owner is not repeated
		lacks(t, next, "ASM-Q-1", "#1 Thứ tự?")
		has(t, next, "kch verify")
		if err := os.Remove(filepath.Join(e.repo["q"], "src", "a.go")); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AC-S2-032 a blocking question parks the task and denies writes; the owner's default answer confirms only", func(t *testing.T) {
		id := ask(true, "Nút thêm ở đâu?")
		if e.stateOf(q) != "WAITING_DECISION" {
			t.Fatalf("state %s", e.stateOf(q))
		}
		has(t, e.writeIn("Q1", "q", "src/a.go"), `"permissionDecision":"deny"`, "câu hỏi blocking")
		open := e.must(http.StatusOK, e.owner, "GET", "/v1/questions?status=OPEN", nil)["_raw"].(string)
		has(t, open, id)
		e.must(http.StatusForbidden, e.tok["Q1"], "POST", "/v1/questions/"+id+"/answer", map[string]any{"option": "trong Part"}) // a worker
		e.must(http.StatusForbidden, e.tok["L"], "POST", "/v1/questions/"+id+"/answer", map[string]any{"option": "trong Part"})  // a lead
		before := e.leaseOf(q)
		out := answer(id, "trong Part")
		after := e.leaseOf(q)
		if out["differs"] != false || e.stateOf(q) != "RUNNING" || after["epoch"] != before["epoch"] {
			t.Fatalf("answer %s; lease %v → %v", out["_raw"], before["epoch"], after["epoch"])
		}
		dec := e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q=DEC-Q-Q2", nil)["hits"].([]any)[0].(map[string]any)["Item"].(map[string]any)
		if dec["Trust"] != "AUTHORITATIVE" || dec["Kind"] != "decision" {
			t.Fatalf("decision %v", dec)
		}
		if res := e.writeIn("Q1", "q", "src/a.go"); res != "" {
			t.Fatalf("write after the answer: %s", res)
		}
		e.must(http.StatusConflict, e.owner, "POST", "/v1/questions/"+id+"/answer", map[string]any{"option": "trong Part"})
	})
	t.Run("AC-S2-032 an answer other than the default is a feedback: revision + 1, lease replaced with the notice", func(t *testing.T) {
		id := ask(true, "Nút thêm có icon không?")
		before := e.leaseOf(q)
		out := answer(id, "ngoài Part")
		after := e.leaseOf(q)
		if out["differs"] != true || e.stateOf(q) != "RUNNING" || after["epoch"].(float64) != before["epoch"].(float64)+1 ||
			after["task_revision"].(float64) != before["task_revision"].(float64)+1 {
			t.Fatalf("answer %s; lease %v → %v", out["_raw"], before, after)
		}
		has(t, str(after, "pending_notice"), "ngoài Part", "khác mặc định", "Có thể liên quan", "Giữ nguyên")
	})
	t.Run("AC-S2-032 QUESTION_WAITING is journaled once for a blocking question left open", func(t *testing.T) {
		id := ask(true, "Câu hỏi chờ lâu?")
		for range 2 {
			e.sqlAs(store.Runtime, func(tx pgx.Tx) error {
				_, err := intel.JournalWaitingQuestions(context.Background(), tx, 0)
				return err
			})
		}
		if n := e.events("alpha", "QUESTION_WAITING"); n != 1 {
			t.Fatalf("QUESTION_WAITING %d", n)
		}
		// Overlap with the budget: the task leaves WAITING_DECISION only when the set is empty (C.8).
		e.must(http.StatusOK, e.owner, "PUT", "/v1/tasks/"+q+"/budget", map[string]any{"max_cost_microusd": 0})
		answer(id, "trong Part")
		if st := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+q, nil)["task"].(map[string]any); st["state"] != "WAITING_DECISION" ||
			fmt.Sprint(st["waiting_reasons"]) != "[budget]" {
			t.Fatalf("after the answer with the budget exhausted: %v", st)
		}
		e.must(http.StatusOK, e.owner, "PUT", "/v1/tasks/"+q+"/budget", map[string]any{"max_cost_microusd": 1_000_000})
		if e.stateOf(q) != "RUNNING" {
			t.Fatalf("state %s", e.stateOf(q))
		}
	})
	t.Run("AC-S2-032 a lease that expires while the task waits: RUNNABLE once the set is empty", func(t *testing.T) {
		id := ask(true, "Hết hạn lease?")
		e.sqlAs(store.Runtime, func(tx pgx.Tx) error {
			if _, err := tx.Exec(context.Background(), `UPDATE lease SET expires_at = now() - interval '1 minute' WHERE task_id = $1 AND state = 'ACTIVE'`, q); err != nil {
				return err
			}
			_, err := work.ExpireLeases(context.Background(), tx)
			return err
		})
		if e.stateOf(q) != "WAITING_DECISION" {
			t.Fatalf("state %s", e.stateOf(q))
		}
		answer(id, "trong Part")
		if e.stateOf(q) != "RUNNABLE" {
			t.Fatalf("state %s", e.stateOf(q))
		}
	})
}

func TestAC_S2_033_Consult(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("C1", "alpha", "worker", "")
	e.agentP("P", "alpha", "worker", "product")
	e.agentP("D1", "alpha", "worker", "")
	e.agentP("P2", "alpha", "worker", "design")
	// Z (alpha + beta, older than P) has the specialty too but holds a lease in beta: never selected.
	e.must(http.StatusOK, e.owner, "PUT", "/v1/principals/"+e.id["Z"], map[string]any{"specialty": "product"})
	e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+e.id["U"]+"/assign", map[string]any{"principal": e.id["Z"]})
	c := e.gated("C")
	e.give(c, "C1", nil)
	e.hook("C1", "prompt-submit", nil)
	var consult string
	t.Run("AC-S2-033 the holder consults product: 201, one message with references and no conversation", func(t *testing.T) {
		sent := len(e.host.sent)
		status, out := e.kch("C1", "consult", "product", "Nút thêm nên nằm trong hay ngoài Part?", "--refs", "REQ-C,NOPE")
		if status != http.StatusCreated || len(e.host.sent) != sent+1 {
			t.Fatalf("consult: %d %s; sent %v", status, out["_raw"], e.host.sent[sent:])
		}
		consult = str(out, "id")
		msg := e.host.sent[sent]
		has(t, msg, "agent-P: ", "Nút thêm nên nằm trong hay ngoài Part?", "REQ-C (Tiêu đề REQ-C, AUTHORITATIVE)", "DỮ LIỆU CHƯA KIỂM CHỨNG", "kch answer "+consult)
		lacks(t, msg, "agent-Z", "Hoàn thành C", "NOPE")
	})
	t.Run("AC-S2-033 the same question returns the earlier consult without a message; another one is 409", func(t *testing.T) {
		sent := len(e.host.sent)
		status, out := e.kch("C1", "consult", "product", "nút thêm nên nằm trong, hay ngoài Part")
		if status != http.StatusOK || out["duplicate"] != true || str(out, "id") != consult || len(e.host.sent) != sent {
			t.Fatalf("duplicate: %d %s", status, out["_raw"])
		}
		if status, out := e.kch("C1", "consult", "product", "Màu của nút?"); status != http.StatusConflict {
			t.Fatalf("over the allowance: %d %s", status, out["_raw"])
		}
	})
	t.Run("AC-S2-033 a non-holder, or the answerer, cannot consult for the task", func(t *testing.T) {
		for _, who := range []string{"L", "P"} {
			if status, _ := e.kch(who, "consult", "product", "Câu khác?"); status != http.StatusConflict && status != http.StatusForbidden {
				t.Fatalf("%s consulted: %d", who, status)
			}
		}
	})
	t.Run("AC-S2-033 another agent answering is 404; the answerer's answer is a finding the asker gets at its next hook", func(t *testing.T) {
		for _, who := range []string{"W", "Z", "C1"} {
			if status, _ := e.verb(who, "answer", map[string]any{"args": []string{consult}, "stdin": "trả lời hộ"}); status != http.StatusNotFound {
				t.Fatalf("%s answered: %d", who, status)
			}
		}
		sent := len(e.host.sent)
		status, out := e.verb("P", "answer", map[string]any{"args": []string{consult, "--refs", "REQ-C"}, "stdin": "Đặt trong Part, theo REQ-C."})
		if status != http.StatusCreated {
			t.Fatalf("answer: %d %s", status, out["_raw"])
		}
		board := e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id["alpha"]+"/blackboard?task="+c, nil)["_raw"].(string)
		has(t, board, "finding", "Đặt trong Part, theo REQ-C.", e.id["P"])
		if len(e.host.sent) != sent+1 || !strings.HasPrefix(e.host.sent[sent], "agent-C1: ") {
			t.Fatalf("asker not told through the host: %v", e.host.sent[sent:])
		}
		inbox := "Trả lời tham vấn " + consult + " từ P (dữ liệu chưa kiểm chứng"
		has(t, e.hook("C1", "prompt-submit", nil), inbox, "Đặt trong Part, theo REQ-C.")
		lacks(t, e.hook("C1", "prompt-submit", nil), inbox) // delivered once; the finding stays on the blackboard
		if status, _ := e.verb("P", "answer", map[string]any{"args": []string{consult}, "stdin": "lần hai"}); status != http.StatusConflict {
			t.Fatalf("second answer: %d", status)
		}
	})
	t.Run("AC-S2-033 past its deadline a consult is EXPIRED, the asker is told, a late answer is 409", func(t *testing.T) {
		d := e.taskP("alpha", "DD", "feature:dd", "")
		e.give(d, "D1", map[string]any{"intel_mode": "DEEP"})
		e.hook("D1", "prompt-submit", nil)
		ids := []string{}
		for _, question := range []string{"Bố cục lưới?", "Màu nhấn?"} {
			status, out := e.kch("D1", "consult", "design", question)
			if status != http.StatusCreated {
				t.Fatalf("consult: %d %s", status, out["_raw"])
			}
			ids = append(ids, str(out, "id"))
		}
		e.admin(`UPDATE consult SET expires_at = now() - interval '1 second' WHERE id = ANY($1::uuid[])`, ids)
		if status, _ := e.verb("P2", "answer", map[string]any{"args": []string{ids[0]}, "stdin": "muộn"}); status != http.StatusConflict {
			t.Fatalf("late answer: %d", status)
		}
		if err := api.SweepIntel(context.Background(), e.pool, e.host); err != nil {
			t.Fatal(err)
		}
		if n := e.count(store.Runtime, `SELECT count(*) FROM consult WHERE id = ANY($1::uuid[]) AND status = 'EXPIRED'`, ids); n != 2 {
			t.Fatalf("expired %d", n)
		}
		if n := e.events("alpha", "CONSULT_EXPIRED"); n != 2 {
			t.Fatalf("CONSULT_EXPIRED %d", n)
		}
		_, out := e.kch("D1", "status") // any verb carries what waits for the asker
		has(t, str(out, "text"), ids[0], ids[1], "EXPIRED")
	})
}

func TestAC_S2_034_Convergence(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("N1", "alpha", "worker", "")
	e.agentP("N2", "alpha", "worker", "")
	e.agentP("P", "alpha", "worker", "product")
	check := "cat src/msg.txt; test -f src/ok.txt"
	n := e.taskP("alpha", "NP", "feature:np", check)
	e.give(n, "N1", map[string]any{"intel_mode": "DIRECT"}) // DIRECT: no consult until convergence grants one
	e.hook("N1", "prompt-submit", nil)
	msg := func(text string) { e.write(filepath.Join(e.repo["np"], "src", "msg.txt"), text+"\n") }
	verifyOnce := func() map[string]any {
		t.Helper()
		status, out := e.kch("N1", "verify")
		if status != http.StatusOK || out["all_pass"] != false {
			t.Fatalf("verify: %d %s", status, out["_raw"])
		}
		return out
	}
	conv := func(out map[string]any) map[string]any { c, _ := out["convergence"].(map[string]any); return c }
	msg("lỗi A: thiếu ok.txt")
	t.Run("AC-S2-034 two identical failing verifies: STEER, +1 consult once for the task, NONPROGRESS", func(t *testing.T) {
		if c := conv(verifyOnce()); c["status"] != "PROGRESS" || c["repeats"] != 1.0 {
			t.Fatalf("first: %v", c)
		}
		if status, _ := e.kch("N1", "consult", "product", "Trước khi được cấp?"); status != http.StatusConflict {
			t.Fatalf("DIRECT consulted: %d", status)
		}
		out := verifyOnce()
		if c := conv(out); c["status"] != "STEER" || c["bonus_consult"] != true {
			t.Fatalf("second: %v", c)
		}
		has(t, str(out, "text"), "Xét lại", "yêu cầu", "kiến trúc", "kch consult")
		if n := e.events("alpha", "NONPROGRESS"); n != 1 {
			t.Fatalf("NONPROGRESS %d", n)
		}
		has(t, e.hook("N1", "prompt-submit", nil), "Hai lần verify liên tiếp thất bại giống hệt nhau")
		if status, out := e.kch("N1", "consult", "product", "Vì sao ok.txt không có?"); status != http.StatusCreated {
			t.Fatalf("bonus consult: %d %s", status, out["_raw"])
		}
	})
	t.Run("AC-S2-034 a changed error is progress and resets the counter", func(t *testing.T) {
		msg("lỗi B: khác hẳn")
		if c := conv(verifyOnce()); c["status"] != "PROGRESS" || c["repeats"] != 1.0 {
			t.Fatalf("changed fingerprint: %v", c)
		}
		if c := conv(verifyOnce()); c["status"] != "STEER" || c["bonus_consult"] == true {
			t.Fatalf("second steer: %v", c)
		}
		if status, _ := e.kch("N1", "consult", "product", "Câu thứ hai?"); status != http.StatusConflict {
			t.Fatalf("the extra consult was granted twice: %d", status)
		}
	})
	t.Run("AC-S2-034 the third identical verify parks the task; the owner resumes it and the counter is 0", func(t *testing.T) {
		if c := conv(verifyOnce()); c["status"] != "PARKED" {
			t.Fatalf("third: %v", c)
		}
		if st := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+n, nil)["task"].(map[string]any); st["state"] != "WAITING_DECISION" ||
			fmt.Sprint(st["waiting_reasons"]) != "[nonprogress]" {
			t.Fatalf("task %v", st)
		}
		has(t, e.writeIn("N1", "np", "src/ok.txt"), `"permissionDecision":"deny"`, "giống hệt nhau")
		if status, _ := e.kch("N1", "verify"); status != http.StatusConflict {
			t.Fatalf("verify while parked: %d", status)
		}
		e.must(http.StatusForbidden, e.tok["L"], "POST", "/v1/tasks/"+n+"/resume", map[string]any{"note": "tiếp"})
		e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+n+"/resume", map[string]any{"note": "Thử đọc lại REQ trước"})
		if e.stateOf(n) != "RUNNING" {
			t.Fatalf("state %s", e.stateOf(n))
		}
		if c := conv(verifyOnce()); c["repeats"] != 1.0 || c["status"] != "PROGRESS" {
			t.Fatalf("after resume: %v", c)
		}
		e.must(http.StatusConflict, e.owner, "POST", "/v1/tasks/"+n+"/resume", nil)
	})
	t.Run("AC-S2-034 with convergence switched off identical verifies neither steer nor park", func(t *testing.T) {
		off := e.taskP("alpha", "NOFF", "feature:noff", check)
		e.give(off, "N2", map[string]any{"intel_mode": "DIRECT", "intel_features": []string{"gate", "questions", "consult", "lesson"}})
		for range 4 {
			_, out := e.kch("N2", "verify")
			if out["convergence"] != nil || out["attempt"] == nil {
				t.Fatalf("convergence off: %s", out["_raw"])
			}
		}
		if e.stateOf(off) != "RUNNING" {
			t.Fatalf("state %s", e.stateOf(off))
		}
	})
}

func TestAC_S2_035_FeedbackDelta(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("F1", "alpha", "worker", "")
	e.agentP("F2", "alpha", "worker", "")
	f := e.gated("F")
	u := e.gated("FU")
	e.give(f, "F1", nil)
	before := e.give(u, "F2", map[string]any{"intel_mode": "DIRECT"})
	for _, a := range []string{"F1", "F2"} {
		e.hook(a, "prompt-submit", nil)
	}
	if status, out := e.verb("F1", "understand", complete("F")); status != http.StatusCreated {
		t.Fatalf("understand: %d %s", status, out["_raw"])
	}
	if res := e.writeIn("F1", "f", "src/a.go"); res != "" {
		t.Fatalf("write: %s", res)
	}
	t.Run("AC-S2-035 the feedback notice names what may be related and what stays", func(t *testing.T) {
		e.must(http.StatusCreated, e.owner, "POST", "/v1/tasks/"+f+"/feedback", map[string]any{"text": "Quy tắc REL-F phải đổi theo màn hình mới"})
		has(t, str(e.leaseOf(f), "pending_notice"), "Quy tắc REL-F phải đổi", "Có thể liên quan: ", "REL-F (Tiêu đề REL-F)", "Giữ nguyên: REQ-F")
	})
	t.Run("AC-S2-035 writes are denied until a delta covers every pinned key", func(t *testing.T) {
		has(t, e.writeIn("F1", "f", "src/a.go"), `"permissionDecision":"deny"`, "Có thể liên quan") // S1: once with the notice
		has(t, e.writeIn("F1", "f", "src/a.go"), `"permissionDecision":"deny"`, "nộp delta", "REQ-F (Tiêu đề REQ-F)")
		if status, _ := e.verb("F1", "understand", map[string]any{"delta": map[string]any{"changed": []string{"src/a.go"}}}); status != http.StatusUnprocessableEntity {
			t.Fatalf("delta without the pinned key: %d", status)
		}
		has(t, e.writeIn("F1", "f", "src/a.go"), `"permissionDecision":"deny"`)
		status, out := e.verb("F1", "understand", map[string]any{"delta": map[string]any{"changed": []string{"src/a.go"}, "preserved": []string{"REQ-F"}}})
		if status != http.StatusCreated || out["kind"] != "DELTA" {
			t.Fatalf("delta: %d %s", status, out["_raw"])
		}
		if res := e.writeIn("F1", "f", "src/a.go"); res != "" {
			t.Fatalf("write after the delta: %s", res)
		}
	})
	t.Run("AC-S2-035 the other task of the project noticed nothing", func(t *testing.T) {
		after := e.leaseOf(u)
		if after["epoch"] != before["epoch"] || e.writeIn("F2", "fu", "src/a.go") != "" {
			t.Fatalf("U changed: %v → %v", before, after)
		}
	})
	t.Run("AC-S2-035 verify measures the declared changes against the real ones (delta_mismatch)", func(t *testing.T) {
		e.write(filepath.Join(e.repo["f"], "src", "a.go"), "package a\n")
		e.write(filepath.Join(e.repo["f"], "src", "b.go"), "package a\n")
		_, out := e.kch("F1", "verify")
		m, _ := out["delta_mismatch"].(map[string]any)
		if fmt.Sprint(m["undeclared"]) != "[src/b.go]" || fmt.Sprint(m["declared_unchanged"]) != "[]" {
			t.Fatalf("delta_mismatch: %s", out["_raw"])
		}
		if n := e.count(store.Runtime, `SELECT count(*) FROM attempt WHERE task_id = $1 AND delta_mismatch->'undeclared' ? 'src/b.go'`, f); n != 1 {
			t.Fatalf("attempt row: %d", n)
		}
	})
}

func TestAC_S2_036_Experience(t *testing.T) {
	e := newEnv(t)
	e.seed()
	for _, n := range []string{"E1", "E0", "E2", "E3"} {
		e.agentP(n, "alpha", "worker", "")
	}
	ex := e.taskP("alpha", "EX", "feature:editor", "")
	e.give(ex, "E1", map[string]any{"intel_mode": "DIRECT"})
	e.hook("E1", "prompt-submit", nil)
	lesson := map[string]any{"cause": "quên tạo file ok.txt mà check cần", "fix": "tạo src/ok.txt trước khi verify",
		"applies_when": "khi check kiểm tra sự tồn tại của file", "limits": "không áp dụng cho check chạy test thật"}
	var key string
	t.Run("AC-S2-036 FAIL then PASS asks for a lesson; stop blocks once; the lesson is a CANDIDATE experience with a RUNTIME source", func(t *testing.T) {
		e.kch("E1", "verify")
		e.write(filepath.Join(e.repo["ex"], "src", "ok.txt"), "ok\n")
		_, out := e.kch("E1", "verify")
		if out["all_pass"] != true || out["lesson_requested"] == nil {
			t.Fatalf("verify: %s", out["_raw"])
		}
		has(t, str(out, "text"), "kch lesson", "1/2 lần verify FAIL")
		has(t, e.hook("E1", "stop", map[string]any{"stop_hook_active": false}), `"decision":"block"`, "kch lesson")
		if out := e.hook("E1", "stop", map[string]any{"stop_hook_active": true}); out != "" {
			t.Fatalf("stop blocked twice in a turn: %s", out)
		}
		short := map[string]any{"cause": "ngắn", "fix": lesson["fix"], "applies_when": lesson["applies_when"], "limits": lesson["limits"]}
		if status, _ := e.verb("E1", "lesson", short); status != http.StatusUnprocessableEntity {
			t.Fatalf("short lesson: %d", status)
		}
		status, res := e.verb("E1", "lesson", lesson)
		if status != http.StatusCreated {
			t.Fatalf("lesson: %d %s", status, res["_raw"])
		}
		key = str(res, "key")
		item := e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q="+key, nil)["hits"].([]any)[0].(map[string]any)["Item"].(map[string]any)
		if item["Trust"] != "CANDIDATE" || item["Kind"] != "experience" || item["Level"] != "PROJECT" || item["Scope"] != "feature:editor" {
			t.Fatalf("item %v", item)
		}
		src := e.must(http.StatusOK, e.owner, "GET", "/v1/sources/"+item["SourceIDs"].([]any)[0].(string), nil)
		if src["Origin"] != "RUNTIME" || !strings.Contains(str(src, "Content"), `"failed_attempts":1`) {
			t.Fatalf("source %s", src["_raw"])
		}
		if out := e.hook("E1", "stop", map[string]any{"stop_hook_active": false}); out != "" {
			t.Fatalf("stop after the lesson: %s", out)
		}
	})
	t.Run("AC-S2-036 a first-time PASS asks for nothing", func(t *testing.T) {
		e0 := e.taskP("alpha", "EX0", "feature:e0", "true")
		e.give(e0, "E0", map[string]any{"intel_mode": "DIRECT"})
		if _, out := e.kch("E0", "verify"); out["all_pass"] != true || out["lesson_requested"] != nil {
			t.Fatalf("verify: %s", out["_raw"])
		}
	})
	t.Run("AC-S2-036 once confirmed, a new task of the same scope gets the experience; another scope or project does not", func(t *testing.T) {
		item := e.must(http.StatusOK, e.owner, "GET", "/v1/projects/"+e.id["alpha"]+"/knowledge?q="+key, nil)["hits"].([]any)[0].(map[string]any)["Item"].(map[string]any)
		e.must(http.StatusOK, e.owner, "POST", "/v1/knowledge/"+item["ID"].(string)+"/confirm", map[string]any{"expected_version": 1})
		same := e.taskP("alpha", "EX2", "feature:editor", "")
		e.give(same, "E2", map[string]any{"intel_mode": "DIRECT"})
		has(t, e.hook("E2", "prompt-submit", nil), "Kinh nghiệm đã được chủ sản phẩm xác nhận", key, "khi check kiểm tra sự tồn tại")
		other := e.taskP("alpha", "EX3", "feature:khac", "")
		e.give(other, "E3", map[string]any{"intel_mode": "DIRECT"})
		lacks(t, e.hook("E3", "prompt-submit", nil), key)
		e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+e.id["U"]+"/assign", map[string]any{"principal": e.id["V"]}) // beta, same scope string
		lacks(t, e.hook("V", "prompt-submit", nil), key, "Kinh nghiệm đã được")
	})
}

func TestAC_S2_037_CostSplit(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("K1", "alpha", "worker", "")
	e.agentP("P", "alpha", "worker", "product")
	k := e.gated("K")
	own := e.taskP("alpha", "PK", "feature:pk", "")
	e.give(own, "P", map[string]any{"intel_mode": "DIRECT"}) // the answerer works on its own task meanwhile
	e.give(k, "K1", nil)
	e.hook("K1", "prompt-submit", nil)
	usage := func(agent, id string, usd float64, turn string) {
		t.Helper()
		e.sqlAs(store.Runtime, func(tx pgx.Tx) error {
			_, err := work.RecordUsage(context.Background(), tx, work.LedgerEntry{Source: "openbot_usage", ExternalID: "agent-" + agent + "/" + id,
				OpenBotAgentID: "agent-" + agent, Provider: "anthropic", Model: "claude-test", OccurredAt: time.Now(), TurnID: turn,
				Usage: work.Usage{Cost: int64(usd * 1e6), InputTokens: 10, OutputTokens: 1}})
			return err
		})
	}
	usage("K1", "u1", 0.01, "turn-k1") // DISCOVERY: before the understanding
	if status, out := e.verb("K1", "understand", complete("K")); status != http.StatusCreated {
		t.Fatalf("understand: %d %s", status, out["_raw"])
	}
	usage("K1", "u2", 0.02, "turn-k2") // BUILD
	if _, out := e.kch("K1", "verify"); out["all_pass"] != false {
		t.Fatalf("verify: %s", out["_raw"])
	}
	usage("K1", "u3", 0.04, "turn-k3") // REWORK: after the first failing attempt
	status, out := e.kch("K1", "consult", "product", "Nên giữ REQ-K không?")
	if status != http.StatusCreated {
		t.Fatalf("consult: %d %s", status, out["_raw"])
	}
	delivery := fmt.Sprintf("dlv-%d", len(e.host.sent))
	usage("P", "p1", 0.08, "turn-c") // the consult turn, before its id is known: on P's own task for now
	usage("P", "p2", 0.16, "turn-x") // P's unrelated work
	e.host.mu.Lock()
	e.host.turns[delivery] = "turn-c"
	e.host.mu.Unlock()
	if status, out := e.verb("P", "answer", map[string]any{"args": []string{str(out, "id")}, "stdin": "Giữ REQ-K."}); status != http.StatusCreated {
		t.Fatalf("answer: %d %s", status, out["_raw"])
	}
	if err := api.SweepIntel(context.Background(), e.pool, e.host); err != nil { // the worker resolves the turn (REV-005 #4)
		t.Fatal(err)
	}
	usage("P", "p3", 0.32, "turn-c") // after: straight to COLLAB
	t.Run("AC-S2-037 cost by phase, with only the consult turn of the answerer attributed to the asker, once", func(t *testing.T) {
		cost := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+k+"/cost", nil)
		phase := func(p string) float64 {
			v, _ := cost["by_phase"].(map[string]any)[p].(map[string]any)
			f, _ := v["cost_microusd"].(float64)
			return f
		}
		if phase("DISCOVERY") != 10000 || phase("BUILD") != 20000 || phase("REWORK") != 40000 || phase("COLLAB") != 400000 {
			t.Fatalf("by phase: %s", cost["_raw"])
		}
		if cost["attempts"] != 1.0 || cost["consults"] != 1.0 || cost["packets_injected"].(float64) < 1 || cost["runner_seconds"].(float64) <= 0 ||
			!strings.Contains(str(cost, "memory_intelligence_cost"), "arm_difference") || len(cost["by_model"].([]any)) != 1 {
			t.Fatalf("report: %s", cost["_raw"])
		}
		used := func(path string) float64 {
			b := e.must(http.StatusOK, e.owner, "GET", path, nil)
			if tb, ok := b["task"].(map[string]any); ok {
				return tb["used"].(map[string]any)["cost_microusd"].(float64)
			}
			return b["budget"].(map[string]any)["used"].(map[string]any)["cost_microusd"].(float64)
		}
		if used("/v1/tasks/"+k+"/budget") != 470000 || used("/v1/tasks/"+own+"/budget") != 160000 ||
			used("/v1/projects/"+e.id["alpha"]) != 630000 {
			t.Fatalf("budgets: asker %v answerer %v project %v", used("/v1/tasks/"+k+"/budget"), used("/v1/tasks/"+own+"/budget"), used("/v1/projects/"+e.id["alpha"]))
		}
		if n := e.count(store.Runtime, `SELECT used_cost_microusd FROM budget WHERE level = 'SYSTEM'`); n != 630000 {
			t.Fatalf("system used %d", n)
		}
		e.must(http.StatusOK, e.tok["K1"], "GET", "/v1/tasks/"+k+"/cost", nil)
		e.must(http.StatusNotFound, e.tok["P"], "GET", "/v1/tasks/"+k+"/cost", nil)
		e.must(http.StatusNotFound, e.tok["V"], "GET", "/v1/tasks/"+k+"/cost", nil)
	})
}

func TestAC_S2_038_ProjectIsolationOfPartC(t *testing.T) {
	e := newEnv(t)
	e.seed()
	e.agentP("I1", "alpha", "worker", "")
	e.agentP("P", "alpha", "worker", "product")
	i := e.gated("I")
	e.give(i, "I1", nil)
	e.hook("I1", "prompt-submit", nil) // packet_log
	u := complete("I")
	u["questions"] = []map[string]any{{"text": "ALPHA-SECRET câu hỏi?", "options": []string{"a", "b"}, "default": "a", "impact": "low", "blocking": false}}
	if status, out := e.verb("I1", "understand", u); status != http.StatusCreated {
		t.Fatalf("understand: %d %s", status, out["_raw"])
	}
	e.kch("I1", "verify") // attempt
	status, out := e.kch("I1", "consult", "product", "ALPHA-SECRET tham vấn?")
	if status != http.StatusCreated {
		t.Fatalf("consult: %d %s", status, out["_raw"])
	}
	consult := str(out, "id")
	qid := str(e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+i+"/questions", nil)["questions"].([]any)[0].(map[string]any), "id")
	t.Run("AC-S2-038 through the API another project sees nothing of part C", func(t *testing.T) {
		for _, who := range []string{"V", "W"} { // V: project beta; W: alpha but not the holder (PM-S2-15)
			for _, view := range []string{"questions", "understanding", "cost"} {
				e.must(http.StatusNotFound, e.tok[who], "GET", "/v1/tasks/"+i+"/"+view, nil)
			}
		}
		lacks(t, e.must(http.StatusOK, e.tok["V"], "GET", "/v1/questions", nil)["_raw"].(string), "ALPHA-SECRET", qid)
		e.must(http.StatusOK, e.tok["L"], "GET", "/v1/tasks/"+i+"/questions", nil) // the lead of alpha may
		e.must(http.StatusForbidden, e.tok["V"], "POST", "/v1/questions/"+qid+"/answer", map[string]any{"option": "a"})
		if status, _ := e.verb("V", "answer", map[string]any{"args": []string{consult}, "stdin": "chen ngang"}); status != http.StatusNotFound {
			t.Fatalf("beta answered an alpha consult: %d", status)
		}
	})
	t.Run("AC-S2-038 under row-level security a bare SELECT of another project's agent returns nothing", func(t *testing.T) {
		for _, table := range []string{"understanding", "question", "consult", "attempt", "packet_log"} {
			if n := e.count(e.scopeOf("V"), "SELECT count(*) FROM "+table); n != 0 {
				t.Fatalf("beta sees %d rows of %s", n, table)
			}
			if n := e.count(e.scopeOf("I1"), "SELECT count(*) FROM "+table); n == 0 {
				t.Fatalf("the holder sees no row of %s", table)
			}
		}
		if n := e.count(e.scopeOf("V"), `SELECT count(*) FROM knowledge WHERE key LIKE 'ASM-I-%'`); n != 0 {
			t.Fatal("beta sees alpha's assumptions")
		}
	})
	t.Run("AC-S2-038 an answerer is always a member of the consult's project (database rule)", func(t *testing.T) {
		err := store.InScope(context.Background(), e.pool, store.Runtime, func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO consult (project_id, task_id, lease_id, asker, answerer, specialty, question, question_hash, expires_at)
				SELECT project_id, task_id, lease_id, asker, $2, specialty, question, 'x', expires_at FROM consult WHERE id = $1`, consult, e.id["V"])
			return err
		})
		if err == nil || !strings.Contains(err.Error(), "consult_project_id_answerer_fkey") {
			t.Fatalf("a consult answered from another project was stored: %v", err)
		}
		if n := e.count(store.Runtime, `SELECT count(*) FROM consult WHERE id = $1 AND answerer = $2`, consult, e.id["P"]); n != 1 {
			t.Fatal("the consult went to someone else")
		}
	})
}

func TestAC_S2_039_FeedbackWithoutLease(t *testing.T) {
	e := newEnv(t)
	e.seed()
	T := e.id["T"]
	verified := func(agent, task, repo string) {
		t.Helper()
		e.hook(agent, "prompt-submit", nil)
		e.write(filepath.Join(e.repo[repo], "src", "ok.txt"), "ok\n")
		if _, out := e.kch(agent, "verify"); out["task_state"] != "VERIFIED" {
			t.Fatalf("verify: %s", out["_raw"])
		}
		e.must(http.StatusOK, e.tok[agent], "POST", "/v1/tasks/"+task+"/release", nil)
	}
	t.Run("AC-S2-039 DIRECT: feedback sent while nobody held the task reaches the next lease, in order, and the first write is refused once", func(t *testing.T) {
		if l := e.give(T, "W", nil); l["intel_mode"] != "DIRECT" {
			t.Fatalf("lease %v", l)
		}
		verified("W", T, "alpha")
		for _, text := range []string{"F1: nút thêm nằm trong Part", "F2: đổi nhãn thành Thêm nhóm"} {
			e.must(http.StatusCreated, e.owner, "POST", "/v1/tasks/"+T+"/feedback", map[string]any{"text": text})
		}
		if e.stateOf(T) != "RUNNABLE" {
			t.Fatalf("state %s", e.stateOf(T))
		}
		notice := str(e.give(T, "W", nil), "pending_notice")
		has(t, notice, "[rev 2] F1: nút thêm nằm trong Part", "[rev 3] F2: đổi nhãn thành Thêm nhóm")
		if strings.Index(notice, "F1:") > strings.Index(notice, "F2:") {
			t.Fatalf("order: %s", notice)
		}
		has(t, e.hook("W", "prompt-submit", nil), "F1: nút thêm nằm trong Part", "F2: đổi nhãn thành Thêm nhóm")
		has(t, e.edit("W", e.in("alpha", "src", "a.go")), `"permissionDecision":"deny"`, "F1:", "F2:", "rev 3")
		if out := e.edit("W", e.in("alpha", "src", "a.go")); out != "" {
			t.Fatalf("refused twice: %s", out)
		}
	})
	t.Run("AC-S2-039 ASSISTED: the next lease needs a delta", func(t *testing.T) {
		e.agentP("S1", "alpha", "worker", "")
		s := e.gated("S")
		e.give(s, "S1", nil)
		e.hook("S1", "prompt-submit", nil)
		if status, out := e.verb("S1", "understand", complete("S")); status != http.StatusCreated {
			t.Fatalf("understand: %d %s", status, out["_raw"])
		}
		verified("S1", s, "s")
		e.must(http.StatusCreated, e.owner, "POST", "/v1/tasks/"+s+"/feedback", map[string]any{"text": "F3: giữ thứ tự cũ"})
		e.give(s, "S1", nil)
		e.hook("S1", "prompt-submit", nil)
		has(t, e.writeIn("S1", "s", "src/a.go"), `"permissionDecision":"deny"`, "F3: giữ thứ tự cũ", "nộp delta", "REQ-S")
		has(t, e.writeIn("S1", "s", "src/a.go"), `"permissionDecision":"deny"`, "nộp delta")
		if status, out := e.verb("S1", "understand", map[string]any{"delta": map[string]any{"changed": []string{}, "preserved": []string{"REQ-S"}}}); status != http.StatusCreated {
			t.Fatalf("delta: %d %s", status, out["_raw"])
		}
		if out := e.writeIn("S1", "s", "src/a.go"); out != "" {
			t.Fatalf("write after the delta: %s", out)
		}
	})
}
