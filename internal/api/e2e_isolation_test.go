package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/attest"
	"github.com/williamsmiths/kongcowork-harness/internal/attest/attesttest"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
)

// Spec S2 part A end to end. Nothing of the kernel is mocked: the server
// listens on two real unix sockets with process attestation, every agent call
// comes from a real process tree (host -> provider in the agent workspace ->
// client), and the hook client is the real kch binary run from the command
// line that the real `kch enroll` wrote.
func TestMain(m *testing.M) { attesttest.Main(m) }

type iso struct {
	t                           *testing.T
	pool                        *pgxpool.Pool
	dir, home, agents           string
	ownerSock, agentSock        string
	host, provider, bwrap, kch  string
	id, repo                    map[string]string
	ref, toolchain, ptmp, other string
}

func newIso(t *testing.T) *iso {
	t.Helper()
	pool := testdb.New(t)
	dir := attesttest.ShortDir(t)
	f := &iso{t: t, pool: pool, dir: dir, home: filepath.Join(dir, "h"), id: map[string]string{}, repo: map[string]string{},
		ref: filepath.Join(dir, "ref"), toolchain: filepath.Join(dir, "toolchain"), ptmp: filepath.Join(dir, "ptmp")}
	f.agents = filepath.Join(f.home, "OpenBot", "Agents")
	f.ownerSock, f.agentSock = hookinstall.OwnerSocket(f.home), hookinstall.AgentSocket(f.home)
	f.other = filepath.Join(hookinstall.SessionTmpBase(os.Getuid()), "-other-session", "x.txt")
	for _, d := range []string{filepath.Dir(f.ownerSock), filepath.Dir(f.agentSock), f.ref, f.toolchain, f.ptmp, filepath.Join(dir, "sbx"),
		f.ws("X"), f.ws("Y"), f.ws("U"), filepath.Join(f.home, ".claude")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	e := &env{t: t}
	e.write(filepath.Join(f.home, ".config", "secret.txt"), "secret\n")
	e.write(filepath.Join(f.ref, "doc.md"), "reference\n")
	e.write(filepath.Join(f.ws("X"), "notes.md"), "notes\n")
	for _, name := range []string{"alpha", "beta"} {
		f.repo[name] = e.gitRepo(filepath.Join(dir, "repos", name))
	}
	f.host, f.provider = attesttest.Copy(t, dir, "openbot"), attesttest.Copy(t, dir, "claude")
	f.bwrap = attesttest.Copy(t, filepath.Join(dir, "sbx"), "bwrap")
	f.kch = filepath.Join(dir, "kch")
	build, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(build, "go", "build", "-o", f.kch, "github.com/williamsmiths/kongcowork-harness/cmd/kch").CombinedOutput(); err != nil {
		t.Fatalf("build kch: %v\n%s", err, out)
	}
	machine, _ := json.Marshal(map[string]any{"openbot_exe": f.host, "agents_root": f.agents, "sandbox_read_roots": []string{f.toolchain},
		"pilot_tmp_root": f.ptmp, "sandbox_env": map[string]string{"GOMODCACHE": "/x/mod"}})
	e.write(hookinstall.MachinePath(f.home), string(machine))
	if err := store.InScope(context.Background(), pool, store.Runtime, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO principal (kind,name,token_hash) VALUES ('owner','owner','x')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Pool: pool, Runner: verify.NewRunner(), LogDir: filepath.Join(dir, "state", "evidence"), AgentsRoot: f.agents,
		PacketChars: 6000, ReadRoots: []string{f.toolchain}, PilotTmpRoot: f.ptmp}
	cfg := attest.Config{OpenBotExe: f.host, AgentsRoot: f.agents, SelfExe: attest.Self()} // this test process plays harnessd
	for sock, h := range map[string]http.Handler{
		f.ownerSock: srv.OwnerRoutes(&api.PeerAuth{Pool: pool, Attest: cfg, Socket: api.SocketOwner}),
		f.agentSock: srv.AgentRoutes(&api.PeerAuth{Pool: pool, Attest: cfg, Socket: api.SocketAgent})} {
		ln, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		hs := &http.Server{Handler: h, ConnContext: api.ConnContext, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = hs.Serve(ln) }()
		t.Cleanup(func() { _ = hs.Close() })
	}
	return f
}

func (f *iso) ws(agent string) string { return filepath.Join(f.agents, "agent-"+agent) }

// o calls the Owner API from this process: an unsandboxed process of the OS
// user that is neither an enrolled agent nor a child of harnessd (PM-S2-01).
func (f *iso) o(want int, method, path string, body any) map[string]any {
	f.t.Helper()
	client := &http.Client{Timeout: 2 * time.Minute, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", f.ownerSock)
		}}}
	var rd io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, "http://harness"+path, rd)
	resp, err := client.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{"_raw": string(raw)}
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode != want {
		f.t.Fatalf("%s %s: status %d, want %d: %s", method, path, resp.StatusCode, want, raw)
	}
	return out
}

