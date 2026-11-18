package hookinstall

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

func TestUninstall_AC_H_070_RemovesOnlyOurEntries(t *testing.T) {
	workspace := t.TempDir()
	path := writeSettings(t, workspace, foreign)
	before := parseFile(t, path)
	if _, _, err := Install(testSettings(workspace)); err != nil {
		t.Fatal(err)
	}
	// A hand-merged group that holds a foreign handler next to ours keeps the foreign one.
	root := parseFile(t, path)
	post := root["hooks"].(map[string]any)["PostToolUse"].([]any)[0].(map[string]any)
	post["hooks"] = append(post["hooks"].([]any), map[string]any{"type": "command", "command": "fmt-on-save"})
	data, _ := json.Marshal(root)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Uninstall(workspace, testBinary); err != nil {
		t.Fatal(err)
	}
	after := parseFile(t, path)
	if got := ourCommands(t, path, testBinary); len(got) != 0 {
		t.Fatalf("our entries remain: %v", got)
	}
	hooks := after["hooks"].(map[string]any)
	wantPost := []any{map[string]any{"matcher": "*", "hooks": []any{map[string]any{"type": "command", "command": "fmt-on-save"}}}}
	if !reflect.DeepEqual(hooks["PostToolUse"], wantPost) {
		t.Fatalf("PostToolUse = %v", hooks["PostToolUse"])
	}
	delete(hooks, "PostToolUse")
	delete(before["hooks"].(map[string]any), "Stop") // an empty array we appended to cannot be told from one we created
	if !reflect.DeepEqual(after["hooks"], before["hooks"]) || after["model"] != before["model"] {
		t.Fatalf("uninstall did not restore the foreign hooks:\n got %v\nwant %v", after["hooks"], before["hooks"])
	}
	// The sandbox stays: removing the hooks must not widen what Bash can reach.
	if sb, _ := after["sandbox"].(map[string]any); sb["enabled"] != true || sb["allowUnsandboxedCommands"] != false {
		t.Fatalf("uninstall touched the sandbox block: %v", after["sandbox"])
	}

	fresh := t.TempDir()
	if err := Uninstall(fresh, testBinary); err != nil {
		t.Fatalf("uninstall without a settings file: %v", err)
	}
	if _, err := os.Stat(SettingsPath(fresh)); !os.IsNotExist(err) {
		t.Fatal("uninstall created a settings file")
	}
	if _, _, err := Install(testSettings(fresh)); err != nil {
		t.Fatal(err)
	}
	if err := Uninstall(fresh, testBinary); err != nil {
		t.Fatal(err)
	}
	if got := parseFile(t, SettingsPath(fresh)); got["hooks"] != nil {
		t.Fatalf("after install + uninstall the file still has hooks: %v", got["hooks"])
	}
}

func TestInstall_AC_H_070_QuotesPathsForTheShell(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "agent w;id")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := "/opt/my tools/kch's $HOME"
	s := Generate(Input{Workspace: workspace, Binary: bin, Socket: "/run dir/a.sock", Enrolled: true, Home: "/home/u", TmpRoot: "/var/tmp/t"})
	cmd := s.Hooks[4].Command
	out, err := exec.Command("bash", "-c", `set -- `+cmd+`; printf '%s\n' "$@"`).Output()
	if err != nil {
		t.Fatalf("bash could not parse %q: %v", cmd, err)
	}
	want := bin + "\nhook\nstop\n--socket\n/run dir/a.sock\n--workspace\n" + workspace + "\n--enrolled\n"
	if string(out) != want {
		t.Fatalf("bash sees %q, want %q (command %q)", out, want, cmd)
	}
	path, _, err := Install(s)
	if err != nil {
		t.Fatal(err)
	}
	if got := ourCommands(t, path, bin); len(got) != len(events) {
		t.Fatalf("our quoted entries are not recognised: %v", got)
	}
	if _, _, err := Install(s); err != nil {
		t.Fatal(err)
	}
	if got := ourCommands(t, path, bin)["Stop"]; len(got) != 1 {
		t.Fatalf("quoted entries duplicated: %v", got)
	}
}

func TestInstall_AC_H_070_RefusesWhatItCannotMergeSafely(t *testing.T) {
	for name, content := range map[string]string{
		"not json":           `{"model": `,
		"array at top level": `[1,2]`,
		"hooks not object":   `{"hooks": []}`,
		"event not array":    `{"hooks": {"Stop": {"hooks": []}}}`,
		"trailing content":   `{} {}`,
		"env not object":     `{"env": []}`,
		"deny not array":     `{"permissions": {"deny": "x"}}`,
	} {
		workspace := t.TempDir()
		path := writeSettings(t, workspace, content)
		if _, _, err := Install(testSettings(workspace)); !kerr.Is(err, kerr.Invalid) {
			t.Errorf("%s: err = %v, want Invalid", name, err)
		}
		if raw, _ := os.ReadFile(path); string(raw) != content {
			t.Errorf("%s: the user's file was overwritten: %s", name, raw)
		}
	}
	workspace := t.TempDir()
	bad := testSettings(workspace)
	bad.Binary = "kch"
	if _, _, err := Install(bad); !kerr.Is(err, kerr.Invalid) {
		t.Errorf("relative binary: %v", err)
	}
	if _, _, err := Install(testSettings(filepath.Join(workspace, "missing"))); !kerr.Is(err, kerr.Invalid) {
		t.Errorf("missing workspace: %v", err)
	}
	bad = testSettings(workspace)
	bad.Sandbox.AllowRead = append(bad.Sandbox.AllowRead, "/home/u/a\nb")
	if _, _, err := Install(bad); !kerr.Is(err, kerr.Invalid) {
		t.Errorf("control character in a sandbox path: %v", err)
	}
}
