package verify

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Check is one required check of a task (task.required_checks).
type Check struct {
	ID             string
	Command        string
	TimeoutSeconds int
	Network        bool // share the host's network namespace; default: no network at all (AC-S2-010)
}

// Request is everything one verification run needs. The caller fills it from
// the task row, the project's repo registration and the harness
// configuration; nothing in it comes from the agent's request body.
type Request struct {
	ProjectID      string
	TaskID         string
	Revision       int
	Repo           string   // repository (or worktree) root, absolute
	BaseCommit     string   // task.base_commit, a full commit hash pinned in BaseStore
	BaseStore      Store    // trusted base store (spec S2 D.1)
	IgnorePatterns []string // trusted ignore list of the repo (project_repo.ignore_patterns)
	Allowed        []string
	Protected      []string
	Checks         []Check
	LogDir         string // absolute, outside Repo
	RunnerID       string

	// Isolation of the checks (spec S2 part A "Runner cô lập", AC-S2-010).
	HideDirs  []string // directories replaced by an empty tmpfs inside the sandbox (secrets, other repos)
	HideFiles []string // files masked with /dev/null inside the sandbox
	CacheDir  string   // per-project directory bound read-write at $HOME/.cache; "" = an empty tmpfs
	Bwrap     string   // bubblewrap binary; "" = DefaultBwrap
}

// Item is the outcome of one check and becomes one evidence row.
type Item struct {
	ID         string // evidence id, fixed at run time so Record is idempotent
	CheckID    string
	Command    string
	Result     string // PASS, FAIL or ERROR
	ExitCode   *int   // nil when the command did not produce one (timeout, failed start)
	LogSHA256  string
	LogPath    string
	Detail     map[string]any
	StartedAt  time.Time
	FinishedAt time.Time
}

// Result is the outcome of one run. Callers must treat it as read-only: two
// concurrent callers may hold the same Result (AC-H-045).
type Result struct {
	Candidate        string // tree hash before the checks ran; "" when the tree could not be read (AC-S2-051)
	CandidateChanged bool   // the tree hash after the checks differs
	Items            []Item // required checks in order, then "scope"
	AllPass          bool   // every check and "scope" PASS on an unchanged candidate
	StartedAt        time.Time
	FinishedAt       time.Time

	sealedFor string // set by Run only; Record refuses anything else (AC-H-042)
}

// tree is what the run snapshots.
func (r Request) tree() Tree {
	return Tree{Repo: r.Repo, Base: r.BaseCommit, Store: r.BaseStore, Ignore: r.IgnorePatterns}
}

func (r Request) sealKey() string {
	return fmt.Sprintf("%s|%s|%d|%s", r.ProjectID, r.TaskID, r.Revision, r.RunnerID)
}

func (r Request) validate() error {
	switch {
	case r.ProjectID == "" || r.TaskID == "" || r.RunnerID == "":
		return kerr.New(kerr.Invalid, "verify: project id, task id and runner id are required")
	case !filepath.IsAbs(r.Repo) || !filepath.IsAbs(r.LogDir):
		return kerr.New(kerr.Invalid, "verify: repo and log directory must be absolute paths")
	case len(r.Checks) == 0:
		return kerr.New(kerr.Invalid, "verify: no required checks (T-H-01)")
	case r.CacheDir != "" && !filepath.IsAbs(r.CacheDir):
		return kerr.New(kerr.Invalid, "verify: cache directory must be an absolute path")
	}
	if err := r.tree().validate(); err != nil {
		return err
	}
	if _, err := r.BaseStore.path(r.Repo); err != nil {
		return err
	}
	for _, p := range append(append([]string{}, r.HideDirs...), r.HideFiles...) {
		if !filepath.IsAbs(p) {
			return kerr.New(kerr.Invalid, "verify: hidden paths must be absolute")
		}
	}
	seen := map[string]bool{}
	for _, c := range r.Checks {
		if c.ID == "" || strings.TrimSpace(c.Command) == "" {
			return kerr.New(kerr.Invalid, "verify: a check needs an id and a command")
		}
		if c.ID == ScopeCheckID || seen[c.ID] {
			return kerr.New(kerr.Invalid, "verify: check id %q is reserved or repeated", c.ID)
		}
		seen[c.ID] = true
	}
	// A log or a cache written inside the repo would itself change the candidate.
	repo, err := guard.Canonical(r.Repo, "")
	if err != nil {
		return kerr.New(kerr.Invalid, "verify: repo path: %v", err)
	}
	for name, dir := range map[string]string{"log directory": r.LogDir, "cache directory": r.CacheDir, "base store": r.BaseStore.Dir} {
		if dir == "" {
			continue
		}
		real, err := guard.Canonical(dir, "")
		if err != nil {
			return kerr.New(kerr.Invalid, "verify: %s: %v", name, err)
		}
		if guard.Inside(repo, real) {
			return kerr.New(kerr.Invalid, "verify: %s must be outside the repo", name)
		}
	}
	return nil
}

func isCommitHash(s string) bool {
	if len(s) < 7 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