// tree is the real process tree of an agent: host -> provider in its workspace.
func (f *iso) tree(agent string, more ...attesttest.Step) []attesttest.Step {
	return append([]attesttest.Step{{Exe: f.host, Dir: f.dir}, {Exe: f.provider, Dir: f.ws(agent)}}, more...)
}

// freeTree is the agent's tree with a host that is no descendant of this test
// process (which plays harnessd): a launcher starts the host and exits.
func (f *iso) freeTree(agent string, more ...attesttest.Step) []attesttest.Step {
	return append([]attesttest.Step{{Exe: f.provider, Dir: f.dir}, {Exe: f.host, Dir: f.dir, Orphan: true}, {Exe: f.provider, Dir: f.ws(agent)}}, more...)
}

// detached is a process of the same OS user outside every tree: its parent exited.
func (f *iso) detached() []attesttest.Step {
	return []attesttest.Step{{Exe: f.provider, Dir: f.dir, Env: []string{"HOME=" + f.home}}, {Orphan: true}}
}

// from sends one request from the last process of a real tree.
func (f *iso) from(steps []attesttest.Step, sock, method, path string, body any, header ...string) (int, map[string]any) {
	f.t.Helper()
	p := attesttest.Plan{Steps: steps, Socket: sock, Method: method, Path: path, Header: map[string]string{}}
	if body != nil {
		raw, _ := json.Marshal(body)
		p.Body = string(raw)
	}
	for i := 0; i+1 < len(header); i += 2 {
		p.Header[header[i]] = header[i+1]
	}
	a := attesttest.Run(f.t, p)
	if a.Status == 0 {
		f.t.Fatalf("%s %s: no answer: %+v", method, path, a)
	}
	out := map[string]any{"_raw": a.Body}
	_ = json.Unmarshal([]byte(a.Body), &out)
	return a.Status, out
}

// hook posts a hook event from the agent's tree the way the hook client does:
// with the effective config of the workspace. It returns the decision ("" = none).
func (f *iso) hook(agent, event string, body map[string]any) string {
	f.t.Helper()
	sent := map[string]any{"settings_sha256": hookinstall.Current(hookinstall.SettingsPath(f.ws(agent))),
		"extra_config": hookinstall.ExtraConfig(f.ws(agent)), "cwd": f.ws(agent)}
	for k, v := range body {
		sent[k] = v
	}
	status, out := f.from(f.tree(agent), f.agentSock, "POST", "/v1/hooks/claude/"+event, sent)
	if status != http.StatusOK {
		f.t.Fatalf("hook %s: %d %s", event, status, out["_raw"])
	}
	if out["output"] == nil {
		return ""
	}
	raw, _ := json.Marshal(out["output"])
	return string(raw)
}

func (f *iso) tool(agent, name string, input map[string]any) string {
	return f.hook(agent, "pre-tool", map[string]any{"tool_name": name, "tool_input": input})
}

// kchHook runs the real hook client exactly as Claude Code does: `sh -c` of
// the command line `kch enroll` wrote into the settings file, as a child of
// the provider. A failed hook command fails the test.
func (f *iso) kchHook(agent, event string, input map[string]any, envs ...string) string {
	f.t.Helper()
	raw, err := os.ReadFile(hookinstall.SettingsPath(f.ws(agent)))
	if err != nil {
		f.t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		f.t.Fatal(err)
	}
	name := map[string]string{"pre-tool": "PreToolUse", "prompt-submit": "UserPromptSubmit", "post-tool": "PostToolUse"}[event]
	return f.kchRun(agent, settings.Hooks[name][0].Hooks[0].Command, input, envs...)
}

