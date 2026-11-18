package api_test

import (
	"net/http"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/attest/attesttest"
)

// AC-S2-001: who a peer is follows from a real process tree, never from a request.
func TestAC_S2_001_AttestOnTheAgentSocket(t *testing.T) {
	f := enrolled(t)
	whoami := func(steps []attesttest.Step, sock string) map[string]any {
		t.Helper()
		status, out := f.from(steps, sock, "GET", "/v1/whoami", nil)
		if status != http.StatusOK {
			t.Fatalf("whoami: %d %s", status, out["_raw"])
		}
		return out
	}
	x := whoami(f.tree("X", attesttest.Step{}), f.agentSock) // host -> provider (cwd agent-X) -> client
	if x["agent_id"] != "agent-X" || x["allowed"] != true || x["role"] != "agent" || str(x, "principal", "id") != f.id["X"] {
		t.Fatalf("whoami of agent-X: %s", x["_raw"])
	}
	// agent-U has a workspace and a real tree, but no principal: attested, not let in.
	u := whoami(f.tree("U", attesttest.Step{}), f.agentSock)
	if u["agent_id"] != "agent-U" || u["allowed"] != false || u["refused"] != "not_enrolled" || u["role"] != "none" || u["principal"] != nil {
		t.Fatalf("whoami of agent-U: %s", u["_raw"])
	}
	if status, _ := f.from(f.tree("U", attesttest.Step{}), f.agentSock, "POST", "/v1/hooks/claude/pre-tool", map[string]any{}); status != http.StatusUnauthorized {
		t.Fatalf("agent-U on the agent socket: %d, want 401", status)
	}
	if status, _ := f.from(f.tree("U"), f.agentSock, "POST", "/v1/agent/status", nil); status != http.StatusUnauthorized {
		t.Fatalf("agent-U verb: %d, want 401", status)
	}
	if n := f.events("/v1/system/events", "ATTEST_REJECTED", `"reason":"not_enrolled"`); n != 2 {
		t.Fatalf("%d ATTEST_REJECTED events for the two refused processes, want 2 (whoami is not journaled)", n)
	}
	if status, out := f.from(f.tree("X"), f.agentSock, "POST", "/v1/agent/status", nil); status != http.StatusOK || out["agent_id"] != "agent-X" {
		t.Fatalf("agent-X verb: %d %s", status, out["_raw"])
	}
}

// AC-S2-002: an agent cannot become another agent, and a process outside the host tree is nobody.
func TestAC_S2_002_NoImpersonation(t *testing.T) {
	f := enrolled(t)
	f.o(http.StatusOK, "POST", "/v1/principals/"+f.id["Y"]+"/hook-install", map[string]any{"settings_path": f.ws("Y") + "/.claude/settings.json", "sha256": "0000000000000000000000000000000000000000000000000000000000000000"})
	agentOf := func(name string, steps ...attesttest.Step) {
		t.Helper()
		status, out := f.from(steps, f.agentSock, "POST", "/v1/agent/status", map[string]any{"principal_id": f.id["Y"], "agent_id": "agent-Y"},
			"Authorization", "Bearer agent-Y", "X-Agent", "agent-Y")
		if status != http.StatusOK || out["agent_id"] != "agent-X" || out["principal_id"] != f.id["X"] {
			t.Fatalf("%s: %d %s, want agent-X", name, status, out["_raw"])
		}
	}
	agentOf("the client moves into the workspace of agent-Y", f.tree("X", attesttest.Step{Chdir: f.ws("Y")})...)
	agentOf("KCH_AGENT and a body naming agent-Y", f.tree("X", attesttest.Step{Env: []string{"KCH_AGENT=agent-Y", "CLAUDE_PROJECT_DIR=" + f.ws("Y")}})...)
	agentOf("an intermediate process that runs the host binary with a child in agent-Y",
		f.tree("X", attesttest.Step{Exe: f.host, Dir: f.dir}, attesttest.Step{Exe: f.provider, Dir: f.ws("Y")}, attesttest.Step{})...)

	refused := func(name string, steps ...attesttest.Step) {
		t.Helper()
		if status, out := f.from(steps, f.agentSock, "POST", "/v1/agent/status", nil); status != http.StatusUnauthorized {
			t.Fatalf("%s: %d %s, want 401", name, status, out["_raw"])
		}
		if status, _ := f.from(steps, f.agentSock, "POST", "/v1/hooks/claude/pre-tool", map[string]any{}); status != http.StatusUnauthorized {
			t.Fatalf("%s: hook %d, want 401", name, status)
		}
	}
	refused("no host in the ancestors", attesttest.Step{Exe: f.provider, Dir: f.ws("X")}, attesttest.Step{})
	refused("detached from the tree (double fork)", f.tree("X", attesttest.Step{}, attesttest.Step{Orphan: true})...)
	refused("a workspace directory that is not below AgentsRoot", attesttest.Step{Exe: f.host, Dir: f.dir}, attesttest.Step{Exe: f.provider, Dir: f.dir})
	// REV-004 #17: the hook client runs outside the sandbox; a sandboxed peer is not it.
	refused("a peer inside the provider's sandbox", f.tree("X", attesttest.Step{Exe: f.bwrap}, attesttest.Step{Exe: f.provider})...)
}

