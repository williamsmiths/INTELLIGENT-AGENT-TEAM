package hookinstall

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const testBinary = "/opt/harness/bin/kch"

// testSettings generates the managed block of a workspace the way `kch enroll` does.
func testSettings(workspace string) Settings {
	return Generate(Input{Workspace: workspace, Binary: testBinary, Socket: "/home/u/.local/state/kc-harness/run/agent.sock", Enrolled: true,
		Home: "/home/u", RepoRoots: []string{"/var/tmp/kc-pilot/repos/b", "/var/tmp/kc-pilot/repos/a"}, ReferenceRoots: []string{"/home/u/Projects/ref"},
		ReadRoots: []string{"/home/u/.local/go", "/home/u/.nvm"}, TmpRoot: "/var/tmp/kc-pilot/tmp/agent-w",
		HiddenTmp: []string{"/tmp/claude-1000/-home-u-other"},
		Env:       map[string]string{"GOPROXY": "off", "GOCACHE": "/var/tmp/kc-pilot/tmp/agent-w/go-build", "TMPDIR": "/var/tmp/kc-pilot/tmp/agent-w"}})
}

// parseFile decodes a settings file the way Install does (numbers kept as written).
func parseFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		t.Fatalf("%s is not JSON: %v\n%s", path, err, raw)
	}
	return root
}

// ourCommands lists, per event, the commands in the file that Install wrote.
func ourCommands(t *testing.T, path, binary string) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	hooks, _ := parseFile(t, path)["hooks"].(map[string]any)
	for _, e := range events {
		groups, _ := hooks[e.Name].([]any)
		for _, g := range groups {
			handlers, _ := g.(map[string]any)["hooks"].([]any)
			for _, h := range handlers {
				if cmd, _ := h.(map[string]any)["command"].(string); isOurs(cmd, binary, e) {
					out[e.Name] = append(out[e.Name], cmd)
				}
			}
		}
	}
	return out
}

const foreign = `{
  "model": "opus",
  "cleanupPeriodDays": 12345678901234567890,
  "ratio": 1.50,
  "permissions": {"allow": ["Bash(go test:*)"], "deny": ["Bash(curl:*)", "Read(//stale/**)", "WebFetch"]},
  "env": {"FOREIGN": "1", "GOPROXY": "direct"},
  "sandbox": {"enabled": false, "allowUnsandboxedCommands": true},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "/usr/local/bin/audit a && b > c", "timeout": 3}]}],
    "Notification": [{"hooks": [{"type": "command", "command": "notify-send done"}]}],
    "Stop": []
  }
}
`

