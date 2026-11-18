package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Tree names what a snapshot reads: the working tree of Repo, compared with
// the trusted copy of Base. Nothing in it comes from the repository's .git.
type Tree struct {
	Repo   string   // repository (or worktree) root, absolute
	Base   string   // the task's base commit, pinned in Store (Store.PinBase)
	Store  Store    // trusted base store
	Ignore []string // trusted ignore patterns (project_repo.ignore_patterns), root .gitignore syntax
}

// snapshot is the working tree of a repo written as a git tree object into a
// throwaway git directory. Nothing is written into the repository and nothing
// is read from its .git: not its config, index, info files or objects.
type snapshot struct {
	t    Tree
	dir  string
	git  gitCmd
	tree string
}

// CandidateHash returns the git tree hash of the working tree: every file
// tracked in the base commit plus every other file that the trusted ignore
// list does not match (spec §2.1, S2 D.1). One changed byte, mode bit or link
// target in any such file changes the hash; a file the trusted list ignores
// does not. An error means the tree could not be read completely — it never
// means "unchanged" (AC-S2-051).
func CandidateHash(ctx context.Context, t Tree) (string, error) {
	s, err := takeSnapshot(ctx, t)
	if err != nil {
		return "", err
	}
	defer s.close()
	return s.tree, nil
}

// ChangedFiles lists every path that differs between t.Base and the current
// working tree, committed after the base or not, including new files that are
// not on the trusted ignore list and deleted files. A rename reports both
// paths (AC-H-046). The same error rule as CandidateHash applies.
func ChangedFiles(ctx context.Context, t Tree) ([]string, error) {
	s, err := takeSnapshot(ctx, t)
	if err != nil {
		return nil, err
	}
	defer s.close()
	return s.changed(ctx)
}

// Snapshot is CandidateHash and ChangedFiles from one reading of the tree, so
// the hash and the list describe exactly the same state.
func Snapshot(ctx context.Context, t Tree) (candidate string, changed []string, err error) {
	s, err := takeSnapshot(ctx, t)
	if err != nil {
		return "", nil, err
	}
	defer s.close()
	changed, err = s.changed(ctx)
	return s.tree, changed, err
}

func (t Tree) validate() error {
	switch {
	case !filepath.IsAbs(t.Repo):
		return kerr.New(kerr.Invalid, "verify: repo path must be absolute")
	case !isFullHash(t.Base):
		return kerr.New(kerr.Invalid, "verify: base commit must be a full commit hash")
	}
	if _, err := CleanIgnore(t.Ignore); err != nil {
		return err
	}
	return nil
}

func takeSnapshot(ctx context.Context, t Tree) (*snapshot, error) {
	if err := t.validate(); err != nil {
		return nil, err
	}
	store, err := t.Store.path(t.Repo)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(store, "HEAD")); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotPinned, t.Base)
	}
	dir, err := os.MkdirTemp("", "kch-verify-")
	if err != nil {
		return nil, fmt.Errorf("verify: temp dir: %w", err)
	}
	s := &snapshot{t: t, dir: dir}
	if err := s.open(ctx, store); err != nil {
		s.close()
		return nil, err
	}
	if err := s.build(ctx); err != nil {
		s.close()
		return nil, err
	}
	return s, nil
}

// open creates the throwaway git directory. Its only alternate object
// directory is the trusted base store (AC-S2-050 a); HOME and XDG_CONFIG_HOME
// are an empty directory of the snapshot (AC-S2-050 d); attributes are read
// from the empty tree, so no .gitattributes of the working tree — filter,
// ident, eol, working-tree-encoding — is applied (AC-S2-050 c).
func (s *snapshot) open(ctx context.Context, store string) error {
	home, gitDir := filepath.Join(s.dir, "home"), filepath.Join(s.dir, "git")
	if err := os.Mkdir(home, 0o700); err != nil {
		return fmt.Errorf("verify: temp home: %w", err)
	}
	env := safeEnv(home)
	if _, _, err := (gitCmd{dir: s.dir, env: env}).run(ctx, "init", "--quiet", "--bare", "--template=",
		"--object-format="+objectFormat(s.t.Base), gitDir); err != nil {
		return fmt.Errorf("verify: temp git dir: %w", err)
	}
	alternates := filepath.Join(gitDir, "objects", "info", "alternates")
	if err := os.WriteFile(alternates, []byte(filepath.Join(store, "objects")+"\n"), 0o600); err != nil {
		return fmt.Errorf("verify: temp git dir: %w", err)
	}
	if err := os.WriteFile(s.ignoreFile(), []byte(strings.Join(s.t.Ignore, "\n")+"\n"), 0o600); err != nil {
		return fmt.Errorf("verify: trusted ignore file: %w", err)
	}
	s.git = gitCmd{dir: s.t.Repo, env: append(env,
		"GIT_DIR="+gitDir, "GIT_WORK_TREE="+s.t.Repo, "GIT_INDEX_FILE="+filepath.Join(s.dir, "index"))}
	if _, _, err := s.git.run(ctx, "cat-file", "-e", s.t.Base+"^{tree}"); err != nil {
		return fmt.Errorf("%w: %s", ErrNotPinned, s.t.Base)
	}
	empty, _, err := s.git.run(ctx, "hash-object", "-w", "-t", "tree", "/dev/null")
	if err != nil || !isFullHash(strings.TrimSpace(empty)) {
		return fmt.Errorf("verify: empty tree: %v", err)
	}
	s.git.pre = []string{"--attr-source=" + strings.TrimSpace(empty)}
	return nil
}

func (s *snapshot) ignoreFile() string { return filepath.Join(s.dir, "ignore") }

// build fills the throwaway index and writes the tree. There is no
// `git add -A`: the index starts as the base commit, then exactly the listed
// paths (see paths) are re-read from the working tree — added, updated or
// removed. A path git skips or cannot read fails the snapshot (AC-S2-051).
func (s *snapshot) build(ctx context.Context) error {
	if _, err := s.git.quiet(ctx, "read-tree", s.t.Base); err != nil {
		return fmt.Errorf("verify: read the base tree: %w", err)
	}
	paths, err := s.paths(ctx)
	if err != nil {
		return err
	}
	load := s.git
	load.stdin = []byte(strings.Join(paths, "\x00") + "\x00")
	if len(paths) == 0 {
		load.stdin = []byte{}
	}
	if _, err := load.quiet(ctx, "update-index", "-z", "--add", "--remove", "--stdin"); err != nil {
		return fmt.Errorf("verify: index the working tree: %w", err)
	}
	tree, err := s.git.quiet(ctx, "write-tree")
	if err != nil {
		return fmt.Errorf("verify: write tree: %w", err)
	}
	s.tree = strings.TrimSpace(tree)
	if !isFullHash(s.tree) {
		return fmt.Errorf("verify: unexpected tree hash %q", s.tree)
	}
	return nil
}

// changed compares the snapshot with the base commit of the trusted store.
func (s *snapshot) changed(ctx context.Context) ([]string, error) {
	// --no-renames: a rename is a deletion plus an addition, so both paths are judged.
	out, err := s.git.quiet(ctx, "diff-tree", "-r", "--no-renames", "--name-only", "-z", s.t.Base+"^{commit}", s.tree)
	if err != nil {
		return nil, fmt.Errorf("verify: diff since %s: %w", s.t.Base, err)
	}
	paths := splitNUL(out)
	sort.Strings(paths)
	return paths, nil
}

func (s *snapshot) close() { _ = os.RemoveAll(s.dir) }
