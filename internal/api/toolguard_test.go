package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
)

// AC-S2-007: the tools that run outside the sandbox obey an allowlist in
// every lease mode and without a lease. Every call comes from the real
// process tree of the enrolled agent.
func TestAC_S2_007_ToolAllowlistInEveryMode(t *testing.T) {
	f := enrolled(t)
	ws, alpha, beta := f.ws("X"), f.repo["alpha"], f.repo["beta"]
	tmp := filepath.Join(f.ptmp, "agent-X")
	file := func(p string) map[string]any { return map[string]any{"file_path": p} }
	var t2 *testing.T // the running subtest
	denied := func(mode, what, tool string, input map[string]any) {
		t := t2
		t.Helper()
		if out := f.tool("X", tool, input); out == "" {
			t.Fatalf("mode %s: %s was allowed (%s %v)", mode, what, tool, input)
		} else {
			has(t, out, `"permissionDecision":"deny"`)
		}
	}
	allowed := func(mode, what, tool string, input map[string]any) {
		t := t2
		t.Helper()
		if out := f.tool("X", tool, input); out != "" {
			t.Fatalf("mode %s: %s was refused (%s %v): %s", mode, what, tool, input, out)
		}
	}
	for _, mode := range []string{"none", "OBSERVE", "CONTEXT", "FULL"} {
		t.Run("AC-S2-007 mode "+mode, func(t *testing.T) {
			t2 = t
			if mode != "none" {
				// A brief-only task routes ASSISTED (spec S2 C.1: B = 2) and would need an
				// understanding first (AC-S2-031); this test is about the tool allowlist, so the
				// owner forces DIRECT.
				f.o(http.StatusOK, "POST", "/v1/tasks/"+f.task("T-"+mode)+"/assign", map[string]any{"principal": f.id["X"], "mode": mode, "intel_mode": "DIRECT"})
				f.hook("X", "prompt-submit", nil) // the context is acknowledged: a FULL write is judged by scope
				defer f.release("X")
			}
			// Reads outside the read roots.
			denied(mode, "Read in ~/.config", "Read", file(filepath.Join(f.home, ".config", "secret.txt")))
			denied(mode, "Read in the repo of another project", "Read", file(filepath.Join(beta, "src", "keep.txt")))
			denied(mode, "Read /proc/1/environ", "Read", file("/proc/1/environ"))
			denied(mode, "Read the temp dir of another session", "Read", file(f.other))
			denied(mode, "Read through .. out of the workspace", "Read", file(ws+"/../agent-Y/notes.md"))
			denied(mode, "Glob the home", "Glob", map[string]any{"pattern": "**/*.env", "path": f.home})
			denied(mode, "Glob with an absolute pattern", "Glob", map[string]any{"pattern": "/etc/**"})
			denied(mode, "Glob with .. in the pattern", "Glob", map[string]any{"pattern": "../../.config/*", "path": ws})
			denied(mode, "Grep the repo of another project", "Grep", map[string]any{"pattern": "RULE", "path": beta})
			has(t, f.hook("X", "pre-tool", map[string]any{"tool_name": "Grep", "cwd": f.home, "tool_input": map[string]any{"pattern": "x"}}),
				`"permissionDecision":"deny"`) // no path: the search starts at the cwd, which is outside
			// Writes outside the task repo and the private temp root.
			denied(mode, "Write ~/.bashrc", "Write", file(filepath.Join(f.home, ".bashrc")))
			denied(mode, "Edit ~/.claude/settings.json", "Edit", file(filepath.Join(f.home, ".claude", "settings.json")))
			denied(mode, "Write the kch binary", "Write", file(f.kch))
			denied(mode, "Write in workspace/.claude", "Write", file(filepath.Join(ws, ".claude", "x")))
			denied(mode, "Write in the own workspace", "MultiEdit", file(filepath.Join(ws, "notes.md")))
			denied(mode, "NotebookEdit in the repo of another project", "NotebookEdit", map[string]any{"notebook_path": filepath.Join(beta, "n.ipynb")})
			denied(mode, "Write git's own files", "Write", file(filepath.Join(alpha, ".git", "config")))
			denied(mode, "Write a .gitignore", "Write", file(filepath.Join(alpha, "src", ".gitignore")))
			// Tools that are refused whatever they name.
			denied(mode, "an MCP tool", "mcp__openbot__send_message", map[string]any{"text": "hi"})
			denied(mode, "WebFetch", "WebFetch", map[string]any{"url": "https://example.invalid"})
			denied(mode, "WebSearch", "WebSearch", map[string]any{"query": "x"})
			denied(mode, "an unknown tool with a path", "LS", map[string]any{"path": alpha})
			denied(mode, "an unknown tool with a file_path", "FancyReader", file(filepath.Join(alpha, "src", "keep.txt")))
			// Inside the read roots.
			allowed(mode, "Read in the repo of its project", "Read", file(filepath.Join(alpha, "src", "keep.txt")))
			allowed(mode, "Read in the reference root", "Read", file(filepath.Join(f.ref, "doc.md")))
			allowed(mode, "Read in its workspace", "Read", file(filepath.Join(ws, "notes.md")))
			allowed(mode, "Read a toolchain file", "Read", file(filepath.Join(f.toolchain, "bin", "go")))
			allowed(mode, "Read its own session temp dir", "Read", file(filepath.Join(hookinstall.SessionTmp(os.Getuid(), ws), "out.txt")))
			allowed(mode, "Glob in the repo", "Glob", map[string]any{"pattern": "src/**/*.txt", "path": alpha})
			allowed(mode, "Grep in the reference root", "Grep", map[string]any{"pattern": "ref", "path": f.ref})
			allowed(mode, "Glob from the workspace cwd", "Glob", map[string]any{"pattern": "**/*.md"})
			allowed(mode, "Bash", "Bash", map[string]any{"command": "cat ~/.config/secret.txt"}) // confined by the sandbox, not by the hook
			allowed(mode, "a tool without a path", "TodoWrite", map[string]any{"todos": []any{}})
			allowed(mode, "Write in the private temp root", "Write", file(filepath.Join(tmp, "scratch.txt")))
			// The task repo, by mode.
			inScope, outOfScope := file(filepath.Join(alpha, "src", "a.go")), file(filepath.Join(alpha, "docs", "x.md"))
			switch mode {
			case "none":
				denied(mode, "Write in a governed repo without a lease", "Write", inScope)
			case "FULL":
				allowed(mode, "Write inside allowed_paths", "Write", inScope)
				denied(mode, "Write outside allowed_paths", "Edit", outOfScope)
			default:
				allowed(mode, "Write inside the task repo", "Write", inScope)
				allowed(mode, "Write anywhere in the task repo", "Edit", outOfScope)
			}
		})
	}
	// Denials are journaled with rule codes only: in the project of the lease, or as a system event without one.
	if n := f.events("/v1/projects/"+f.id["alpha"]+"/events", "TOOL_DENIED", `"rule":"read_outside"`); n < 3 {
		t.Fatalf("%d TOOL_DENIED read_outside events in the project journal", n)
	}
	if n := f.events("/v1/system/events", "TOOL_DENIED", `"rule":"tool_denied"`); n != 3 {
		t.Fatalf("%d TOOL_DENIED tool_denied system events for the calls without a lease, want 3", n)
	}
	if n := f.events("/v1/projects/"+f.id["alpha"]+"/events", "TOOL_DENIED", f.home) + f.events("/v1/system/events", "TOOL_DENIED", f.home); n != 0 {
		t.Fatal("a journal payload carries a path")
	}
}

