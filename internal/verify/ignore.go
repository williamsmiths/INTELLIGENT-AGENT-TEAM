package verify

import (
	"context"
	"fmt"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// RootIgnoreAtHead returns the patterns of the root .gitignore as it is stored
// in the HEAD commit: the default trusted ignore list of a repository at
// registration (spec S2 D.1). Comments and blank lines are dropped. The file
// in the working tree and every nested .gitignore are never read. A directory
// without a commit or without a root .gitignore has no patterns.
func RootIgnoreAtHead(ctx context.Context, repo string) ([]string, error) {
	head, err := HeadCommit(ctx, repo)
	if err != nil || head == "" {
		return []string{}, err
	}
	g, done, err := inRepo(repo)
	if err != nil {
		return nil, err
	}
	defer done()
	entry, _, err := g.run(ctx, "ls-tree", "-z", head, "--", ".gitignore")
	if err != nil {
		return nil, fmt.Errorf("verify: read the root .gitignore: %w", err)
	}
	// "<mode> blob <id>\t.gitignore\0"; a symbolic link or a directory of that name is not an ignore file.
	meta, _, _ := strings.Cut(entry, "\t")
	fields := strings.Fields(meta)
	if len(fields) != 3 || fields[1] != "blob" || !strings.HasPrefix(fields[0], "100") {
		return []string{}, nil
	}
	body, _, err := g.run(ctx, "cat-file", "blob", fields[2])
	if err != nil {
		return nil, fmt.Errorf("verify: read the root .gitignore: %w", err)
	}
	return ParseIgnore(body)
}

// MaxIgnorePatterns bounds one trusted ignore list.
const MaxIgnorePatterns = 2000

// ParseIgnore turns the text of an ignore file into a trusted pattern list.
func ParseIgnore(text string) ([]string, error) {
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return CleanIgnore(lines)
}

// CleanIgnore validates a trusted ignore list: comments and blank lines are
// dropped; a pattern with a NUL byte or a line break, a pattern longer than
// 1024 bytes, or more than MaxIgnorePatterns patterns is refused (422).
func CleanIgnore(patterns []string) ([]string, error) {
	out := []string{}
	for _, p := range patterns {
		if strings.TrimSpace(p) == "" || strings.HasPrefix(p, "#") {
			continue
		}
		if strings.ContainsAny(p, "\x00\n\r") || len(p) > 1024 {
			return nil, kerr.New(kerr.Invalid, "ignore pattern must be one line of at most 1024 bytes")
		}
		out = append(out, p)
	}
	if len(out) > MaxIgnorePatterns {
		return nil, kerr.New(kerr.Invalid, "at most %d ignore patterns", MaxIgnorePatterns)
	}
	return out, nil
}
