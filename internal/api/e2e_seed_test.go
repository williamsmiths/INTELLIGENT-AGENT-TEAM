package api_test

import (
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
)

// seed builds two projects with different domains, both using the key REQ-1
// and the same words, so any leak between them is visible.
func (e *env) seed() {
	e.t.Helper()
	o := e.owner
	one := int64(1_000_000)
	for _, name := range []string{"alpha", "beta"} {
		p := e.must(http.StatusCreated, o, "POST", "/v1/projects", map[string]any{"slug": name, "name": name})
		pid := str(p, "id")
		e.id[name] = pid
		e.must(http.StatusOK, o, "PUT", "/v1/projects/"+pid+"/limits", map[string]any{
			"default_task_max_cost_microusd": one, "budget": map[string]any{"max_cost_microusd": 5 * one}})
		e.must(http.StatusCreated, o, "POST", "/v1/projects/"+pid+"/repos", map[string]any{"path": e.repo[name]})
	}
	agent := func(name, project, role, enforcement string) {
		out := e.must(http.StatusCreated, o, "POST", "/v1/principals", map[string]any{"name": name, "openbot_agent_id": "agent-" + name,
			"enforcement": enforcement, "project_id": e.id[project], "role": role})
		e.id[name], e.tok[name] = str(out, "principal", "id"), str(out, "token")
		// A HOOK agent needs a registered install before a FULL assignment (AC-S2-011).
		e.sum[name] = strings.Repeat("0", 63) + string(rune('a'+len(e.sum)))
		e.must(http.StatusOK, o, "POST", "/v1/principals/"+e.id[name]+"/hook-install", map[string]any{
			"settings_path": filepath.Join(filepath.Dir(e.tmp), "agents", "agent-"+name, ".claude", "settings.json"), "sha256": e.sum[name]})
	}
	agent("W", "alpha", "worker", "HOOK")
	agent("L", "alpha", "lead", "HOOK")
	agent("V", "beta", "worker", "HOOK")
	agent("Z", "alpha", "worker", "HOOK")
	e.must(http.StatusCreated, o, "POST", "/v1/principals/"+e.id["Z"]+"/memberships", map[string]any{"project_id": e.id["beta"], "role": "worker"})

	know := func(project string, body map[string]any) map[string]any {
		return e.must(http.StatusCreated, o, "POST", "/v1/projects/"+e.id[project]+"/knowledge", body)
	}
	know("alpha", map[string]any{"key": "REQ-1", "kind": "requirement", "aspect": "TO_BE", "trust": "AUTHORITATIVE",
		"scope": "feature:editor", "title": "Xóa nhóm câu hỏi", "body": "ALPHA-RULE: xóa nhóm câu hỏi phải xác nhận trước."})
	know("beta", map[string]any{"key": "REQ-1", "kind": "requirement", "aspect": "TO_BE", "trust": "AUTHORITATIVE",
		"scope": "feature:editor", "title": "Xóa nhóm câu hỏi", "body": "BETA-RULE: xóa nhóm câu hỏi không cần xác nhận."})
	know("alpha", map[string]any{"key": "UI-1", "kind": "ui", "aspect": "TO_BE", "trust": "AUTHORITATIVE",
		"scope": "feature:editor", "title": "Vị trí nút thêm", "body": "Nút thêm nằm trong Part."})
	src := e.must(http.StatusCreated, o, "POST", "/v1/projects/"+e.id["alpha"]+"/sources", map[string]any{"kind": "screenshot", "content": "ảnh màn hình editor"})
	know("alpha", map[string]any{"key": "UI-1-NOW", "kind": "ui", "aspect": "AS_IS", "trust": "OBSERVED", "scope": "feature:editor",
		"title": "Vị trí nút thêm hiện tại", "body": "Nút thêm nằm ngoài Part.", "sourceids": []string{str(src, "ID")}})

	e.id["T"] = e.task("alpha", "L", "EDITOR-1")
	e.id["U"] = e.task("beta", "", "EDITOR-1")
}

// task creates a task as a lead (or the owner when lead is "") and makes it RUNNABLE.
func (e *env) task(project, lead, key string) string {
	e.t.Helper()
	tok := e.owner
	if lead != "" {
		tok = e.tok[lead]
	}
	t := e.must(http.StatusCreated, tok, "POST", "/v1/projects/"+e.id[project]+"/tasks", map[string]any{
		"key": key, "title": "Thêm nhóm câu hỏi trong Part", "desired_outcome": "Người soạn đề thêm được nhóm câu hỏi ngay trong Part.",
		"scope": "feature:editor", "repo_path": e.repo[project], "allowed_paths": []string{"src/**"},
		"protected_paths": []string{"tests/acceptance/**"}, "requirement_keys": []string{"REQ-1"},
		"required_checks": []map[string]any{{"id": "unit", "command": "test -f src/ok.txt"}}})
	id := str(t, "id")
	e.must(http.StatusOK, tok, "POST", "/v1/tasks/"+id+"/ready", nil)
	return id
}

func (e *env) in(project string, rel ...string) string {
	return filepath.Join(append([]string{e.repo[project]}, rel...)...)
}

// commit records the current working tree of a project repo.
func (e *env) commit(project string) {
	e.t.Helper()
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "work"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.repo[project]
		if out, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}

// reset throws away uncommitted work in a project repo.
func (e *env) reset(project string) {
	e.t.Helper()
	for _, args := range [][]string{{"reset", "-q", "--hard"}, {"clean", "-qfd"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = e.repo[project]
		if out, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
}
