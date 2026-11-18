package api_test

import (
	"fmt"
	"net/http"
	"regexp"
	"testing"
)

func TestAC_S2_060_LeadDelegatesToWorker(t *testing.T) {
	e := newTeam(t)
	out := e.delegateMust(http.StatusCreated, "PL", nil)
	if !regexp.MustCompile(`^D-\d{6}-1$`).MatchString(str(out, "key")) || str(out, "assignee") != "EL" ||
		fmt.Sprint(out["checks"]) != "[unit]" || out["notified"] != true || str(out, "repo") != e.repo["alpha"] {
		t.Fatalf("answer %v", out)
	}
	view := e.must(http.StatusOK, e.owner, "GET", "/v1/tasks/"+str(out, "task_id"), nil)
	task, lease := view["task"].(map[string]any), view["lease"].(map[string]any)
	if task["state"] != "RUNNING" || lease["principal_id"] != e.id["EL"] || lease["mode"] != "FULL" || lease["state"] != "ACTIVE" {
		t.Fatalf("task %v lease %v", task, lease)
	}
	if fmt.Sprint(task["protected_paths"]) != "[tests/**]" || task["created_by"] != e.id["PL"] ||
		fmt.Sprint(task["required_checks"]) != "[map[command:test -f src/ok.txt id:unit]]" {
		t.Fatalf("task %v", task)
	}
	if max := view["budget"].(map[string]any)["task"].(map[string]any)["limits"].(map[string]any)["max_cost_microusd"]; max != 1e6 {
		t.Fatalf("task budget %v, want the project default", max)
	}
	for _, typ := range []string{"TASK_CREATED", "LEASE_GRANTED"} {
		if n := e.viaDelegate(typ, "PL"); n != 1 {
			t.Fatalf("%s with actor PL and via=delegate: %d", typ, n)
		}
	}
	has(t, e.lastSent("EL"), "[kc-harness] Task "+str(out, "key"), "giao bởi PL")
}

func TestAC_S2_061_NoSelfNoLeadToLeadNoWorker(t *testing.T) {
	e := newTeam(t)
	e.agentP("QL", "alpha", "lead", "quality")
	refused(t, e.delegateMust(http.StatusForbidden, "PL", map[string]any{"to": "product"}), "Lead")
	refused(t, e.delegateMust(http.StatusForbidden, "PL", map[string]any{"to": "quality"}), "Lead")
	refused(t, e.delegateMust(http.StatusForbidden, "EL", map[string]any{"to": "product"}), "chỉ Lead")
	e.noTask("alpha", 0)
}

func TestAC_S2_062_ProjectFromRuntime(t *testing.T) {
	e := newTeam(t)
	e.project("beta")
	e.must(http.StatusCreated, e.owner, "POST", "/v1/principals/"+e.id["PL"]+"/memberships", map[string]any{"project_id": e.id["beta"], "role": "lead"})
	refused(t, e.delegateMust(http.StatusConflict, "PL", nil), "không xác định được project", "alpha, beta")
	e.noTask("alpha", 0)
	e.noTask("beta", 0)
}

func TestAC_S2_063_LeadSetsNoOwnerField(t *testing.T) {
	e := newTeam(t)
	for field, v := range map[string]any{"required_checks": []map[string]any{{"id": "x", "command": "true"}},
		"protected_paths": []string{}, "mode": "OBSERVE", "intel_mode": "DIRECT", "intel_features": []string{},
		"assignee": e.id["EL"], "network": true} {
		status, out := e.delegate("PL", map[string]any{field: v})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("%s: status %d: %s", field, status, out["_raw"])
		}
		has(t, str(out, "error"), field)
	}
	e.noTask("alpha", 0)
}

func TestAC_S2_064_RecipientReallyProtected(t *testing.T) {
	e := newTeam(t)
	el := e.id["EL"]
	for _, c := range []struct{ name, broken, fixed string }{
		{"SUPERVISED", `UPDATE principal SET enforcement = 'SUPERVISED' WHERE id = $1`, `UPDATE principal SET enforcement = 'HOOK' WHERE id = $1`},
		{"no hook install", `DELETE FROM hook_install WHERE principal_id = $1`, ""},
		{"tampered", `UPDATE hook_install SET tampered = true WHERE principal_id = $1`, `UPDATE hook_install SET tampered = false WHERE principal_id = $1`},
		{"no openbot id", `UPDATE principal SET openbot_agent_id = NULL WHERE id = $1`, `UPDATE principal SET openbot_agent_id = 'agent-EL' WHERE id = $1`},
	} {
		e.admin(c.broken, el)
		refused(t, e.delegateMust(http.StatusConflict, "PL", nil), "người nhận EL")
		e.noTask("alpha", 0)
		if c.fixed != "" {
			e.admin(c.fixed, el)
		} else {
			e.agentInstall("EL")
		}
	}
	e.delegateMust(http.StatusCreated, "PL", nil) // every refusal above came from the broken condition
}
