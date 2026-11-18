package hookinstall

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Machine is ~/.config/kc-harness/machine.json: what differs from one machine
// to the next. harnessd and `kch enroll` read it; nothing in it is a secret.
type Machine struct {
	OpenBotExe       string            `json:"openbot_exe"`        // executable of the host app; required: attestation is built on it
	AgentsRoot       string            `json:"agents_root"`        // default ~/OpenBot/Agents
	SandboxReadRoots []string          `json:"sandbox_read_roots"` // toolchain directories an agent may read
	PilotTmpRoot     string            `json:"pilot_tmp_root"`     // "<pilot_tmp_root>/<agent id>" is the private temp root of an agent
	SandboxEnv       map[string]string `json:"sandbox_env"`        // optional extra environment of the agent's Bash, e.g. GOMODCACHE
}

// MachinePath is the machine config file below a home directory.
func MachinePath(home string) string {
	return filepath.Join(configDir(home), "machine.json")
}

// LoadMachine reads and checks the machine config. "~/" at the start of a
// path means the home directory. Paths are made canonical so they compare
// equal to what the kernel reports.
func LoadMachine(path, home string) (Machine, error) {
	var m Machine
	raw, err := os.ReadFile(path)
	if err != nil {
		return m, fmt.Errorf("machine config: %w", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, fmt.Errorf("machine config %s: %w", path, err)
	}
	abs := func(name, p string) (string, error) {
		if rest, ok := strings.CutPrefix(p, "~/"); ok {
			p = filepath.Join(home, rest)
		}
		if !filepath.IsAbs(p) || hasControl(p) {
			return "", fmt.Errorf("machine config %s: %s must be an absolute path", path, name)
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			return real, nil
		}
		return filepath.Clean(p), nil
	}
	if m.OpenBotExe == "" {
		return m, fmt.Errorf("machine config %s: openbot_exe is required", path)
	}
	if m.OpenBotExe, err = abs("openbot_exe", m.OpenBotExe); err != nil {
		return m, err
	}
	if m.AgentsRoot == "" {
		m.AgentsRoot = filepath.Join(home, "OpenBot", "Agents")
	}
	if m.AgentsRoot, err = abs("agents_root", m.AgentsRoot); err != nil {
		return m, err
	}
	if m.PilotTmpRoot == "" {
		m.PilotTmpRoot = "/var/tmp/kc-pilot/tmp"
	}
	if m.PilotTmpRoot, err = abs("pilot_tmp_root", m.PilotTmpRoot); err != nil {
		return m, err
	}
	for i, r := range m.SandboxReadRoots {
		if m.SandboxReadRoots[i], err = abs("sandbox_read_roots", r); err != nil {
			return m, err
		}
	}
	return m, nil
}

// SessionTmp is the temp directory Claude Code gives the session of a
// workspace: /tmp/claude-<uid>/<workspace path with "/" replaced by "-">.
func SessionTmp(uid int, workspace string) string {
	return filepath.Join(SessionTmpBase(uid), strings.ReplaceAll(workspace, "/", "-"))
}

// SessionTmpBase is the temp directory all Claude Code sessions of a user share (limitation L11).
func SessionTmpBase(uid int) string {
	return fmt.Sprintf("/tmp/claude-%d", uid)
}

// OtherSessionTmp lists the session temp directories that exist now and do
// not belong to this workspace: they are hidden from its sandbox.
func OtherSessionTmp(uid int, workspace string) []string {
	own := SessionTmp(uid, workspace)
	entries, err := os.ReadDir(SessionTmpBase(uid))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		p := filepath.Join(SessionTmpBase(uid), e.Name())
		if e.IsDir() && strings.HasPrefix(e.Name(), "-") && p != own && !hasControl(p) {
			out = append(out, p)
		}
	}
	return out
}

// Fixed locations below the home directory of the OS user (spec S2 A.1). The
// config directory holds what an agent must never read and is hidden from
// every sandbox; the state directory holds the agent socket and the logs.
func configDir(home string) string { return filepath.Join(home, ".config", "kc-harness") }

// StateDir holds evidence logs, the trusted base store and the hook log.
func StateDir(home string) string { return filepath.Join(home, ".local", "state", "kc-harness") }

// OwnerSocket is the socket of the Owner API.
func OwnerSocket(home string) string { return filepath.Join(configDir(home), "run", "owner.sock") }

// AgentSocket is the socket `kch hook` talks to.
func AgentSocket(home string) string { return filepath.Join(StateDir(home), "run", "agent.sock") }

// EnvFile holds HARNESS_DSN and the other settings of harnessd (mode 0600).
func EnvFile(home string) string { return filepath.Join(configDir(home), "harnessd.env") }

// HookLog is the local log of hook events harnessd did not answer (L3, AC-S2-005).
func HookLog(home string) string { return filepath.Join(StateDir(home), "failopen.log") }
