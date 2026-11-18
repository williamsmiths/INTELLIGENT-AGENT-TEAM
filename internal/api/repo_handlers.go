package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// setRepoIgnore serves PUT /v1/projects/{p}/repos/ignore (owner only): it
// replaces the trusted ignore list of one repo root (spec S2 D.1). The body is
// {"path": "<repo root>", "patterns": ["..."]}; "path" may be left out when the
// project has exactly one root.
func (s *Server) setRepoIgnore(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Path     string   `json:"path"`
		Patterns []string `json:"patterns"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var root string
	var patterns []string
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		root, patterns, err = work.SetRepoIgnore(r.Context(), tx, principalOf(r), r.PathValue("p"), in.Path, in.Patterns)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"root": root, "ignore_patterns": orEmpty(patterns)}, err)
}

// stateDir is where the harness keeps what agents must not write: the parent of the evidence log directory.
func (s *Server) stateDir() string { return filepath.Dir(s.LogDir) }

// bases is the trusted base store (spec S2 D.1). Without an explicit
// directory it is "bases" next to the evidence logs, which in production is
// ~/.local/state/kc-harness/bases.
func (s *Server) bases() verify.Store {
	if s.Bases.Dir != "" {
		return s.Bases
	}
	return verify.Store{Dir: filepath.Join(s.stateDir(), "bases")}
}

// treeOf is what a snapshot of the task's repo reads: the pinned base, the
// trusted store and the trusted ignore list of the repo root. Nothing of it
// comes from the repository or from the agent (AC-S2-050).
func (s *Server) treeOf(ctx context.Context, t work.Task) (verify.Tree, error) {
	tree := verify.Tree{Repo: t.RepoPath, Base: t.BaseCommit, Store: s.bases()}
	err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		tree.Ignore, err = work.RepoIgnore(ctx, tx, t.RepoPath)
		return err
	})
	return tree, err
}

// verifyRequest fills a verification request from the task row, the repo
// registration and the harness configuration (AC-S2-010, AC-S2-050). Hidden
// from every check: the OS user's credential directories, the harness's own
// state (base store, evidence logs), every agent workspace and the repo roots
// of all other projects.
func (s *Server) verifyRequest(ctx context.Context, t work.Task) (verify.Request, error) {
	tree, err := s.treeOf(ctx, t)
	if err != nil {
		return verify.Request{}, err
	}
	home, _ := os.UserHomeDir()
	req := verify.Request{ProjectID: t.ProjectID, TaskID: t.ID, Revision: t.Revision, Repo: t.RepoPath,
		BaseCommit: t.BaseCommit, BaseStore: tree.Store, IgnorePatterns: tree.Ignore,
		Allowed: t.AllowedPaths, Protected: t.ProtectedPaths, LogDir: s.LogDir, RunnerID: "harnessd",
		HideDirs:  append(append(verify.DefaultHideDirs(home), s.Isolation.HideDirs...), tree.Store.Dir, s.LogDir),
		HideFiles: append(verify.DefaultHideFiles(home), s.Isolation.HideFiles...),
		Bwrap:     s.Isolation.Bwrap}
	if s.AgentsRoot != "" {
		req.HideDirs = append(req.HideDirs, s.AgentsRoot)
	}
	cacheRoot := s.Isolation.CacheRoot
	if cacheRoot == "" {
		cacheRoot = filepath.Join(s.stateDir(), "cache")
	}
	req.CacheDir = filepath.Join(cacheRoot, t.ProjectID)
	err = s.runtime(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT root FROM project_repo WHERE project_id <> $1`, t.ProjectID)
		if err != nil {
			return err
		}
		others, err := pgx.CollectRows(rows, pgx.RowTo[string])
		req.HideDirs = append(req.HideDirs, others...)
		return err
	})
	for _, c := range t.RequiredChecks {
		req.Checks = append(req.Checks, verify.Check{ID: c.ID, Command: c.Command, Network: c.Network})
	}
	return req, err
}