func (f *iso) kchRun(agent, command string, input map[string]any, envs ...string) string {
	f.t.Helper()
	stdin, _ := json.Marshal(input)
	steps := f.tree(agent)
	steps[0].Env = append([]string{"HOME=" + f.home}, envs...)
	a := attesttest.Run(f.t, attesttest.Plan{Steps: steps, Exec: []string{"sh", "-c", command}, Stdin: string(stdin)})
	if a.Exit != 0 || a.Error != "" {
		f.t.Fatalf("hook command failed: %+v", a)
	}
	return a.Body
}

// bash is a Bash tool call as the provider reports it to the pre-tool hook.
func (f *iso) bash(agent, command string, envs ...string) string {
	return f.kchHook(agent, "pre-tool", map[string]any{"tool_name": "Bash", "cwd": f.repo["alpha"], "tool_input": map[string]any{"command": command}}, envs...)
}

// printed runs the command a pre-tool answer put in place of the agent's and returns what it prints.
func (f *iso) printed(answer string, wantExit int) string {
	f.t.Helper()
	var out struct {
		H struct {
			PermissionDecision string                   `json:"permissionDecision"`
			UpdatedInput       struct{ Command string } `json:"updatedInput"`
		} `json:"hookSpecificOutput"`
	}
	if json.Unmarshal([]byte(answer), &out) != nil || out.H.PermissionDecision != "allow" || !strings.HasPrefix(out.H.UpdatedInput.Command, "cat <<'KCH_") {
		f.t.Fatalf("not a rewritten command: %s", answer)
	}
	cmd := exec.Command("bash", "-c", out.H.UpdatedInput.Command)
	raw, err := cmd.Output()
	if exit := cmd.ProcessState.ExitCode(); exit != wantExit {
		f.t.Fatalf("rewritten command exit %d (%v), want %d: %s", exit, err, wantExit, out.H.UpdatedInput.Command)
	}
	return string(raw)
}

// kchDirect runs kch as a child of the provider without any hook: what happens
// to a kch command the hook did not answer (a compound command in the sandbox).
func (f *iso) kchDirect(args ...string) attesttest.Answer {
	steps := f.tree("X")
	steps[0].Env = []string{"HOME=" + f.home}
	return attesttest.Run(f.t, attesttest.Plan{Steps: steps, Exec: append([]string{f.kch}, args...)})
}

// enroll runs the real `kch enroll` as a detached process of the OS user.
func (f *iso) enroll(principal, workspace string) attesttest.Answer {
	return attesttest.Run(f.t, attesttest.Plan{Steps: f.detached(), Exec: []string{f.kch, "enroll", principal, workspace}})
}

func (f *iso) seed() {
	f.t.Helper()
	one := int64(1_000_000)
	for _, name := range []string{"alpha", "beta"} {
		p := f.o(http.StatusCreated, "POST", "/v1/projects", map[string]any{"slug": name, "name": name})
		f.id[name] = str(p, "id")
		f.o(http.StatusOK, "PUT", "/v1/projects/"+f.id[name]+"/limits", map[string]any{
			"default_task_max_cost_microusd": one, "budget": map[string]any{"max_cost_microusd": 5 * one}})
		f.o(http.StatusCreated, "POST", "/v1/projects/"+f.id[name]+"/repos", map[string]any{"path": f.repo[name]})
	}
	for _, name := range []string{"X", "Y"} {
		out := f.o(http.StatusCreated, "POST", "/v1/principals", map[string]any{"name": name, "openbot_agent_id": "agent-" + name,
			"enforcement": "HOOK", "project_id": f.id["alpha"], "role": "worker"})
		f.id[name] = str(out, "principal", "id")
	}
	f.o(http.StatusOK, "PUT", "/v1/projects/"+f.id["alpha"]+"/reference-roots", map[string]any{"paths": []string{f.ref}})
	f.o(http.StatusOK, "PUT", "/v1/principals/"+f.id["X"], map[string]any{"specialty": "engineering"})
}

