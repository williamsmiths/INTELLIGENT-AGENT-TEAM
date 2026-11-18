package hookinstall

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// AC-S2-011: the sandbox is generated from data exactly as spec S2 A.1 lists it.
func TestGenerate_AC_S2_011_SandboxFromData(t *testing.T) {
	ws := t.TempDir()
	s := testSettings(ws)
	if again := testSettings(ws); !reflect.DeepEqual(s, again) {
		t.Fatal("Generate is not deterministic")
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	want := Sandbox{
		DenyRead:   []string{"/home/u", "/run", "/var/run", "/tmp/claude-1000/-home-u-other"},
		AllowRead:  uniq([]string{ws, "/home/u/.claude/shell-snapshots", "/home/u/.local/go", "/home/u/.nvm", "/var/tmp/kc-pilot/repos/a", "/var/tmp/kc-pilot/repos/b", "/home/u/Projects/ref"}),
		AllowWrite: []string{"/var/tmp/kc-pilot/repos/a", "/var/tmp/kc-pilot/repos/b", "/var/tmp/kc-pilot/tmp/agent-w"},
		DenyWrite:  []string{"/var/tmp/kc-pilot/repos/a/.git", "/var/tmp/kc-pilot/repos/b/.git", ws + "/.claude", ws + "/.mcp.json"},
	}
	if !reflect.DeepEqual(sb, want) {
		t.Fatalf("sandbox =\n%+v\nwant\n%+v", sb, want)
	}
	if !reflect.DeepEqual(s.Deny, []string{"Read(//home/u/**)", "Edit(/" + ws + "/.claude/**)", "WebFetch", "WebSearch", "mcp__*"}) {
		t.Fatalf("permissions.deny = %v", s.Deny)
	}
	if s.Env["GOPROXY"] != "off" || s.Env["TMPDIR"] != "/var/tmp/kc-pilot/tmp/agent-w" || len(s.Env) != 3 {
		t.Fatalf("env = %v", s.Env)
	}
	// Consistency of the lists: a write area is never hidden, the reference root
	// and the toolchain are readable but not writable, git's files are not writable.
	for _, w := range sb.AllowWrite {
		for _, d := range sb.DenyRead {
			if guard.Inside(d, w) {
				t.Errorf("write area %s is inside the hidden %s", w, d)
			}
		}
		if guard.Inside("/home/u", w) {
			t.Errorf("write area %s is inside the home directory", w)
		}
	}
	for _, r := range []string{"/home/u/Projects/ref", "/home/u/.nvm", ws} {
		for _, w := range sb.AllowWrite {
			if guard.Inside(w, r) {
				t.Errorf("read-only root %s is writable through %s", r, w)
			}
		}
	}
	for _, h := range s.Hooks {
		if !strings.HasSuffix(h.Command, " --enrolled") || !strings.Contains(h.Command, " --socket ") {
			t.Errorf("hook command %q", h.Command)
		}
	}
}

func TestGenerate_AC_S2_011_RefusesAWriteAreaInsideTheHiddenHome(t *testing.T) {
	ws := t.TempDir()
	in := Input{Workspace: ws, Binary: testBinary, Socket: "/s/agent.sock", Home: "/home/u", TmpRoot: "/var/tmp/t",
		RepoRoots: []string{"/home/u/Projects/app"}}
	if _, _, err := Install(Generate(in)); !kerr.Is(err, kerr.Invalid) || !strings.Contains(err.Error(), "/home/u/Projects/app") {
		t.Fatalf("a repo root inside the home was accepted: %v", err)
	}
	in.RepoRoots, in.TmpRoot = nil, "/home/u/tmp"
	if err := Generate(in).Validate(); !kerr.Is(err, kerr.Invalid) {
		t.Fatalf("a temp root inside the home was accepted: %v", err)
	}
	in.TmpRoot = "relative/tmp"
	if err := Generate(in).Validate(); !kerr.Is(err, kerr.Invalid) {
		t.Fatalf("a relative path was accepted: %v", err)
	}
}

func TestLoadMachine(t *testing.T) {
	dir := t.TempDir()
	write := func(content string) string {
		t.Helper()
		path := dir + "/machine.json"
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	m, err := LoadMachine(write(`{"openbot_exe":"/opt/openbot/openbot","sandbox_read_roots":["~/.nvm","/opt/go"],"sandbox_env":{"GOMODCACHE":"/m"}}`), "/home/u")
	if err != nil {
		t.Fatal(err)
	}
	if m.OpenBotExe != "/opt/openbot/openbot" || m.AgentsRoot != "/home/u/OpenBot/Agents" || m.PilotTmpRoot != "/var/tmp/kc-pilot/tmp" ||
		!reflect.DeepEqual(m.SandboxReadRoots, []string{"/home/u/.nvm", "/opt/go"}) || m.SandboxEnv["GOMODCACHE"] != "/m" {
		t.Fatalf("machine = %+v", m)
	}
	for name, content := range map[string]string{"no openbot_exe": `{"agents_root":"/a"}`, "relative": `{"openbot_exe":"openbot"}`, "not json": `{`} {
		if _, err := LoadMachine(write(content), "/home/u"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := LoadMachine(dir+"/absent.json", "/home/u"); err == nil {
		t.Error("a missing machine config was accepted")
	}
	if got := SessionTmp(1000, "/home/u/OpenBot/Agents/agent-1"); got != "/tmp/claude-1000/-home-u-OpenBot-Agents-agent-1" {
		t.Errorf("SessionTmp = %s", got)
	}
}
