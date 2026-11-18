package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// The server lets a lone `kch <verb>` through while a task waits because the
// hook client flags it (kch_verb). The invariant that makes this safe: the
// flag is true only for a command the client then replaces by the verb call,
// so what the agent typed never runs as Bash (REV-005 #2, re-verify C2).
func TestKchVerbFlagOnlyForReplacedCommands(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var flags []bool
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/v1/hooks/claude/") {
			var in struct {
				KchVerb   bool `json:"kch_verb"`
				ToolInput struct {
					Command string `json:"command"`
				} `json:"tool_input"`
			}
			_ = json.Unmarshal(body, &in)
			if in.ToolInput.Command != "" {
				t.Errorf("the command text reached the server: %q", in.ToolInput.Command)
			}
			mu.Lock()
			flags = append(flags, in.KchVerb)
			mu.Unlock()
			_, _ = w.Write([]byte(`{"output":null}`))
			return
		}
		_, _ = w.Write([]byte(`{"text":"ok"}`)) // /v1/agent/<verb>
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := hookCall{event: "pre-tool", socket: sock, enrolled: true}
	for _, tc := range []struct {
		command string
		flag    bool
		output  string // what the client prints: the rewrite, a deny, or nothing
	}{
		{"kch status", true, `"updatedInput"`},
		{"/tmp/x/kch status", true, `"updatedInput"`},
		{"kch status && touch f", false, `"permissionDecision":"deny"`},
		{"cd /x && kch understand <<'END'\n{}\nEND", false, "không `cd … &&`"},
		{`kch "$(id)"`, false, `"permissionDecision":"deny"`},
		{"touch f", false, ""},
		{"echo kch status", false, ""},
	} {
		mu.Lock()
		flags = nil
		mu.Unlock()
		in, _ := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": tc.command}})
		var out bytes.Buffer
		c.run(bytes.NewReader(in), &out)
		mu.Lock()
		got := flags
		mu.Unlock()
		if len(got) != 1 || got[0] != tc.flag {
			t.Errorf("%q: kch_verb sent %v, want [%v]", tc.command, got, tc.flag)
		}
		switch {
		case tc.output == "" && out.Len() != 0:
			t.Errorf("%q: printed %s, want nothing", tc.command, out.String())
		case tc.output != "" && !strings.Contains(out.String(), tc.output):
			t.Errorf("%q: printed %s, want %s", tc.command, out.String(), tc.output)
		}
		if tc.flag && strings.Contains(out.String(), `"updatedInput"`) && strings.Contains(out.String(), tc.command) {
			t.Errorf("%q: the replaced command still contains what the agent typed: %s", tc.command, out.String())
		}
	}
}