// task creates a RUNNABLE task in the alpha repo whose check passes on the base tree.
func (f *iso) task(key string, requirements ...string) string {
	f.t.Helper()
	t := f.o(http.StatusCreated, "POST", "/v1/projects/"+f.id["alpha"]+"/tasks", map[string]any{"key": key, "title": "Việc " + key,
		"desired_outcome": "xong", "scope": "feature:x", "repo_path": f.repo["alpha"], "allowed_paths": []string{"src/**"},
		"requirement_keys": requirements, "required_checks": []map[string]any{{"id": "unit", "command": "test -f src/keep.txt"}}})
	f.o(http.StatusOK, "POST", "/v1/tasks/"+str(t, "id")+"/ready", nil)
	return str(t, "id")
}

func (f *iso) assign(want int, task, agent, mode string) map[string]any {
	return f.o(want, "POST", "/v1/tasks/"+task+"/assign", map[string]any{"principal": f.id[agent], "mode": mode})
}

func (f *iso) release(agent string) {
	f.t.Helper()
	if status, out := f.from(f.tree(agent), f.agentSock, "POST", "/v1/agent/release", nil); status != http.StatusOK {
		f.t.Fatalf("release: %d %s", status, out["_raw"])
	}
}

// events counts the journal events of a type whose payload contains a text.
// path is "/v1/system/events" or "/v1/projects/<id>/events".
func (f *iso) events(path, typ, contains string) int {
	f.t.Helper()
	n := 0
	events, _ := f.o(http.StatusOK, "GET", path+"?limit=500", nil)["events"].([]any)
	for _, e := range events {
		raw, _ := json.Marshal(e)
		if strings.Contains(string(raw), `"`+typ+`"`) && strings.Contains(string(raw), contains) {
			n++
		}
	}
	return n
}

// enrolled is a fixture with the projects seeded and agent X enrolled by the real kch.
func enrolled(t *testing.T) *iso {
	t.Helper()
	f := newIso(t)
	f.seed()
	if a := f.enroll(f.id["X"], f.ws("X")); a.Exit != 0 {
		t.Fatalf("kch enroll: %+v", a)
	}
	return f
}

