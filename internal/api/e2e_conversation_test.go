package api_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

func TestAC_S2_080_ChannelChoosesProject(t *testing.T) {
	e, h := teamTwo(t)
	h.channel("C2", "PL", "S")
	e.bind("beta", "channel", "C2")
	status, text := e.search("PL", "S")
	if status != http.StatusOK {
		t.Fatalf("search: %d %s", status, text)
	}
	has(t, text, "BETA-RULE")
	lacks(t, text, "ALPHA-RULE")
	// Without a task, `kch status` names the conversation's project and the Lead's delegate command.
	if status, out := e.verbIn("PL", "S", "status", nil); status != http.StatusOK || !strings.Contains(str(out, "text"), "Project của cuộc hội thoại này: beta (vai trò lead)") ||
		!strings.Contains(str(out, "text"), "kch delegate") {
		t.Fatalf("status: %d %s", status, out["_raw"])
	}
	if status, out := e.verbIn("PL", "S", "delegate", map[string]any{"to": "engineering", "title": "Việc P2",
		"outcome": "Xong việc P2.", "allowed_paths": []string{"src/**"}}); status != http.StatusCreated {
		t.Fatalf("delegate: %d %s", status, out["_raw"])
	}
	e.noTask("beta", 1)
	e.noTask("alpha", 0)
}

func TestAC_S2_085_PrivateThread(t *testing.T) {
	e, h := teamTwo(t)
	h.mainThread("PL", "T", "S-T")
	e.bind("alpha", "thread", "T")
	status, text := e.search("PL", "S-T")
	if status != http.StatusOK {
		t.Fatalf("search: %d %s", status, text)
	}
	has(t, text, "ALPHA-RULE")
	lacks(t, text, "BETA-RULE")
	// A thread of another agent is not the caller's: no binding, back to the membership rule.
	h.mainThread("OTHER", "T2", "S-T2")
	e.bind("alpha", "thread", "T2")
	if status, text := e.search("PL", "S-T2"); status != http.StatusConflict {
		t.Fatalf("search in another agent's thread: %d %s", status, text)
	}
}

func TestAC_S2_081_MismatchIsClosed(t *testing.T) {
	e, h := teamTwo(t)
	e.give(e.ownerTask("alpha", "A-1", "alpha"), "PL", nil)
	h.channel("C2", "PL", "S")
	e.bind("beta", "channel", "C2")
	status, text := e.search("PL", "S")
	if status != http.StatusConflict {
		t.Fatalf("search: %d %s", status, text)
	}
	has(t, text, "beta", "A-1", "alpha")
	lacks(t, text, "ALPHA-RULE", "BETA-RULE")
	if status, out := e.verbIn("PL", "S", "delegate", map[string]any{"to": "engineering", "title": "x", "outcome": "y",
		"allowed_paths": []string{"src/**"}}); status != http.StatusConflict {
		t.Fatalf("delegate: %d %s", status, out["_raw"])
	}
	e.noTask("beta", 0)
	out := e.hook("PL", "pre-tool", map[string]any{"session_id": "S", "tool_name": "Write", "cwd": e.repo["alpha"],
		"tool_input": map[string]any{"file_path": e.in("alpha", "src", "x.txt")}})
	has(t, out, `"deny"`, "tin này đến từ cuộc hội thoại của beta")
	if n := e.count(store.Runtime, `SELECT count(*) FROM journal WHERE type = 'WRITE_DENIED' AND payload->>'rule' = 'conversation_mismatch'`); n != 1 {
		t.Fatalf("WRITE_DENIED conversation_mismatch: %d", n)
	}
	// A lone kch verb is judged by the verb's own 409, not by the write rule (D3).
	lacks(t, e.hook("PL", "pre-tool", map[string]any{"session_id": "S", "tool_name": "Bash", "kch_verb": true}), "tin này đến từ")
}

func TestAC_S2_089_ThreadNeverBlocksLease(t *testing.T) {
	e, h := teamTwo(t)
	e.must(http.StatusCreated, e.owner, "POST", "/v1/principals/"+e.id["EB"]+"/memberships", map[string]any{"project_id": e.id["alpha"], "role": "worker"})
	h.mainThread("EB", "TE", "S-TE")
	e.bind("alpha", "thread", "TE")
	h.channel("C2", "PL", "S-PL")
	e.bind("beta", "channel", "C2")
	if status, out := e.verbIn("PL", "S-PL", "delegate", map[string]any{"to": "engineering", "title": "Việc P2",
		"outcome": "Xong việc P2.", "scope": "feature:p2", "allowed_paths": []string{"src/**"}}); status != http.StatusCreated {
		t.Fatalf("delegate: %d %s", status, out["_raw"])
	}
	has(t, e.hook("EB", "prompt-submit", map[string]any{"session_id": "S-TE"}), "Việc P2")
	understanding := map[string]any{"goal": "Làm việc P2", "assumptions": []map[string]any{{"text": "Không có yêu cầu nào khác ngoài brief",
		"basis": "inferred", "evidence": "brief của Lead"}}}
	for _, v := range []struct {
		verb string
		body any
	}{{"understand", understanding}, {"context", map[string]any{"args": []string{}}}, {"verify", map[string]any{"args": []string{}}}} {
		if status, out := e.verbIn("EB", "S-TE", v.verb, v.body); status >= 300 {
			t.Fatalf("%s: %d %s", v.verb, status, out["_raw"])
		}
	}
	out := e.hook("EB", "pre-tool", map[string]any{"session_id": "S-TE", "tool_name": "Write", "cwd": e.repo["beta"],
		"tool_input": map[string]any{"file_path": e.in("beta", "src", "ok.txt")}})
	if out != "" {
		t.Fatalf("write in the task's repo from the bound thread: %s", out)
	}
}

func TestAC_S2_090_NoPacketInMismatchedChannel(t *testing.T) {
	e, h := teamTwo(t)
	e.must(http.StatusCreated, e.owner, "POST", "/v1/principals/"+e.id["EB"]+"/memberships", map[string]any{"project_id": e.id["alpha"], "role": "worker"})
	e.give(e.ownerTask("alpha", "A-2", "alpha"), "EB", nil)
	h.channel("C2", "EB", "S")
	e.bind("beta", "channel", "C2")
	out := e.hook("EB", "prompt-submit", map[string]any{"session_id": "S"})
	has(t, out, "additionalContext", "tin này đến từ cuộc hội thoại của beta")
	lacks(t, out, "Ngữ cảnh task", "Hoàn thành A-2", "ALPHA-RULE")
	has(t, e.hook("EB", "prompt-submit", map[string]any{"session_id": "other"}), "Hoàn thành A-2") // outside the channel: the packet as before
}