// AC-S2-008: an EXHAUSTED SYSTEM budget stops every tool in every lease mode.
func TestAC_S2_008_SystemCeilingInEveryMode(t *testing.T) {
	f := enrolled(t)
	zero := int64(0)
	for _, mode := range []string{"OBSERVE", "CONTEXT", "FULL"} {
		t.Run("AC-S2-008 mode "+mode, func(t *testing.T) {
			f.assign(http.StatusOK, f.task("T-"+mode), "X", mode)
			defer f.release("X")
			if out := f.tool("X", "Bash", map[string]any{"command": "ls"}); out != "" {
				t.Fatalf("refused before the ceiling: %s", out)
			}
			budget := f.o(http.StatusOK, "PUT", "/v1/system/budget", map[string]any{"max_tool_calls": zero})
			if str(budget, "state") != "EXHAUSTED" {
				t.Fatalf("SYSTEM budget: %s", budget["_raw"])
			}
			defer f.o(http.StatusOK, "PUT", "/v1/system/budget", map[string]any{})
			for tool, input := range map[string]map[string]any{"Bash": {"command": "ls"}, "Read": {"file_path": filepath.Join(f.repo["alpha"], "src", "keep.txt")},
				"Write": {"file_path": filepath.Join(f.repo["alpha"], "src", "a.go")}, "TodoWrite": {}} {
				has(t, f.tool("X", tool, input), `"permissionDecision":"deny"`, "ngân sách hệ thống")
			}
			// The real hook client refuses the agent's own kch commands too.
			has(t, f.bash("X", "kch status"), `"permissionDecision":"deny"`, "ngân sách hệ thống")
		})
	}
	// Without a lease nothing is charged to the SYSTEM budget, so nothing is stopped.
	f.o(http.StatusOK, "PUT", "/v1/system/budget", map[string]any{"max_tool_calls": zero})
	if out := f.tool("X", "Bash", map[string]any{"command": "ls"}); out != "" {
		t.Fatalf("an agent without a lease was stopped by the SYSTEM ceiling: %s", out)
	}
}
