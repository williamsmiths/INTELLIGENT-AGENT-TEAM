package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// REV-006b N2, N4, N5: the cases the review found without an AC.

func TestAC_S2_072_RecipientAlreadyHoldsLease(t *testing.T) {
	e := newTeam(t)
	e.repoP("alpha", "second")
	e.give(e.ownerTask("alpha", "HELD-1", "second"), "EL", nil)
	refused(t, e.delegateMust(http.StatusConflict, "PL", map[string]any{"repo": e.repo["alpha"]}), "EL đang giữ một lease")
	e.noTask("alpha", 1)
}

func TestAC_S2_073_WorkerInLeaseProjectNoFallback(t *testing.T) {
	e := newTeam(t)
	e.project("beta")
	e.agentP("X", "beta", "lead", "product")
	e.must(http.StatusCreated, e.owner, "POST", "/v1/principals/"+e.id["X"]+"/memberships", map[string]any{"project_id": e.id["alpha"], "role": "worker"})
	e.agentP("BW", "beta", "worker", "engineering")
	e.give(e.ownerTask("alpha", "X-1", "alpha"), "X", nil)
	refused(t, e.delegateMust(http.StatusForbidden, "X", nil), "chỉ Lead")
	e.noTask("beta", 0)
	e.noTask("alpha", 1)
}

func TestAC_S2_074_BudgetDefaults(t *testing.T) {
	e := newTeam(t)
	limits := "/v1/projects/" + e.id["alpha"] + "/limits"
	e.must(http.StatusOK, e.owner, "PUT", limits, map[string]any{"budget": map[string]any{"max_cost_microusd": 5_000_000}})
	refused(t, e.delegateMust(http.StatusConflict, "PL", nil), "default_task_max_cost")
	e.must(http.StatusOK, e.owner, "PUT", limits, map[string]any{"default_task_max_cost_microusd": 1_000_000})
	refused(t, e.delegateMust(http.StatusConflict, "PL", nil), "PROJECT budget max_cost") // T-H-01 inside the transaction
	e.noTask("alpha", 0)
}

func TestAC_S2_075_NotifyFailureDoesNotFailDelegate(t *testing.T) {
	e := newTeam(t)
	e.host.failing(func(context.Context) error { return errors.New("host down") })
	out := e.delegateMust(http.StatusCreated, "PL", nil)
	if out["notified"] != false || e.stateOf(str(out, "task_id")) != "RUNNING" {
		t.Fatalf("answer %v", out)
	}
	has(t, str(out, "text"), "Chưa gửi được tin báo")
	if n := e.count(store.Runtime, `SELECT count(*) FROM journal WHERE type = 'NOTIFY_FAILED' AND task_id = $1`, str(out, "task_id")); n != 1 {
		t.Fatalf("NOTIFY_FAILED events: %d", n)
	}
}

func TestAC_S2_076_PolicyValidatedOnSave(t *testing.T) {
	e := newTeam(t)
	path := "/v1/projects/" + e.id["alpha"] + "/delegate-policy"
	unit := map[string]any{"id": "unit", "command": "go test ./..."}
	for name, body := range map[string]map[string]any{
		"duplicate id":    {"required_checks": []any{unit, unit}},
		"id scope":        {"required_checks": []any{map[string]any{"id": "scope", "command": "true"}}},
		"empty command":   {"required_checks": []any{map[string]any{"id": "unit", "command": ""}}},
		"absolute path":   {"required_checks": []any{unit}, "protected_paths": []string{"/etc/**"}},
		"parent path":     {"required_checks": []any{unit}, "protected_paths": []string{"tests/../../x"}},
		"empty path":      {"required_checks": []any{unit}, "protected_paths": []string{" "}},
		"unknown field":   {"required_checks": []any{unit}, "mode": "FULL"},
		"wrong type body": {"required_checks": "unit"},
	} {
		if status, out := e.call(e.owner, "PUT", path, body); status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d: %s", name, status, out["_raw"])
		}
	}
	got := e.must(http.StatusOK, e.owner, "GET", path, nil)
	if got["required_checks"].([]any)[0].(map[string]any)["command"] != "test -f src/ok.txt" {
		t.Fatalf("a refused policy was stored: %v", got)
	}
	e.must(http.StatusOK, e.owner, "PUT", path, map[string]any{"required_checks": []any{unit}, "protected_paths": []string{"tests/**", "Makefile"}})
}

func TestAC_S2_077_LeadReadsPolicy(t *testing.T) {
	e := newTeam(t)
	_, out := e.kch("PL", "delegate-policy")
	has(t, str(out, "text"), "Check unit: test -f src/ok.txt", "tests/**")
	if status, _ := e.kch("EL", "delegate-policy"); status != http.StatusForbidden {
		t.Fatalf("worker read the policy: %d", status)
	}
}

func TestAC_S2_078_KeyNeverCollides(t *testing.T) {
	e := newTeam(t)
	day := "D-" + time.Now().Format("060102") + "-"
	e.must(http.StatusCreated, e.owner, "POST", "/v1/projects/"+e.id["alpha"]+"/tasks", map[string]any{"key": day + "2", "title": "bằng tay"})
	if out := e.delegateMust(http.StatusCreated, "PL", nil); str(out, "key") != day+"3" {
		t.Fatalf("key %v, want %s3", out["key"], day)
	}
}
