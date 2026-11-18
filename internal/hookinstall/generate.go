package hookinstall

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Input is the data the managed settings of one agent are generated from
// (spec S2 A.1 "Sandbox do kch enroll sinh"). Every path is absolute and canonical.
type Input struct {
	Workspace      string            // the agent's workspace: <AgentsRoot>/<openbot agent id>
	Binary         string            // the kch binary the hooks run
	Socket         string            // agent.sock of harnessd
	Enrolled       bool              // pre-tool fails closed when harnessd cannot be reached
	Home           string            // home of the OS user: hidden from the sandbox as a whole
	RepoRoots      []string          // repo roots of the projects the agent is a member of: read and write, except .git
	ReferenceRoots []string          // read-only roots the Owner declared for those projects
	ReadRoots      []string          // toolchain roots of the machine (machine.json sandbox_read_roots)
	TmpRoot        string            // private temp root of the agent: read and write
	HiddenTmp      []string          // /tmp/claude-<uid>/-* of the other sessions that exist now (limitation L11)
	SharedRoot     string            // OpenBot's shared folder: OpenBot adds it to every session's directories; hidden here
	Env            map[string]string // GOMODCACHE, GOCACHE, GOPROXY, TMPDIR, ...
}

// HookCommand is one hook handler.
type HookCommand struct {
	Event   string // Claude Code event name
	Matcher string // "*" for tool events, "" otherwise
	Command string
	Timeout int // seconds
}

// Sandbox is the "sandbox" block of the settings file.
type Sandbox struct {
	DenyRead, AllowRead, AllowWrite, DenyWrite []string
}

// Settings is the whole managed block of one settings file.
type Settings struct {
	Workspace string
	Binary    string
	Home      string
	Hooks     []HookCommand
	Sandbox   Sandbox
	Env       map[string]string
	Deny      []string // permissions.deny
}

// Generate builds the managed block. It is a pure function of its input: the
// same data gives the same settings, byte for byte after Install.
func Generate(in Input) Settings {
	s := Settings{Workspace: in.Workspace, Binary: in.Binary, Home: in.Home, Env: map[string]string{}}
	for _, e := range events {
		cmd := shellQuote(in.Binary) + " hook " + e.Arg + " --socket " + shellQuote(in.Socket) + " --workspace " + shellQuote(in.Workspace)
		if in.Enrolled {
			cmd += " --enrolled"
		}
		h := HookCommand{Event: e.Name, Command: cmd, Timeout: e.Timeout}
		if e.Matcher {
			h.Matcher = "*"
		}
		s.Hooks = append(s.Hooks, h)
	}
	repos, refs := uniq(in.RepoRoots), uniq(in.ReferenceRoots)
	// The home is hidden as a whole and single areas are opened again, so a
	// directory that appears later is hidden by default (REV-004 #6).
	s.Sandbox.DenyRead = append([]string{in.Home, "/run", "/var/run"}, uniq(in.HiddenTmp)...)
	s.Sandbox.AllowRead = uniq(append(append(append([]string{in.Workspace, filepath.Join(in.Home, ".claude", "shell-snapshots")},
		in.ReadRoots...), repos...), refs...))
	s.Sandbox.AllowWrite = uniq(append(append([]string{}, repos...), in.TmpRoot))
	for _, r := range repos {
		s.Sandbox.DenyWrite = append(s.Sandbox.DenyWrite, filepath.Join(r, ".git")) // spec S2 D.1: the agent never writes git's own files
	}
	s.Sandbox.DenyWrite = append(s.Sandbox.DenyWrite, filepath.Join(in.Workspace, ".claude"), filepath.Join(in.Workspace, ".mcp.json"))
	if in.SharedRoot != "" {
		// OpenBot passes its shared folder as an additional directory of every session. Measured on
		// Claude Code 2.1.323 (team pilot): that directory stays readable from Bash even with this
		// denyRead, so Bash can still list other projects' files there (limitation L14, spec S2 §A.3).
		// The Read/Edit permission rules below do hold for the file tools; the entries stay so the
		// intent is explicit and takes effect if Claude Code ever lets denyRead win.
		s.Sandbox.DenyRead = append(s.Sandbox.DenyRead, in.SharedRoot)
		s.Sandbox.DenyWrite = append(s.Sandbox.DenyWrite, in.SharedRoot)
	}
	for k, v := range in.Env {
		s.Env[k] = v
	}
	// Permission rules need "//" for an absolute path; "/abs" would be read as relative to the settings file.
	s.Deny = []string{"Read(/" + in.Home + "/**)", "Edit(/" + filepath.Join(in.Workspace, ".claude") + "/**)", "WebFetch", "WebSearch", "mcp__*"}
	if in.SharedRoot != "" {
		s.Deny = append(s.Deny, "Read(/"+in.SharedRoot+"/**)", "Edit(/"+in.SharedRoot+"/**)")
	}
	return s
}

// Validate refuses settings that would not mean what they say: a relative or
// unclean path, or a write area inside a hidden directory (it would silently
// become read-only — measured on Claude Code 2.1.323).
func (s Settings) Validate() error {
	if err := checkPaths(s.Workspace, s.Binary); err != nil {
		return err
	}
	if len(s.Hooks) == 0 || !cleanAbs(s.Home) {
		return kerr.New(kerr.Invalid, "hookinstall: settings need hooks and an absolute home directory")
	}
	sb := s.Sandbox
	for _, list := range [][]string{sb.DenyRead, sb.AllowRead, sb.AllowWrite, sb.DenyWrite} {
		for _, p := range list {
			if !cleanAbs(p) {
				return kerr.New(kerr.Invalid, "hookinstall: sandbox path %q must be absolute, clean and without control characters", p)
			}
		}
	}
	for _, w := range sb.AllowWrite {
		for _, d := range sb.DenyRead {
			if guard.Inside(d, w) {
				return kerr.New(kerr.Invalid, "hookinstall: write area %s is inside the hidden directory %s; repo roots and the temp root must live outside it", w, d)
			}
		}
	}
	for k, v := range s.Env {
		if k == "" || strings.ContainsAny(k, "= ") || hasControl(k) || hasControl(v) {
			return kerr.New(kerr.Invalid, "hookinstall: invalid environment entry %q", k)
		}
	}
	return nil
}

func cleanAbs(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !hasControl(p)
}

// uniq returns the non-empty values once, sorted.
func uniq(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, v := range in {
		if v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}