func writeSettings(t *testing.T, workspace, content string) string {
	t.Helper()
	path := SettingsPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInstall_AC_H_070_FreshWorkspace(t *testing.T) {
	workspace := t.TempDir()
	path, sum, err := Install(testSettings(workspace))
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(workspace, ".claude", "settings.json") {
		t.Fatalf("path = %s", path)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", info, err)
	}
	if got, err := FileHash(path); err != nil || got != sum {
		t.Fatalf("returned sha256 %s, file %s (%v): the registered hash must cover the whole file", sum, got, err)
	}
	root := parseFile(t, path)
	hooks := root["hooks"].(map[string]any)
	flags := " --socket /home/u/.local/state/kc-harness/run/agent.sock --workspace " + workspace + " --enrolled"
	for _, e := range events {
		group := hooks[e.Name].([]any)[0].(map[string]any)
		h := group["hooks"].([]any)[0].(map[string]any)
		if h["command"] != testBinary+" hook "+e.Arg+flags || h["type"] != "command" || h["timeout"].(json.Number).String() != map[bool]string{true: "900", false: "15"}[e.Arg == "pre-tool"] {
			t.Errorf("%s: handler = %v", e.Name, h)
		}
		if _, has := group["matcher"]; has != e.Matcher {
			t.Errorf("%s: matcher = %v", e.Name, group["matcher"])
		}
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left behind: %v", entries)
	}
}

func TestInstall_AC_H_070_MergeKeepsForeignHooksAndUnrelatedKeys(t *testing.T) {
	workspace := t.TempDir()
	path := writeSettings(t, workspace, foreign)
	before := parseFile(t, path)
	if _, _, err := Install(testSettings(workspace)); err != nil {
		t.Fatal(err)
	}
	after := parseFile(t, path)
	for _, key := range []string{"model", "cleanupPeriodDays", "ratio"} {
		if !reflect.DeepEqual(after[key], before[key]) {
			t.Errorf("key %s changed: %v -> %v", key, before[key], after[key])
		}
	}
	perms := after["permissions"].(map[string]any)
	if !reflect.DeepEqual(perms["allow"], before["permissions"].(map[string]any)["allow"]) {
		t.Errorf("permissions.allow changed: %v", perms["allow"])
	}
	if deny := perms["deny"].([]any); deny[0] != "Bash(curl:*)" || len(deny) != 6 {
		t.Errorf("the foreign deny rule was not kept first: %v", deny)
	}
	raw, _ := os.ReadFile(path)
	for _, literal := range []string{"12345678901234567890", "1.50", "/usr/local/bin/audit a && b > c", `"FOREIGN": "1"`} {
		if !strings.Contains(string(raw), literal) {
			t.Errorf("%q was rewritten:\n%s", literal, raw)
		}
	}
	hooksBefore, hooksAfter := before["hooks"].(map[string]any), after["hooks"].(map[string]any)
	if !reflect.DeepEqual(hooksAfter["Notification"], hooksBefore["Notification"]) {
		t.Error("a foreign event changed")
	}
	pre := hooksAfter["PreToolUse"].([]any)
	if len(pre) != 2 || !reflect.DeepEqual(pre[0], hooksBefore["PreToolUse"].([]any)[0]) {
		t.Fatalf("the foreign PreToolUse hook was not kept first and intact: %v", pre)
	}
	for _, e := range events {
		if got := ourCommands(t, path, testBinary)[e.Name]; len(got) != 1 {
			t.Errorf("%s: our commands = %v", e.Name, got)
		}
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after merge = %v", info.Mode().Perm())
	}
}

func TestInstall_AC_S2_011_Idempotent(t *testing.T) {
	workspace := t.TempDir()
	path := writeSettings(t, workspace, foreign)
	_, sum1, err := Install(testSettings(workspace))
	if err != nil {
		t.Fatal(err)
	}
	first, _ := os.ReadFile(path)
	_, sum2, err := Install(testSettings(workspace))
	if err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if sum1 != sum2 || string(first) != string(second) {
		t.Fatalf("second install changed the file:\n%s\n---\n%s", first, second)
	}
	// Enrolling again with other data replaces the managed block instead of adding a second one.
	other := testSettings(workspace)
	other.Sandbox.AllowWrite = []string{"/var/tmp/kc-pilot/repos/c"}
	other.Deny[0] = "Read(//home/v/**)"
	other.Hooks[2].Command = testBinary + " hook pre-tool --socket /other.sock --workspace " + workspace
	if _, _, err := Install(other); err != nil {
		t.Fatal(err)
	}
	root := parseFile(t, path)
	if got := ourCommands(t, path, testBinary); len(got["PreToolUse"]) != 1 || !strings.Contains(got["PreToolUse"][0], "/other.sock") {
		t.Errorf("our PreToolUse after re-enroll = %v", got["PreToolUse"])
	}
	fs := root["sandbox"].(map[string]any)["filesystem"].(map[string]any)
	if !reflect.DeepEqual(fs["allowWrite"], []any{"/var/tmp/kc-pilot/repos/c"}) {
		t.Errorf("allowWrite after re-enroll = %v", fs["allowWrite"])
	}
	deny := root["permissions"].(map[string]any)["deny"].([]any)
	if len(deny) != 6 || deny[1] != "Read(//home/v/**)" {
		t.Errorf("deny after re-enroll = %v", deny)
	}
}