func TestAC_S2_011_EnrollGeneratesRegistersAndGatesFullAssignment(t *testing.T) {
	f := newIso(t)
	f.seed()
	task := f.task("T-1")
	path := hookinstall.SettingsPath(f.ws("X"))

	// Not enrolled yet: no FULL lease (the promise of enforcement would be empty).
	has(t, f.assign(http.StatusConflict, task, "X", "FULL")["_raw"].(string), "kch enroll")
	if a := f.enroll(f.id["X"], f.ws("Y")); a.Exit == 0 {
		t.Fatal("enrolled a principal into the workspace of another agent")
	}
	if a := f.enroll(f.id["X"], f.ws("X")); a.Exit != 0 {
		t.Fatalf("kch enroll: %+v", a)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string
				Timeout int
			}
		}
		Sandbox struct {
			Enabled                  bool
			AllowUnsandboxedCommands *bool
			Filesystem               struct{ DenyRead, AllowRead, AllowWrite, DenyWrite []string }
		}
		Env         map[string]string
		Permissions struct{ Deny []string }
	}
	if err := json.Unmarshal(first, &got); err != nil {
		t.Fatal(err)
	}
	fs, tmp := got.Sandbox.Filesystem, filepath.Join(f.ptmp, "agent-X")
	list := func(v []string) string { return "\n" + strings.Join(v, "\n") + "\n" }
	has(t, list(fs.DenyRead), "\n"+f.home+"\n", "\n/run\n", "\n/var/run\n")
	has(t, list(fs.AllowRead), "\n"+f.ws("X")+"\n", "\n"+f.repo["alpha"]+"\n", "\n"+f.ref+"\n", "\n"+f.toolchain+"\n", "/.claude/shell-snapshots\n")
	lacks(t, list(fs.AllowRead), f.repo["beta"])
	if list(fs.AllowWrite) != list([]string{tmp, f.repo["alpha"]}) { // the repo of its project and its private temp root, nothing else
		t.Fatalf("allowWrite = %v", fs.AllowWrite)
	}
	has(t, list(fs.DenyWrite), "\n"+f.repo["alpha"]+"/.git\n", "\n"+f.ws("X")+"/.claude\n", "\n"+f.ws("X")+"/.mcp.json\n")
	has(t, list(got.Permissions.Deny), "\nRead(/"+f.home+"/**)\n", "\nEdit(/"+f.ws("X")+"/.claude/**)\n", "\nWebFetch\n", "\nWebSearch\n", "\nmcp__*\n")
	if !got.Sandbox.Enabled || got.Sandbox.AllowUnsandboxedCommands == nil || *got.Sandbox.AllowUnsandboxedCommands ||
		got.Env["TMPDIR"] != tmp || got.Env["GOCACHE"] != tmp+"/go-build" || got.Env["GOPROXY"] != "off" || got.Env["GOMODCACHE"] != "/x/mod" {
		t.Fatalf("sandbox %+v env %v", got.Sandbox, got.Env)
	}
	pre := got.Hooks["PreToolUse"][0].Hooks[0]
	has(t, pre.Command, f.kch+" hook pre-tool --socket "+f.agentSock+" --workspace "+f.ws("X")+" --enrolled")
	if pre.Timeout != 900 || got.Hooks["Stop"][0].Hooks[0].Timeout != 15 {
		t.Fatalf("hook timeouts: %d, %d", pre.Timeout, got.Hooks["Stop"][0].Hooks[0].Timeout)
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode %v", st.Mode().Perm())
	}
	if st, err := os.Stat(tmp); err != nil || !st.IsDir() {
		t.Fatalf("private temp root not created: %v", err)
	}
	registered := func() (sum string, tampered bool) {
		t.Helper()
		if err := store.InScope(context.Background(), f.pool, store.Runtime, func(tx pgx.Tx) error {
			return tx.QueryRow(context.Background(), `SELECT sha256, tampered FROM hook_install WHERE principal_id = $1`, f.id["X"]).Scan(&sum, &tampered)
		}); err != nil {
			t.Fatal(err)
		}
		return sum, tampered
	}
	if sum, tampered := registered(); sum != hookinstall.Current(path) || tampered {
		t.Fatalf("registered hash %s, file %s, tampered %v: the hash must cover the whole file", sum, hookinstall.Current(path), tampered)
	}
	// Idempotent: enrolling again changes nothing.
	if a := f.enroll(f.id["X"], f.ws("X")); a.Exit != 0 {
		t.Fatalf("second enroll: %+v", a)
	}
	if second, _ := os.ReadFile(path); string(second) != string(first) {
		t.Fatalf("second enroll changed the file:\n%s\n---\n%s", first, second)
	}

	has(t, f.assign(http.StatusConflict, task, "Y", "FULL")["_raw"].(string), "kch enroll") // Y never enrolled
	f.assign(http.StatusOK, task, "X", "FULL")
	f.release("X")

	// A tampered install blocks a FULL assignment until it is enrolled again.
	if err := os.WriteFile(path, append(first, ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	has(t, f.tool("X", "Bash", map[string]any{"command": "ls"}), "deny", "không khớp")
	has(t, f.assign(http.StatusConflict, task, "X", "FULL")["_raw"].(string), "tampered")
	f.assign(http.StatusOK, task, "X", "OBSERVE") // no enforcement promised, none needed
	f.release("X")
	if a := f.enroll(f.id["X"], f.ws("X")); a.Exit != 0 {
		t.Fatalf("re-enroll: %+v", a)
	}
	f.assign(http.StatusOK, task, "X", "FULL")
}

func TestAC_S2_005_FailClosedAndConfigTampering(t *testing.T) {
	f := enrolled(t)
	path := hookinstall.SettingsPath(f.ws("X"))
	ls := map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": "ls"}}
	if out := f.kchHook("X", "pre-tool", ls); out != "" {
		t.Fatalf("an intact workspace was refused: %s", out)
	}

	t.Run("harnessd is not reachable", func(t *testing.T) {
		dead := filepath.Join(f.dir, "dead.sock")
		flags := " --socket " + dead + " --workspace " + f.ws("X")
		has(t, f.kchRun("X", f.kch+" hook pre-tool"+flags+" --enrolled", ls), `"permissionDecision":"deny"`, "harness không sẵn sàng")
		if out := f.kchRun("X", f.kch+" hook prompt-submit"+flags+" --enrolled", map[string]any{}); out != "" {
			t.Fatalf("prompt-submit was blocked: %s", out)
		}
		if out := f.kchRun("X", f.kch+" hook pre-tool"+flags, ls); out != "" { // not enrolled: observed only, fail-open
			t.Fatalf("a workspace that is not enrolled was refused: %s", out)
		}
		log, err := os.ReadFile(hookinstall.HookLog(f.home))
		if err != nil || strings.Count(string(log), "\n") != 3 || strings.Count(string(log), `"closed":true`) != 1 {
			t.Fatalf("local hook log: %v\n%s", err, log)
		}
		has(t, string(log), `"agent":"agent-X","event":"pre-tool","closed":true`, `"event":"prompt-submit"`)
	})

	t.Run("KCH_SOCKET and KCH_AGENT in the environment are ignored", func(t *testing.T) {
		out := f.bash("X", "kch status", "KCH_SOCKET="+filepath.Join(f.dir, "dead.sock"), "KCH_AGENT=agent-Y", "KCH_URL=http://127.0.0.1:1", "KCH_CONFIG_DIR=/nowhere")
		has(t, f.printed(out, 0), "Agent: X (agent-X)")
	})

	t.Run("a changed settings file, settings.local.json and .mcp.json", func(t *testing.T) {
		orig, _ := os.ReadFile(path)
		tampered := func() int { return f.events("/v1/system/events", "HOOK_TAMPERED", f.id["X"]) }
		steps := []struct {
			name   string
			change func()
			undo   func()
		}{
			{"settings.json changed", func() { _ = os.WriteFile(path, append(orig, '\n'), 0o600) }, func() { _ = os.WriteFile(path, orig, 0o600) }},
			{"settings.local.json", func() {
				_ = os.WriteFile(filepath.Join(f.ws("X"), ".claude", "settings.local.json"), []byte("{}"), 0o600)
			},
				func() { _ = os.Remove(filepath.Join(f.ws("X"), ".claude", "settings.local.json")) }},
			{".mcp.json", func() { _ = os.WriteFile(filepath.Join(f.ws("X"), ".mcp.json"), []byte("{}"), 0o600) },
				func() { _ = os.Remove(filepath.Join(f.ws("X"), ".mcp.json")) }},
		}
		for i, s := range steps {
			s.change()
			for range 2 { // every pre-tool is refused; the journal gets one event per distinct state
				has(t, f.kchHook("X", "pre-tool", ls), `"permissionDecision":"deny"`, "không khớp bản đã đăng ký")
			}
			has(t, f.kchHook("X", "pre-tool", map[string]any{"tool_name": "Read", "tool_input": map[string]any{"file_path": f.ws("X") + "/notes.md"}}), "deny")
			if out := f.kchHook("X", "prompt-submit", map[string]any{}); out != "" {
				t.Fatalf("%s: prompt-submit was blocked: %s", s.name, out)
			}
			if n := tampered(); n != i+1 {
				t.Fatalf("%s: %d HOOK_TAMPERED events, want %d", s.name, n, i+1)
			}
			s.undo()
			if out := f.kchHook("X", "pre-tool", ls); out != "" {
				t.Fatalf("%s: still refused after the restore: %s", s.name, out)
			}
		}
	})

	t.Run("a principal whose workspace was never enrolled", func(t *testing.T) {
		has(t, f.tool("Y", "Bash", map[string]any{"command": "ls"}), "deny", "unregistered")
		if out := f.hook("Y", "prompt-submit", nil); out != "" {
			t.Fatalf("prompt-submit of an unregistered workspace: %s", out)
		}
	})
}
