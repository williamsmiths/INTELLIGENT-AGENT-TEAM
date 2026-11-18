// Package hookinstall generates and installs the managed part of an agent's
// Claude Code settings — the harness hooks and the sandbox the agent's Bash
// runs in — and watches the file afterwards (spec S2 A.1, AC-S2-005,
// AC-S2-011; S1 limitations L2, L3, AC-H-070, AC-H-071).
//
// The settings are generated from data (Generate), never written by hand, and
// the hash registered for a principal covers the whole file. The sandbox
// denies writes to "<workspace>/.claude", so an enrolled agent cannot edit the
// file from its Bash; a change by anything else is seen by the hook client on
// the next tool call (Observe) and by the periodic Scan, journaled as
// HOOK_TAMPERED, and every pre-tool is refused until the file is restored or
// enrolled again.
package hookinstall

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// event is one Claude Code hook event and the kch argument that handles it.
type event struct {
	Name    string // key in settings.json
	Arg     string // kch hook <Arg>; also the event name of POST /v1/hooks/claude/{event}
	Matcher bool   // tool events need a matcher; "*" is every tool
	Timeout int    // seconds Claude Code waits for the hook client
}

// PreToolUse waits long: `kch verify` runs inside that hook (AC-S2-004).
var events = []event{
	{"UserPromptSubmit", "prompt-submit", false, 15},
	{"SessionStart", "session-start", false, 15},
	{"PreToolUse", "pre-tool", true, 900},
	{"PostToolUse", "post-tool", true, 15},
	{"Stop", "stop", false, 15},
}

// SettingsPath is the Claude Code settings file of a workspace.
func SettingsPath(workspaceDir string) string {
	return filepath.Join(workspaceDir, ".claude", "settings.json")
}

// WorkspaceOf is the workspace a settings path belongs to.
func WorkspaceOf(settingsPath string) string {
	return filepath.Dir(filepath.Dir(settingsPath))
}

// Install merges the managed settings into <workspace>/.claude/settings.json.
// Hooks that are not ours and keys we do not manage are kept; our hooks, the
// whole "sandbox" block, our "env" keys and our "permissions.deny" entries are
// replaced, so installing the same settings twice gives the same file. The
// file is written atomically with mode 0600. It returns the path and the
// sha256 of the whole file, ready for Register.
func Install(s Settings) (settingsPath, sum string, err error) {
	if err := s.Validate(); err != nil {
		return "", "", err
	}
	settingsPath = SettingsPath(s.Workspace)
	root, old, err := load(settingsPath)
	if err != nil {
		return "", "", err
	}
	if _, err := removeOurs(root, s.Binary); err != nil {
		return "", "", err
	}
	if err := apply(root, s); err != nil {
		return "", "", err
	}
	data, err := encode(root)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o700); err != nil {
		return "", "", fmt.Errorf("hookinstall: settings directory: %w", err)
	}
	if old == nil || string(old) != string(data) {
		if err := writeAtomic(settingsPath, data); err != nil {
			return "", "", err
		}
	} else if err := os.Chmod(settingsPath, 0o600); err != nil {
		return "", "", fmt.Errorf("hookinstall: settings mode: %w", err)
	}
	return settingsPath, hashBytes(data), nil
}

// Uninstall removes only our hook entries from the settings file and the
// arrays that become empty because of it. Everything else stays (the sandbox
// block too: removing a hook must never widen what Bash can reach), and a
// missing file is not an error. Entries are recognised by the kch binary path.
func Uninstall(workspaceDir, binary string) error {
	if err := checkPaths(workspaceDir, binary); err != nil {
		return err
	}
	settingsPath := SettingsPath(workspaceDir)
	root, old, err := load(settingsPath)
	if err != nil || old == nil {
		return err
	}
	changed, err := removeOurs(root, binary)
	if err != nil || !changed {
		return err
	}
	data, err := encode(root)
	if err != nil {
		return err
	}
	return writeAtomic(settingsPath, data)
}

// FileHash returns the sha256 of a file, in hex. A missing file is an error
// that satisfies errors.Is(err, os.ErrNotExist).
func FileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("hookinstall: hash %s: %w", path, err)
	}
	return hashBytes(data), nil
}

func hashBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func checkPaths(workspaceDir, binary string) error {
	if !filepath.IsAbs(workspaceDir) {
		return kerr.New(kerr.Invalid, "hookinstall: workspace must be an absolute path")
	}
	if info, err := os.Stat(workspaceDir); err != nil || !info.IsDir() {
		return kerr.New(kerr.Invalid, "hookinstall: workspace %s is not a directory", workspaceDir)
	}
	if !filepath.IsAbs(binary) {
		return kerr.New(kerr.Invalid, "hookinstall: the kch binary must be an absolute path")
	}
	if hasControl(binary) || hasControl(workspaceDir) {
		return kerr.New(kerr.Invalid, "hookinstall: control character in the binary or workspace path")
	}
	return nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// isOurs recognises a hook command we installed for this event, whatever
// flags it carries (and the S1 form "<bin> --agent <id> hook <event>").
func isOurs(cmd, binary string, e event) bool {
	rest, ok := strings.CutPrefix(cmd, shellQuote(binary)+" ")
	if !ok {
		return false
	}
	return rest == "hook "+e.Arg || strings.HasPrefix(rest, "hook "+e.Arg+" ") ||
		strings.HasPrefix(rest, "--agent ") && strings.HasSuffix(rest, " hook "+e.Arg)
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote makes s one word of a POSIX shell command line.
func shellQuote(s string) string {
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
