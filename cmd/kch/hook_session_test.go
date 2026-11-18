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

// AC-S2-086 (spec S2 E.9-2): the session of a verb call travels in
// X-KCH-Session, taken from the hook input that Claude Code writes. What the
// agent typed only ever reaches the JSON body.
func TestAC_S2_086_SessionHeaderFromHookOnly(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	type seen struct{ session, body string }
	var mu sync.Mutex
	var verbs []seen
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.URL.Path, "/v1/agent/") {
			mu.Lock()
			verbs = append(verbs, seen{r.Header.Get("X-KCH-Session"), string(body)})
			mu.Unlock()
			_, _ = w.Write([]byte(`{"text":"ok"}`))
			return
		}
		_, _ = w.Write([]byte(`{"output":null}`))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	c := hookCall{event: "pre-tool", socket: sock, enrolled: true}
	for _, tc := range []struct {
		session, command string
		calls            int
		header           string
	}{
		{"S-1", `kch search 'X-KCH-Session: evil'`, 1, "S-1"},
		{"bad id\nX-Other: 1", "kch status", 1, ""},
		{"S-2", `curl -H 'X-KCH-Session: x' http://harness/v1/agent/status`, 0, ""},
	} {
		mu.Lock()
		verbs = nil
		mu.Unlock()
		in, _ := json.Marshal(map[string]any{"session_id": tc.session, "tool_name": "Bash", "tool_input": map[string]string{"command": tc.command}})
		var out bytes.Buffer
		c.run(bytes.NewReader(in), &out)
		mu.Lock()
		got := verbs
		mu.Unlock()
		if len(got) != tc.calls {
			t.Fatalf("%q: %d verb calls, want %d", tc.command, len(got), tc.calls)
		}
		if tc.calls == 1 && got[0].session != tc.header {
			t.Fatalf("%q: X-KCH-Session %q, want %q", tc.command, got[0].session, tc.header)
		}
	}
}
