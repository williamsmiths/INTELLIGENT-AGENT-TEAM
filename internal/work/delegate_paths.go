package work

import (
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// normalizeAllowed turns the paths a Lead wrote into the repo-relative globs
// the guard matches (spec E.1-13). A Lead wrote the absolute directory
// "/repo/prototype/" in the team pilot and every write of the recipient into
// prototype/ was refused as out of scope. Rules:
//   - an absolute path inside the repo becomes relative; outside the repo → 422;
//   - "./" is dropped; a ".." segment → 422;
//   - the repo itself, "" or "." → "**";
//   - a path ending in "/", or naming a directory that exists in the repo → "<dir>/**".
func normalizeAllowed(repo string, paths []string) ([]string, error) {
	out := make([]string, 0, len(paths))
	for _, raw := range paths {
		p := strings.TrimSpace(raw)
		if filepath.IsAbs(p) {
			rel, err := filepath.Rel(repo, filepath.Clean(p))
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				return nil, kerr.New(kerr.Invalid, "allowed_paths: %q nằm ngoài repo %s; dùng đường dẫn tương đối trong repo, ví dụ \"src/**\"", raw, repo)
			}
			if strings.HasSuffix(p, "/") && rel != "." {
				rel += "/"
			}
			p = rel
		}
		p = strings.TrimPrefix(p, "./")
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return nil, kerr.New(kerr.Invalid, "allowed_paths: %q có \"..\"; dùng đường dẫn tương đối trong repo, ví dụ \"src/**\"", raw)
			}
		}
		if p != "" { // "././src" → "src", "src//" → "src/" (REV-007b L-b1); a trailing "/" still means a directory
			dir := strings.HasSuffix(p, "/")
			p = path.Clean(p)
			if dir && p != "." {
				p += "/"
			}
		}
		switch {
		case p == "" || p == "." || p == "./":
			p = "**"
		case strings.HasSuffix(p, "/"):
			p += "**"
		case !strings.ContainsAny(p, "*?["):
			if fi, err := os.Stat(filepath.Join(repo, p)); err == nil && fi.IsDir() {
				p += "/**"
			}
		}
		out = append(out, p)
	}
	return out, nil
}