// AC-S2-003: the agent socket never grants Owner; the owner socket refuses agents and runners.
func TestAC_S2_003_AC_S2_006_SocketsAndRoles(t *testing.T) {
	f := enrolled(t)
	project := map[string]any{"slug": "gamma", "name": "gamma"}
	t.Run("AC-S2-003 an enrolled agent finds no owner route on the agent socket", func(t *testing.T) {
		for _, r := range [][2]string{{"POST", "/v1/projects"}, {"GET", "/v1/me"}, {"GET", "/v1/system/events"}, {"PUT", "/v1/system/budget"},
			{"GET", "/v1/projects/" + f.id["alpha"]}, {"POST", "/v1/principals"}, {"GET", "/v1/principals/" + f.id["X"]}} {
			if status, out := f.from(f.tree("X"), f.agentSock, r[0], r[1], project, "Authorization", "Bearer owner"); status != http.StatusNotFound && status != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s on the agent socket: %d %s, want 404", r[0], r[1], status, out["_raw"])
			}
		}
	})
	t.Run("AC-S2-003 the owner socket refuses an enrolled agent and its children", func(t *testing.T) {
		// The host is detached from this test process, which plays harnessd: otherwise
		// the tree would be refused as a runner child before the agent rule is reached.
		for name, steps := range map[string][]attesttest.Step{"the provider": f.freeTree("X"), "a child of the provider": f.freeTree("X", attesttest.Step{}),
			"a sandboxed child": f.freeTree("X", attesttest.Step{Exe: f.bwrap}, attesttest.Step{Exe: f.provider})} {
			if status, out := f.from(steps, f.ownerSock, "GET", "/v1/system/events", nil); status != http.StatusForbidden {
				t.Fatalf("%s on the owner socket: %d %s, want 403", name, status, out["_raw"])
			}
			if status, out := f.from(steps, f.ownerSock, "POST", "/v1/projects", project); status != http.StatusForbidden {
				t.Fatalf("%s creates a project: %d %s, want 403", name, status, out["_raw"])
			}
		}
		if _, out := f.from(f.freeTree("X"), f.ownerSock, "GET", "/v1/whoami", nil); out["allowed"] != false || out["refused"] != "enrolled_agent" || out["role"] != "none" {
			t.Fatalf("whoami of an agent on the owner socket: %s", out["_raw"])
		}
	})
	t.Run("AC-S2-003 the owner socket refuses a child of harnessd (a verification check)", func(t *testing.T) {
		// This test process plays harnessd; a process it starts and that stays attached is a runner child.
		runner := []attesttest.Step{{Exe: f.provider, Dir: f.repo["alpha"]}, {}}
		if status, out := f.from(runner, f.ownerSock, "GET", "/v1/system/events", nil); status != http.StatusForbidden {
			t.Fatalf("runner child on the owner socket: %d %s, want 403", status, out["_raw"])
		}
		if status, _ := f.from(runner, f.agentSock, "POST", "/v1/agent/status", nil); status != http.StatusUnauthorized {
			t.Fatalf("runner child on the agent socket: %d, want 401", status)
		}
		if n := f.events("/v1/system/events", "ATTEST_REJECTED", `"reason":"runner"`); n != 1 {
			t.Fatalf("%d ATTEST_REJECTED events for the runner child, want 1", n)
		}
	})
	t.Run("AC-S2-003 AC-S2-006 any other process of the OS user is the Owner, whatever Authorization says", func(t *testing.T) {
		status, out := f.from(f.detached(), f.ownerSock, "POST", "/v1/projects", project, "Authorization", "Bearer kch_wrong-token")
		if status != http.StatusCreated {
			t.Fatalf("a detached process on the owner socket: %d %s, want 201", status, out["_raw"])
		}
		if _, who := f.from(f.detached(), f.ownerSock, "GET", "/v1/whoami", nil); who["role"] != "owner" || who["allowed"] != true {
			t.Fatalf("whoami of the detached process: %s", who["_raw"])
		}
		// An OpenBot agent that was never enrolled is such a process too (limitation L1′).
		if status, _ := f.from(f.freeTree("U"), f.ownerSock, "GET", "/v1/system/events", nil); status != http.StatusOK {
			t.Fatalf("an agent without a principal on the owner socket: %d, want 200 (L1′)", status)
		}
		// The owner socket has no hook and no agent verb.
		for _, path := range []string{"/v1/hooks/claude/pre-tool", "/v1/agent/status"} {
			if status, _ := f.from(f.detached(), f.ownerSock, "POST", path, map[string]any{}); status != http.StatusNotFound {
				t.Fatalf("POST %s on the owner socket: %d, want 404", path, status)
			}
		}
	})
}
