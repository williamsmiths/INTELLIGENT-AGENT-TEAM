package guard

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Limits of one resolution. The kernel follows at most 40 links (ELOOP); the
// step limit bounds the work for a path made of very many components.
const (
	maxLinks = 40
	maxSteps = 4096
)

// Resolved is the real location a path names.
type Resolved struct {
	Path string // absolute, without symbolic links, "." or ".."
	// Dangling: a symbolic link on the way points at something that does not
	// exist yet, so a write would create the file at the link's target.
	Dangling bool
}

// Canonical returns the real location of path, resolved against cwd when it is
// relative. See Resolve.
func Canonical(path, cwd string) (string, error) {
	r, err := Resolve(path, cwd)
	return r.Path, err
}

// Resolve walks path one component at a time the way the kernel does
// (AC-S2-053, AC-H-023):
//   - nothing is cleaned lexically first: ".." after a symbolic link goes to
//     the parent of the link's target, not to the parent of the link;
//   - a relative link target is resolved against the real directory that holds
//     the link, whatever name the path used to reach it;
//   - a component that does not exist yet is kept as a plain name, so a new
//     file in a new directory still gets its real location;
//   - a link whose target does not exist is followed and reported as Dangling;
//   - link loops and over-long paths end with an error instead of spinning.
//
// A path that contains a NUL byte is refused: the kernel would cut it there,
// so the name that is judged would not be the name that is written.
func Resolve(path, cwd string) (Resolved, error) {
	switch {
	case path == "":
		return Resolved{}, errors.New("empty path")
	case strings.ContainsRune(path, 0) || strings.ContainsRune(cwd, 0):
		return Resolved{}, errors.New("path contains a NUL byte")
	}
	if !filepath.IsAbs(path) {
		if cwd == "" || !filepath.IsAbs(cwd) {
			return Resolved{}, errors.New("relative path without an absolute cwd")
		}
		path = cwd + "/" + path // no Clean: the walk below gives ".." its real meaning
	}
	// part is one pending component; viaLink marks the ones that came out of a link target.
	type part struct {
		name    string
		viaLink bool
	}
	split := func(p string, viaLink bool) []part {
		names := strings.Split(p, "/")
		parts := make([]part, 0, len(names))
		for _, n := range names {
			if n != "" && n != "." {
				parts = append(parts, part{n, viaLink})
			}
		}
		return parts
	}
	pending := split(path, false)
	cur := "/" // always a real directory path: every link before it is already resolved
	var out Resolved
	links := 0
	for steps := 0; len(pending) > 0; steps++ {
		if steps > maxSteps {
			return Resolved{}, errors.New("path is too long to resolve")
		}
		p := pending[0]
		pending = pending[1:]
		if p.name == ".." {
			cur = filepath.Dir(cur) // cur holds no link, so its lexical parent is its real parent
			continue
		}
		next := filepath.Join(cur, p.name)
		fi, err := os.Lstat(next)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if p.viaLink {
				out.Dangling = true
			}
			cur = next // does not exist yet: a write (or mkdir -p) creates it as a plain name
		case err != nil:
			return Resolved{}, errors.New("cannot resolve the path: " + reason(err))
		case fi.Mode()&os.ModeSymlink == 0:
			cur = next
		default:
			if links++; links > maxLinks {
				return Resolved{}, errors.New("too many symbolic links")
			}
			target, err := os.Readlink(next)
			if err != nil {
				return Resolved{}, errors.New("cannot read a symbolic link: " + reason(err))
			}
			if strings.ContainsRune(target, 0) || target == "" {
				return Resolved{}, errors.New("symbolic link with an invalid target")
			}
			if filepath.IsAbs(target) {
				cur = "/"
			}
			// A relative target continues from cur: the real directory of the link.
			pending = append(split(target, true), pending...)
		}
	}
	out.Path = cur
	return out, nil
}

// reason is the system error without the path: answers go back to the agent
// and into stored responses, so they never echo raw path bytes.
func reason(err error) string {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return "system error"
}
