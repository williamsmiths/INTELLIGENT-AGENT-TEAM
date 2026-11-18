package api_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/openbot"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

func TestAC_S2_082_BindingGrantsNothing(t *testing.T) {
	e, h := teamTwo(t)
	e.repo["gamma"] = e.gitRepo(filepath.Join(filepath.Dir(e.tmp), "repo-gamma"))
	e.project("gamma")
	e.know("gamma", "GAMMA-RULE", "rule", "AUTHORITATIVE", "feature:editor")
	h.channel("C3", "PL", "S3")
	e.bind("gamma", "channel", "C3")
	status, text := e.search("PL", "S3")
	if status != http.StatusConflict {
		t.Fatalf("search: %d %s", status, text)
	}
	lacks(t, text, "GAMMA-RULE", "ALPHA-RULE", "BETA-RULE", "gamma")
	if status, out := e.verbIn("PL", "S3", "delegate", map[string]any{"to": "engineering", "title": "x", "outcome": "y",
		"allowed_paths": []string{"src/**"}}); status != http.StatusConflict {
		t.Fatalf("delegate: %d %s", status, out["_raw"])
	}
	for _, p := range []string{"alpha", "beta", "gamma"} {
		e.noTask(p, 0)
	}
}

func TestAC_S2_083_LookupFailsOpenToOldRule(t *testing.T) {
	e, h := teamTwo(t)
	e.bind("beta", "channel", "C1")
	e.bind("beta", "channel", "C2")
	h.channel("C1", "PL", "S-two")
	h.channel("C2", "PL", "S-two")
	for _, session := range []string{"S-unknown", "S-two"} {
		if status, text := e.search("PL", session); status != http.StatusConflict {
			t.Fatalf("%s: %d %s", session, status, text)
		} else {
			has(t, text, "thuộc 2 project")
		}
	}
	// openbot.db unreadable: same answer, and the failure is journaled once.
	broken := filepath.Join(t.TempDir(), "openbot.db")
	if err := os.WriteFile(broken, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	b := newEnv(t, func(s *api.Server) { s.Conversations = openbot.NewConversations(broken) })
	b.project("alpha")
	b.project("beta")
	b.agentP("PL", "alpha", "lead", "product")
	b.must(http.StatusCreated, b.owner, "POST", "/v1/principals/"+b.id["PL"]+"/memberships", map[string]any{"project_id": b.id["beta"], "role": "lead"})
	for range 2 {
		if status, text := b.search("PL", "S"); status != http.StatusConflict {
			t.Fatalf("unreadable host db: %d %s", status, text)
		}
	}
	if n := b.count(store.Runtime, `SELECT count(*) FROM journal WHERE type = 'CONVERSATION_LOOKUP_FAILED'`); n != 1 {
		t.Fatalf("CONVERSATION_LOOKUP_FAILED: %d, want 1", n)
	}
}

func TestAC_S2_084_OnlyOwnerBinds(t *testing.T) {
	e, _ := teamTwo(t)
	body := map[string]any{"kind": "channel", "openbot_id": "C9"}
	path := "/v1/projects/" + e.id["alpha"] + "/conversations"
	e.must(http.StatusForbidden, e.tok["PL"], "PUT", path, body)
	e.must(http.StatusForbidden, e.tok["PL"], "GET", path, nil)
	e.must(http.StatusForbidden, e.tok["PL"], "DELETE", path+"/channel/C9", nil)
	agentOnly := httptest.NewServer(e.srv.AgentRoutes(api.TokenAuth{Pool: e.pool})) // what agent.sock serves
	defer agentOnly.Close()
	url := e.url
	e.url = agentOnly.URL
	e.must(http.StatusNotFound, e.tok["PL"], "PUT", path, body)
	e.url = url
	if n := e.count(store.Runtime, `SELECT count(*) FROM conversation_binding`); n != 0 {
		t.Fatalf("bindings: %d", n)
	}
}

func TestAC_S2_087_NoPacketWithoutLease(t *testing.T) {
	e, h := teamTwo(t)
	h.channel("C2", "PL", "S")
	e.bind("beta", "channel", "C2")
	if out := e.hook("PL", "prompt-submit", map[string]any{"session_id": "S"}); out != "" {
		t.Fatalf("context without a lease: %s", out)
	}
}

func TestAC_S2_088_BindListUnbind(t *testing.T) {
	e, _ := teamTwo(t)
	path := func(p string) string { return "/v1/projects/" + e.id[p] + "/conversations" }
	for _, body := range []map[string]any{{"kind": "group", "openbot_id": "C2"}, {"kind": "channel", "openbot_id": ""},
		{"kind": "thread", "openbot_id": "a\nb"}} {
		e.must(http.StatusUnprocessableEntity, e.owner, "PUT", path("beta"), body)
	}
	e.must(http.StatusNotFound, e.owner, "PUT", "/v1/projects/00000000-0000-0000-0000-000000000000/conversations",
		map[string]any{"kind": "channel", "openbot_id": "C2"})
	e.bind("beta", "channel", "C2")
	e.bind("alpha", "channel", "C2") // rebinding moves the conversation
	if n := len(e.must(http.StatusOK, e.owner, "GET", path("beta"), nil)["conversations"].([]any)); n != 0 {
		t.Fatalf("beta still has %d bindings", n)
	}
	if n := len(e.must(http.StatusOK, e.owner, "GET", path("alpha"), nil)["conversations"].([]any)); n != 1 {
		t.Fatalf("alpha has %d bindings", n)
	}
	e.must(http.StatusOK, e.owner, "DELETE", path("alpha")+"/channel/C2", nil)
	if n := len(e.must(http.StatusOK, e.owner, "GET", path("alpha"), nil)["conversations"].([]any)); n != 0 {
		t.Fatalf("alpha still has %d bindings", n)
	}
	e.must(http.StatusNotFound, e.owner, "DELETE", path("alpha")+"/channel/C2", nil)
	if n := e.count(store.Runtime, `SELECT count(*) FROM journal WHERE type = 'CONVERSATION_UNBOUND'`); n != 1 {
		t.Fatalf("CONVERSATION_UNBOUND: %d", n)
	}
}
