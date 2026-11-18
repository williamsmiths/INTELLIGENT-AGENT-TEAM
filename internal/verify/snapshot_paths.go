package verify

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ErrSubmodule is returned for a base commit with a submodule whose directory
// holds files: git records such a directory as one link to a commit, so the
// files inside could change without changing the candidate. The harness
// refuses to judge it rather than pass it silently (spec S2 D.1).
var ErrSubmodule = errors.New("verify: a submodule with files in its directory is not supported")

// paths lists what the snapshot re-reads from the working tree:
//
//   - every path tracked in the base commit, ignored or not, so a tracked
//     file that was edited or deleted always shows;
//   - every other file of the working tree, except what the trusted ignore
//     list matches. git lists them with --exclude-from only: without
//     --exclude-standard and without a per-directory ignore file, so no
//     .gitignore inside the tree is read (AC-S2-050 b), nor .git/info/exclude,
//     nor the user's global ignore file (AC-S2-050 d, e);
//   - a directory that is itself a git repository (its own .git) is not
//     skipped and not recorded as a link: its files are listed one by one as
//     plain files, whatever is committed inside it. Only names called .git
//     are left out, as in the root — git cannot store them in a tree.
func (s *snapshot) paths(ctx context.Context) ([]string, error) {
	staged, err := s.git.quiet(ctx, "ls-files", "-z", "--stage")
	if err != nil {
		return nil, fmt.Errorf("verify: list the base tree: %w", err)
	}
	var paths []string
	for _, entry := range splitNUL(staged) { // "<mode> <id> <stage>\t<path>"
		meta, path, ok := strings.Cut(entry, "\t")
		if !ok {
			return nil, fmt.Errorf("verify: unexpected index entry %q", entry)
		}
		if strings.HasPrefix(meta, "160000 ") {
			if err := s.emptySubmodule(path); err != nil {
				return nil, err
			}
		}
		paths = append(paths, path)
	}
	others, err := s.git.quiet(ctx, "ls-files", "-z", "--others", "--exclude-from="+s.ignoreFile())
	if err != nil {
		return nil, fmt.Errorf("verify: list the working tree: %w", err)
	}
	for _, path := range splitNUL(others) {
		if !strings.HasSuffix(path, "/") {
			paths = append(paths, path)
			continue
		}
		nested, err := s.plainFiles(strings.TrimSuffix(path, "/"))
		if err != nil {
			return nil, err
		}
		paths = append(paths, nested...)
	}
	return paths, nil
}

// plainFiles walks a nested repository as an ordinary directory. The trusted
// ignore list is not applied inside it: everything there counts.
func (s *snapshot) plainFiles(rel string) ([]string, error) {
	var out []string
	root := filepath.Join(s.t.Repo, filepath.FromSlash(rel))
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// git stores regular files and symbolic links; sockets, pipes and devices have no content to hash.
		if d.Type().IsRegular() || d.Type()&fs.ModeSymlink != 0 {
			relPath, err := filepath.Rel(s.t.Repo, path)
			if err != nil {
				return err
			}
			out = append(out, filepath.ToSlash(relPath))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("verify: read nested repository %q: %w", rel, err)
	}
	return out, nil
}

// emptySubmodule accepts a submodule path of the base only when there is
// nothing to hide in it: absent, not a directory, or an empty directory.
func (s *snapshot) emptySubmodule(rel string) error {
	dir := filepath.Join(s.t.Repo, filepath.FromSlash(rel))
	fi, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) || (err == nil && !fi.IsDir()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify: read submodule %q: %w", rel, err)
	}
	entries, err := os.ReadDir(dir)
	switch {
	case err != nil:
		return fmt.Errorf("verify: read submodule %q: %w", rel, err)
	case len(entries) > 0:
		return fmt.Errorf("%w: %q", ErrSubmodule, rel)
	}
	return nil
}

func splitNUL(out string) []string {
	paths := []string{}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths
}
