package intel

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Mark is the working tree as the runtime saw it at the first tool call of a
// lease (lease.intel_mark): the candidate hash, and the state of every file
// that differed from the base. It tells a Bash write before the understanding
// (C.2) and which files changed since a feedback (C.5).
type Mark struct {
	Candidate string            `json:"candidate"`
	Files     map[string]string `json:"files"` // repo-relative path -> content state
}

// FileStates reads the state of each repo-relative path: a content hash, a
// link target, or "-" when the file does not exist. Paths come from the
// verification snapshot, never from the agent.
func FileStates(repo string, paths []string) map[string]string {
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		out[p] = fileState(filepath.Join(repo, filepath.FromSlash(p)))
	}
	return out
}

func fileState(path string) string {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		return "-"
	case info.Mode()&os.ModeSymlink != 0:
		target, _ := os.Readlink(path)
		return "l:" + target
	case !info.Mode().IsRegular():
		return "t:" + info.Mode().Type().String()
	}
	f, err := os.Open(path)
	if err != nil {
		return "e:unreadable"
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, io.LimitReader(f, 256<<20)); err != nil {
		return "e:unreadable"
	}
	return hex.EncodeToString(h.Sum(nil)) + ":" + info.Mode().Perm().String()
}

// ChangedSince lists the paths whose state differs between the mark and now.
// A path absent from a side has the state it has in the base there.
func ChangedSince(mark Mark, now map[string]string) []string {
	var out []string
	for p, s := range now {
		if old, ok := mark.Files[p]; !ok || old != s {
			out = append(out, p)
		}
	}
	for p := range mark.Files { // changed before, back to the base now
		if _, ok := now[p]; !ok {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return out
}

// DeltaMismatch compares the paths a delta declared as changed with the paths
// that really changed since the feedback (C.5; measured, never enforced).
// Entries without "/" or "." are knowledge keys and are not compared; a
// declared entry ending in "/" covers its directory.
func DeltaMismatch(declared, changed []string) map[string]any {
	var paths []string
	for _, d := range declared {
		if strings.ContainsAny(d, "/.") {
			paths = append(paths, strings.TrimPrefix(filepath.ToSlash(d), "./"))
		}
	}
	covers := func(d, p string) bool { return d == p || (strings.HasSuffix(d, "/") && strings.HasPrefix(p, d)) }
	undeclared, unchanged := []string{}, []string{}
	for _, p := range changed {
		if !slices.ContainsFunc(paths, func(d string) bool { return covers(d, p) }) {
			undeclared = append(undeclared, p)
		}
	}
	for _, d := range paths {
		if !slices.ContainsFunc(changed, func(p string) bool { return covers(d, p) }) {
			unchanged = append(unchanged, d)
		}
	}
	return map[string]any{"declared_paths": len(paths), "changed_paths": len(changed),
		"undeclared": undeclared, "declared_unchanged": unchanged}
}
