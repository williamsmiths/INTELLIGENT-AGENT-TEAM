package verify

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
)

// DefaultBwrap is the sandbox program of the runner (bubblewrap).
const DefaultBwrap = "/usr/bin/bwrap"

// readyMark is what the wrapper writes on fd 3 once it runs inside the sandbox.
const readyMark = "ok"

// wrapper runs inside the sandbox: it reports on fd 3, closes it, and becomes the check.
const wrapper = `printf ` + readyMark + ` >&3; exec 3>&-; exec bash -c "$1"`

// DefaultHideDirs is the list of secret directories of the OS user that a
// check must not read (spec S2 part A): harness and host-app configuration and
// the usual credential stores. Callers add what only they know: the harness
// repository, the base store, the evidence log directory, other repo roots.
func DefaultHideDirs(home string) []string {
	if home == "" {
		return nil
	}
	dirs := []string{}
	for _, d := range []string{".config/kc-harness", ".config/OpenBot", ".ssh", ".gnupg", ".docker", ".cloudflared"} {
		dirs = append(dirs, filepath.Join(home, d))
	}
	return dirs
}

// DefaultHideFiles is the matching list of secret files.
func DefaultHideFiles(home string) []string {
	if home == "" {
		return nil
	}
	return []string{filepath.Join(home, ".claude", ".credentials.json"), filepath.Join(home, ".claude.json")}
}

// Isolation is the harness-wide part of the sandbox settings: what a caller
// adds to every Request besides the task's own data (see Request).
type Isolation struct {
	HideDirs  []string // more directories to hide, on top of DefaultHideDirs
	HideFiles []string // more files to mask, on top of DefaultHideFiles
	CacheRoot string   // a project's cache is <CacheRoot>/<project id>; "" = no persistent cache
	Bwrap     string   // "" = DefaultBwrap
}

// sandboxArgs builds the bwrap command line of one check (AC-S2-010):
//
//	read-only root; fresh /dev and /proc; empty tmpfs on /tmp and /run;
//	every existing HideDirs entry replaced by an empty tmpfs;
//	the repo bound read-write (after the hides, so a repo below /tmp or below
//	a hidden directory stays visible; hides inside the repo come after it);
//	CacheDir bound read-write at $HOME/.cache (an empty tmpfs without one);
//	every existing HideFiles entry masked with /dev/null;
//	own pid and ipc namespaces, own session, dies with harnessd;
//	own (empty) network namespace unless the check has Network.
func sandboxArgs(req Request, c Check) (bwrap string, args []string, err error) {
	bwrap = req.Bwrap
	if bwrap == "" {
		bwrap = DefaultBwrap
	}
	if st, err := os.Stat(bwrap); err != nil || st.IsDir() {
		return "", nil, fmt.Errorf("%s is not available", bwrap)
	}
	repo, err := filepath.EvalSymlinks(req.Repo)
	if err != nil {
		return "", nil, errors.New("the repo root cannot be resolved")
	}
	args = []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp", "--tmpfs", "/run"}
	var hidden, inRepo []string
	for _, dir := range req.HideDirs {
		real, err := filepath.EvalSymlinks(dir)
		if st, serr := os.Stat(real); err != nil || serr != nil || !st.IsDir() || real == repo || real == "/" {
			continue // nothing to hide there; a mount point cannot be created on the read-only root
		}
		hidden = append(hidden, real)
		if guard.Inside(repo, real) {
			inRepo = append(inRepo, real)
			continue
		}
		args = append(args, "--tmpfs", real)
	}
	args = append(args, "--bind", repo, repo)
	for _, dir := range inRepo {
		args = append(args, "--tmpfs", dir)
	}
	if home := os.Getenv("HOME"); filepath.IsAbs(home) {
		cache := filepath.Join(home, ".cache")
		if err := os.MkdirAll(cache, 0o700); err != nil {
			return "", nil, errors.New("the cache mount point cannot be created")
		}
		if req.CacheDir == "" {
			args = append(args, "--tmpfs", cache)
		} else {
			if err := os.MkdirAll(req.CacheDir, 0o700); err != nil {
				return "", nil, errors.New("the cache directory cannot be created")
			}
			args = append(args, "--bind", req.CacheDir, cache)
		}
	}
files:
	for _, file := range req.HideFiles {
		real, err := filepath.EvalSymlinks(file)
		if st, serr := os.Stat(real); err != nil || serr != nil || !st.Mode().IsRegular() {
			continue
		}
		for _, dir := range hidden {
			if guard.Inside(dir, real) {
				continue files // already gone with its directory
			}
		}
		args = append(args, "--ro-bind", "/dev/null", real)
	}
	args = append(args, "--unshare-pid", "--unshare-ipc", "--die-with-parent", "--new-session")
	if !c.Network {
		args = append(args, "--unshare-net")
	}
	return bwrap, append(args, "--chdir", repo, "--", "bash", "-c", wrapper, "kch-check", c.Command), nil
}
