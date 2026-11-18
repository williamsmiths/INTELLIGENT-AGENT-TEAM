package verify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// gitTimeout bounds every git command the harness starts.
const gitTimeout = 2 * time.Minute

// protectedConfig is put in front of every git command the harness runs on or
// against a repository an agent can write (spec S2 D.1, AC-S2-050): whatever
// the repository's own .git/config says, no hook, fsmonitor program, ignore
// file, attribute file or line-ending rule of the agent's choosing applies.
var protectedConfig = []string{
	"core.excludesFile=/dev/null", "core.attributesFile=/dev/null", "core.fsmonitor=false",
	"core.hooksPath=/dev/null", "core.untrackedCache=false", "core.autocrlf=false",
	"core.symlinks=true", "core.fileMode=true",
}

// safeEnv is the whole environment of such a git command. home must be an
// empty directory the harness just created: HOME and XDG_CONFIG_HOME point at
// it, so $HOME/.gitconfig and $HOME/.config/git/{ignore,attributes} of the OS
// user are never read. No inherited GIT_* variable survives.
func safeEnv(home string) []string {
	env := []string{"LC_ALL=C", "HOME=" + home, "XDG_CONFIG_HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1",
		"GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1"}
	if v := os.Getenv("PATH"); v != "" {
		env = append(env, "PATH="+v)
	}
	return env
}

// emptyHome creates the empty directory safeEnv needs; the caller removes it.
func emptyHome() (string, error) {
	dir, err := os.MkdirTemp("", "kch-git-home-")
	if err != nil {
		return "", fmt.Errorf("verify: temp home: %w", err)
	}
	return dir, nil
}

// gitCmd describes one git invocation.
type gitCmd struct {
	dir   string   // working directory
	env   []string // from safeEnv, plus GIT_DIR and friends when needed
	pre   []string // global options after the protected config (--git-dir, --attr-source)
	stdin []byte
}

// run starts git with the protected config and returns stdout and stderr. A
// non-zero exit, a timeout or a cancelled context is an error; a warning on
// stderr with exit code 0 is returned for the caller to judge.
func (g gitCmd) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	full := make([]string, 0, 2*len(protectedConfig)+len(g.pre)+len(args))
	for _, kv := range protectedConfig {
		full = append(full, "-c", kv)
	}
	full = append(append(full, g.pre...), args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir, cmd.Env = g.dir, g.env
	cmd.WaitDelay = 5 * time.Second
	if g.stdin != nil {
		cmd.Stdin = bytes.NewReader(g.stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	runErr := cmd.Run()
	stderr = clip(errb.String())
	if runErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", stderr, fmt.Errorf("git %s: %w", args[0], ctxErr)
		}
		return "", stderr, fmt.Errorf("git %s: %w: %s", args[0], runErr, stderr)
	}
	return out.String(), stderr, nil
}

// quiet is run for commands that must not say anything on stderr: git reports
// an unreadable directory or a skipped path as a warning with exit code 0, and
// a snapshot that skipped something is a failed snapshot (AC-S2-051).
func (g gitCmd) quiet(ctx context.Context, args ...string) (string, error) {
	out, warn, err := g.run(ctx, args...)
	if err != nil {
		return "", err
	}
	if warn != "" {
		return "", fmt.Errorf("git %s: %s", args[0], warn)
	}
	return out, nil
}

// clip shortens git's message and drops NUL bytes, which jsonb cannot store.
func clip(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\x00", ""))
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

// inRepo prepares a git command that runs inside a repository an agent can
// write, with its own empty home. done removes the home.
func inRepo(repo string) (g gitCmd, done func(), err error) {
	if !filepath.IsAbs(repo) {
		return gitCmd{}, nil, kerr.New(kerr.Invalid, "verify: repo path must be absolute")
	}
	home, err := emptyHome()
	if err != nil {
		return gitCmd{}, nil, err
	}
	return gitCmd{dir: repo, env: safeEnv(home)}, func() { _ = os.RemoveAll(home) }, nil
}

// HeadCommit returns the commit HEAD of the repository rooted at repo points
// at, or "" when repo is not the root of a git working tree or has no commit
// yet (T-H-02). The repository's own configuration cannot make this command
// start a program (AC-S2-050 e).
func HeadCommit(ctx context.Context, repo string) (string, error) {
	g, done, err := inRepo(repo)
	if err != nil {
		return "", err
	}
	defer done()
	if _, err := os.Lstat(filepath.Join(repo, ".git")); err != nil {
		return "", nil // the root itself must hold the repository, not a parent directory
	}
	out, _, err := g.run(ctx, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	var exit *exec.ExitError
	switch {
	case err == nil && isFullHash(strings.TrimSpace(out)):
		return strings.TrimSpace(out), nil
	case err == nil || errors.As(err, &exit):
		return "", nil
	default:
		return "", fmt.Errorf("verify: resolve HEAD: %w", err)
	}
}

// isFullHash reports a full object id: 40 (SHA-1) or 64 (SHA-256) hex digits.
func isFullHash(s string) bool {
	return (len(s) == 40 || len(s) == 64) && isCommitHash(s)
}
