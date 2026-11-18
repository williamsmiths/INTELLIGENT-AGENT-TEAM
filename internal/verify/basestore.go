package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// ErrNotPinned is returned when a base commit is not in the trusted store.
// Nothing is compared against the working repository's own objects instead.
var ErrNotPinned = errors.New("verify: base commit is not pinned in the trusted base store")

// Store is the trusted base store (spec S2 D.1): one bare repository per
// working repository, owned by the harness, holding the commits tasks were
// pinned to. Every comparison against a base reads the base from here. The
// working repository's object directory is never an alternate of anything the
// harness reads, so rewriting objects under its .git changes nothing
// (AC-S2-050 a). Dir must be a directory agents cannot write.
type Store struct {
	Dir string // absolute; production: ~/.local/state/kc-harness/bases
}

// storeLocks serializes writers of one bare repository inside this process.
var storeLocks sync.Map // path -> *sync.Mutex

// path is <Dir>/<sha256(realpath repo)[:16]>.git.
func (s Store) path(repo string) (string, error) {
	if !filepath.IsAbs(s.Dir) {
		return "", kerr.New(kerr.Invalid, "verify: base store directory must be an absolute path")
	}
	if strings.ContainsAny(s.Dir, "\n\x00") {
		return "", kerr.New(kerr.Invalid, "verify: unsupported character in the base store path")
	}
	real, err := guard.Canonical(repo, "")
	if err != nil || !filepath.IsAbs(repo) {
		return "", kerr.New(kerr.Invalid, "verify: repo path must be absolute and resolvable")
	}
	sum := sha256.Sum256([]byte(real))
	return filepath.Join(s.Dir, hex.EncodeToString(sum[:])[:16]+".git"), nil
}

func (s Store) git(dir, home string) gitCmd {
	return gitCmd{dir: dir, env: safeEnv(home), pre: []string{"--git-dir=" + dir}}
}

// Has reports whether commit is pinned for repo.
func (s Store) Has(ctx context.Context, repo, commit string) (bool, error) {
	dir, err := s.path(repo)
	if err != nil || !isFullHash(commit) {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false, nil
	}
	home, err := emptyHome()
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(home)
	return s.has(ctx, s.git(dir, home), commit), nil
}

func (s Store) has(ctx context.Context, g gitCmd, commit string) bool {
	_, _, err := g.run(ctx, "cat-file", "-e", commit+"^{tree}")
	return err == nil
}

// PinBase copies commit (the commit object and its whole tree, without its
// history) from the working repository into the store and keeps it under
// refs/kch/base/<commit>. The copy is checked as it arrives
// (fetch.fsckObjects, transfer.fsckObjects): an object whose content does not
// hash to its name, or a commit whose tree is incomplete, makes the fetch
// fail, and nothing is pinned. Pinning a commit that is already there is a
// cheap no-op, so callers pin at every assignment.
func (s Store) PinBase(ctx context.Context, repo, commit string) error {
	if !isFullHash(commit) {
		return kerr.New(kerr.Invalid, "verify: base commit must be a full commit hash")
	}
	dir, err := s.path(repo)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("verify: base store: %w", err)
	}
	lock, _ := storeLocks.LoadOrStore(dir, &sync.Mutex{})
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()

	home, err := emptyHome()
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	g := s.git(dir, home)
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		init := gitCmd{dir: s.Dir, env: g.env}
		if _, _, err := init.run(ctx, "init", "--quiet", "--bare", "--template=", "--object-format="+objectFormat(commit), dir); err != nil {
			return fmt.Errorf("verify: create the base store: %w", err)
		}
	}
	if s.has(ctx, g, commit) {
		return nil
	}
	// The source is the agent-writable repository: git-upload-pack runs there
	// and reads its config, but serves objects only. gc is off so an object
	// that just arrived cannot be pruned before its ref exists.
	_, _, err = g.run(ctx, "-c", "gc.auto=0", "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
		"-c", "protocol.version=2", "fetch", "--quiet", "--depth=1", "--no-tags", "--no-write-fetch-head",
		"--no-auto-maintenance", "--no-recurse-submodules", "--", repo, commit+":refs/kch/base/"+commit)
	if err != nil {
		return fmt.Errorf("verify: pin %s: %w", commit, err)
	}
	if !s.has(ctx, g, commit) {
		return fmt.Errorf("verify: pin %s: %w", commit, ErrNotPinned)
	}
	return nil
}

// objectFormat is the hash algorithm of an object id of that length.
func objectFormat(id string) string {
	if len(id) == 64 {
		return "sha256"
	}
	return "sha1"
}
