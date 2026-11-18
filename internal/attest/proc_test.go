package attest

import "testing"

func TestParseStatCountsFieldsAfterTheLastParenthesis(t *testing.T) {
	tail := " S 4242 1 1 0 -1 4194560 100 0 0 0 1 2 0 0 20 0 1 0 987654 1000 10 18446744073709551615"
	for _, comm := range []string{"kch", "a b", "evil) S 1 1 (x", ")) R 7 (("} {
		start, parent, err := parseStat("77 (" + comm + ")" + tail)
		if err != nil || start != 987654 || parent != 4242 {
			t.Fatalf("%q: start %d parent %d err %v", comm, start, parent, err)
		}
	}
	for _, bad := range []string{"", "77 kch S 1", "77 (kch) S x", "77 (kch) S 1 2 3"} {
		if _, _, err := parseStat(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestAgentOf(t *testing.T) {
	for cwd, want := range map[string]string{"/r/Agents/agent-X": "agent-X", "/r/Agents/agent-X/a/b": "agent-X", "/r/Agents": "",
		"/r": "", "/r/Agents/../x": "", "/r/AgentsX/agent-Y": "", "": "", "relative/agent-X": ""} {
		if got := agentOf("/r/Agents", cwd); got != want {
			t.Fatalf("agentOf(%q) = %q, want %q", cwd, got, want)
		}
	}
}
