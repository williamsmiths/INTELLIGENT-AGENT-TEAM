// Package guard decides whether a file write is inside the scope of a task.
// It is pure path logic: no database, no model. Spec: AC-H-022, AC-H-023, AC-H-024.
package guard

import (
	"path/filepath"
	"regexp"
	"strings"
)

// Rules is the write scope of one lease.
type Rules struct {
	Repo       string   // repository (or git worktree) root of the task
	Allowed    []string // globs relative to Repo; ** crosses directories
	Protected  []string // globs relative to Repo; win over Allowed
	ExtraRoots []string // roots outside the repo the agent may always write (temp dirs, its own workspace)
}

// Decision is the answer for one write.
type Decision struct {
	Allow  bool
	Rule   string // machine code: allowed, extra_root, protected, not_allowed, outside_scope, symlink_escape, dangling_symlink, bad_path
	Reason string // short human text; safe to show to the agent
	Path   string // canonical path that was judged
}

// AlwaysProtected can never be opened by a task: repository internals, agent
// hook files, and the files that steer git itself (limitation L6, spec S2 D.1).
// It is the one list; internal/verify judges the scope check with it too.
var AlwaysProtected = []string{".git", ".git/**", ".claude", ".claude/**", "**/.gitignore", "**/.gitattributes", ".gitmodules"}

// Inside reports whether target is root or below it. Both must be canonical.
func Inside(root, target string) bool {
	if root == "" {
		return false
	}
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// CheckWrite judges one write by the holder of the lease that owns r. A write
// that would be allowed but goes through a symbolic link whose target does not
// exist yet is refused (AC-S2-053): the file would be created wherever the
// link points at the moment of the write, not where it was judged.
func (r Rules) CheckWrite(path, cwd string) Decision {
	res, err := Resolve(path, cwd)
	if err != nil {
		return Decision{Rule: "bad_path", Reason: "không xác định được đường dẫn ghi: " + err.Error()}
	}
	d := r.judge(res.Path, path, cwd)
	if d.Allow && res.Dangling {
		return Decision{Rule: "dangling_symlink", Reason: "ghi qua symlink treo (đích chưa tồn tại) không được phép", Path: res.Path}
	}
	return d
}

func (r Rules) judge(target, path, cwd string) Decision {
	repo, err := Canonical(r.Repo, "")
	if err != nil {
		return Decision{Rule: "bad_path", Reason: "repo của task không tồn tại", Path: target}
	}
	if Inside(repo, target) {
		rel := filepath.ToSlash(mustRel(repo, target))
		for _, p := range append(append([]string{}, AlwaysProtected...), r.Protected...) {
			if Match(p, rel) {
				return Decision{Rule: "protected", Reason: "đường dẫn được bảo vệ (" + p + "): " + rel, Path: target}
			}
		}
		for _, p := range r.Allowed {
			if Match(p, rel) {
				return Decision{Allow: true, Rule: "allowed", Path: target}
			}
		}
		return Decision{Rule: "not_allowed", Reason: "ngoài allowed_paths của task: " + rel, Path: target}
	}
	// The path as written points into the repo but resolves elsewhere: a symlink leaves the scope.
	if lexical := lexicalAbs(path, cwd); lexical != "" && Inside(filepath.Clean(r.Repo), lexical) {
		return Decision{Rule: "symlink_escape", Reason: "symlink dẫn ra ngoài repo của task", Path: target}
	}
	for _, root := range r.ExtraRoots {
		if c, err := Canonical(root, ""); err == nil && Inside(c, target) {
			return Decision{Allow: true, Rule: "extra_root", Path: target}
		}
	}
	return Decision{Rule: "outside_scope", Reason: "ngoài repo của task và ngoài thư mục tạm/workspace của agent", Path: target}
}

// Match reports whether rel (slash-separated, relative to the repo) matches a glob.
// "**" matches any number of path segments, "*" and "?" stay inside one segment.
func Match(pattern, rel string) bool {
	re, err := compile(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(rel)
}

func compile(pattern string) (*regexp.Regexp, error) {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; {
		case c == '*' && strings.HasPrefix(pattern[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case c == '*' && strings.HasPrefix(pattern[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func lexicalAbs(path, cwd string) string {
	if !filepath.IsAbs(path) {
		if cwd == "" {
			return ""
		}
		path = filepath.Join(cwd, path)
	}
	return filepath.Clean(path)
}

func mustRel(root, target string) string {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return target
	}
	return rel
}
