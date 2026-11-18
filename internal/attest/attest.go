// Package attest tells who is on the other end of a unix socket by what the
// kernel says about that process, not by anything the process sends
// (spec S2 A.1, AC-S2-001..003). There are no tokens: the identity of a peer
// is its chain of ancestors read from SO_PEERCRED and /proc.
//
// The rule: the host is the FARTHEST ancestor whose executable is the host
// app (OpenBotExe), the provider is the process right below the host, and the
// agent is the first directory of the provider's working directory below
// AgentsRoot. A process an agent starts later — even one that runs the host
// binary itself — sits below the real provider and cannot re-root the chain.
//
// Everything that cannot be read is "not attested": Resolve returns an error
// and never panics. Callers refuse by default.
package attest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// maxDepth bounds the walk to pid 1; a real chain on this machine has about ten levels.
const maxDepth = 64

// ErrNotAttested is what every failed resolution wraps.
var ErrNotAttested = errors.New("attest: not attested")

// Config names the executables and the directory the rule is built on. All
// three are absolute, canonical paths.
type Config struct {
	OpenBotExe string // executable of the host app
	AgentsRoot string // "<AgentsRoot>/<agent id>" is the workspace of an agent
	SelfExe    string // executable of harnessd: its children are verification runners
}

// Proc is one process of the chain as the kernel reported it.
type Proc struct {
	PID       int
	StartTime uint64 // field 22 of /proc/<pid>/stat: with the pid it names one process, not a recycled pid
	Exe       string // "" when it cannot be read (another user, a kernel thread, a deleted file)
	CWD       string // "" when it cannot be read
}

// Identity is what a peer attests as.
type Identity struct {
	Chain              []Proc // Chain[0] is the peer, then its parent, up to pid 1
	HostPID            int    // 0: no ancestor runs the host app
	ProviderPID        int    // the process right below the host; 0 without a host
	AgentID            string // "" when the provider does not work in an agent workspace
	HasHarnessAncestor bool   // an ancestor runs SelfExe (a verification check and its children)
	Sandboxed          bool   // a bwrap process sits between the peer and the provider
}

// Resolve walks from pid to pid 1. The start time (and parent) of the peer
// and of every ancestor is read before and after the walk and must be the
// same, so the answer describes the processes that were there when the peer
// connected and not ones that took over their pids meanwhile.
func (c Config) Resolve(pid int) (Identity, error) { return c.resolve(procFS("/proc"), pid) }

func (c Config) resolve(fs procFS, pid int) (Identity, error) {
	if pid <= 0 {
		return Identity{}, fmt.Errorf("%w: no peer pid", ErrNotAttested)
	}
	var id Identity
	var parents []int
	seen := map[int]bool{}
	for cur := pid; cur > 0; {
		if len(id.Chain) >= maxDepth || seen[cur] {
			return Identity{}, fmt.Errorf("%w: ancestor chain too deep or looping", ErrNotAttested)
		}
		seen[cur] = true
		p, parent, err := fs.read(cur)
		if err != nil {
			return Identity{}, fmt.Errorf("%w: pid %d: %v", ErrNotAttested, cur, err)
		}
		id.Chain, parents = append(id.Chain, p), append(parents, parent)
		cur = parent
	}
	// Second pass: every process of the chain is still the same process with
	// the same parent. A process that exited meanwhile (its pid may have been
	// taken by another) or was re-parented makes the chain "not attested".
	for i, p := range id.Chain {
		start, parent, err := fs.stat(p.PID)
		if err != nil || start != p.StartTime || parent != parents[i] {
			return Identity{}, fmt.Errorf("%w: the process tree changed during the walk", ErrNotAttested)
		}
	}
	c.judge(&id)
	return id, nil
}

// judge applies the rule to a chain that was read consistently.
func (c Config) judge(id *Identity) {
	host := -1
	for i := 1; i < len(id.Chain); i++ { // ancestors only: the peer itself is never the host of its own chain
		exe := id.Chain[i].Exe
		if c.OpenBotExe != "" && exe == c.OpenBotExe {
			host = i // keep going: the farthest one wins
		}
		if c.SelfExe != "" && exe == c.SelfExe {
			id.HasHarnessAncestor = true
		}
	}
	if host < 0 {
		return
	}
	provider := id.Chain[host-1]
	id.HostPID, id.ProviderPID = id.Chain[host].PID, provider.PID
	id.AgentID = agentOf(c.AgentsRoot, provider.CWD)
	for i := 1; i < host-1; i++ {
		if filepath.Base(id.Chain[i].Exe) == "bwrap" {
			id.Sandboxed = true
		}
	}
}

// agentOf returns the first path component of cwd below root, or "".
func agentOf(root, cwd string) string {
	if root == "" || cwd == "" || !filepath.IsAbs(cwd) {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(cwd))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	first, _, _ := strings.Cut(rel, string(filepath.Separator))
	return first
}

// Canonical resolves the symbolic links of an executable or directory path so
// it compares equal to what /proc reports. A path that does not exist is
// returned cleaned.
func Canonical(path string) string {
	if path == "" {
		return ""
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		return real
	}
	return filepath.Clean(path)
}

// Self is the canonical executable of the running process (Config.SelfExe).
func Self() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return Canonical(exe)
}
