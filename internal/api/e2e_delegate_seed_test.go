package api_test

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// Spec S2 part E (kch delegate) over HTTP with real PostgreSQL and real git
// repositories; only the host's messaging is the fake notifier.

// project creates a project with limits, its repo and the Owner's delegate
// policy {required_checks:[unit], protected_paths:["tests/**"]} (E.6 Background).
func (e *env) project(name string) {
	e.t.Helper()
	o, one := e.owner, int64(1_000_000)
	e.id[name] = str(e.must(http.StatusCreated, o, "POST", "/v1/projects", map[string]any{"slug": name, "name": name}), "id")
	e.must(http.StatusOK, o, "PUT", "/v1/projects/"+e.id[name]+"/limits", map[string]any{
		"default_task_max_cost_microusd": one, "budget": map[string]any{"max_cost_microusd": 5 * one}})
	e.must(http.StatusCreated, o, "POST", "/v1/projects/"+e.id[name]+"/repos", map[string]any{"path": e.repo[name]})
	e.must(http.StatusOK, o, "PUT", "/v1/projects/"+e.id[name]+"/delegate-policy", map[string]any{
		"required_checks": []map[string]any{{"id": "unit", "command": "test -f src/ok.txt"}}, "protected_paths": []string{"tests/**"}})
}

// newTeam: project alpha; PL = Product Lead (lead, product); EL = Engineering
// Lead (worker, engineering, HOOK, enrolled, OpenBot id).
func newTeam(t *testing.T) *env {
	e := newEnv(t)
	e.project("alpha")
	e.agentP("PL", "alpha", "lead", "product")
	e.agentP("EL", "alpha", "worker", "engineering")
	return e
}

// agentInstall registers the hook install of an agent again (AC-S2-011).
func (e *env) agentInstall(name string) {
	e.t.Helper()
	e.must(http.StatusOK, e.owner, "POST", "/v1/principals/"+e.id[name]+"/hook-install", map[string]any{
		"settings_path": filepath.Join(filepath.Dir(e.tmp), "agents", "agent-"+name, ".claude", "settings.json"), "sha256": e.sum[name]})
}

// delegate runs `kch delegate <<'EOF' {…} EOF` as an agent; extra overrides
// fields of a valid body (nil value: remove the field).
func (e *env) delegate(agent string, extra map[string]any) (int, map[string]any) {
	body := map[string]any{"to": "engineering", "title": "Thêm nút lưu nháp", "outcome": "Người soạn đề lưu được bản nháp.",
		"scope": "feature:editor", "allowed_paths": []string{"src/**"}}
	for k, v := range extra {
		if v == nil {
			delete(body, k)
		} else {
			body[k] = v
		}
	}
	return e.verb(agent, "delegate", body)
}

// delegateMust is delegate + a status assertion.
func (e *env) delegateMust(want int, agent string, extra map[string]any) map[string]any {
	e.t.Helper()
	status, out := e.delegate(agent, extra)
	if status != want {
		e.t.Fatalf("delegate by %s: status %d, want %d: %s", agent, status, want, out["_raw"])
	}
	return out
}

// tasks counts the tasks of a project (any state).
func (e *env) tasks(project string) int {
	return e.count(store.Runtime, `SELECT count(*) FROM task WHERE project_id = $1`, e.id[project])
}

// noTask asserts a refused delegate left nothing behind (E.1-8).
func (e *env) noTask(project string, want int) {
	e.t.Helper()
	if n := e.tasks(project); n != want {
		e.t.Fatalf("project %s has %d tasks, want %d", project, n, want)
	}
}

// ownerTask creates and readies an Owner task in a repo of a project.
func (e *env) ownerTask(project, key, repo string) string {
	e.t.Helper()
	id := str(e.must(http.StatusCreated, e.owner, "POST", "/v1/projects/"+e.id[project]+"/tasks", map[string]any{"key": key,
		"title": "Việc " + key, "desired_outcome": "Hoàn thành " + key, "repo_path": e.repo[repo], "allowed_paths": []string{"src/**"},
		"required_checks": []map[string]any{{"id": "unit", "command": "true"}}}), "id")
	e.must(http.StatusOK, e.owner, "POST", "/v1/tasks/"+id+"/ready", nil)
	return id
}

// viaDelegate counts journal events of a type written by an actor with via=delegate.
func (e *env) viaDelegate(typ, actor string) int {
	return e.count(store.Runtime, `SELECT count(*) FROM journal WHERE type = $1 AND actor = $2 AND payload->>'via' = 'delegate'`,
		typ, e.id[actor])
}

// lastSent is the last message the host delivered to an agent ("" if none).
func (e *env) lastSent(agent string) string {
	e.host.mu.Lock()
	defer e.host.mu.Unlock()
	for i := len(e.host.sent) - 1; i >= 0; i-- {
		if text, ok := strings.CutPrefix(e.host.sent[i], "agent-"+agent+": "); ok {
			return text
		}
	}
	return ""
}

func refused(t *testing.T, out map[string]any, parts ...string) {
	t.Helper()
	has(t, str(out, "error"), parts...)
}
