package work

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// CreateProject creates a project with an empty PROJECT budget row (PM-H-01;
// owner only; actor scope). No task of it can become RUNNABLE before the owner
// sets the PROJECT cost limit (T-H-01).
func CreateProject(ctx context.Context, tx pgx.Tx, actor authn.Principal, slug, name string) (Project, error) {
	if err := requireOwner(actor); err != nil {
		return Project{}, err
	}
	if !slugRe.MatchString(slug) || strings.TrimSpace(name) == "" {
		return Project{}, kerr.New(kerr.Invalid, "slug must match [a-z0-9][a-z0-9-]{0,62} and name is required")
	}
	var id string
	if err := tx.QueryRow(ctx, `INSERT INTO project (slug, name) VALUES ($1, $2) RETURNING id`, slug, name).Scan(&id); err != nil {
		return Project{}, conflict(err, "a project with this slug already exists")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO budget (level, project_id) VALUES ('PROJECT', $1)`, id); err != nil {
		return Project{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: id, Type: "PROJECT_CREATED", Actor: actor.ID}); err != nil {
		return Project{}, err
	}
	return loadProject(ctx, tx, id)
}

// SetProjectLimits replaces max_active_leases, default_task_max_cost and the
// PROJECT budget limits (PM-H-01, PM-H-15; owner only; actor scope). The sum of
// all PROJECT cost limits must stay within the SYSTEM cost limit when SYSTEM
// has one, else 422 (AC-H-082). A limit below the used amount makes the budget
// EXHAUSTED at once (§2.4).
func SetProjectLimits(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string, in ProjectLimits) (Project, error) {
	if err := requireOwner(actor); err != nil {
		return Project{}, err
	}
	if !validID(projectID) {
		return Project{}, notFound("project")
	}
	if in.Budget.negative() || (in.MaxActiveLeases != nil && *in.MaxActiveLeases < 0) ||
		(in.DefaultTaskMaxCost != nil && *in.DefaultTaskMaxCost < 0) {
		return Project{}, kerr.New(kerr.Invalid, "limits must not be negative")
	}
	tag, err := tx.Exec(ctx, `UPDATE project SET max_active_leases = $2, default_task_max_cost_microusd = $3 WHERE id = $1`,
		projectID, in.MaxActiveLeases, in.DefaultTaskMaxCost)
	if err != nil {
		return Project{}, err
	}
	if tag.RowsAffected() == 0 {
		return Project{}, notFound("project")
	}
	b, err := scanBudget(tx.QueryRow(ctx, setProjLimits, append(in.Budget.args(), projectID)...))
	if err != nil {
		return Project{}, err
	}
	if err := checkProjectSum(ctx, tx); err != nil {
		return Project{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "PROJECT_LIMITS_SET", Actor: actor.ID,
		Payload: map[string]any{"state": b.State}}); err != nil {
		return Project{}, err
	}
	return loadProject(ctx, tx, projectID)
}

// GetProject returns a project with its repo roots and PROJECT budget
// (PM-H-01, PM-H-03; actor scope): the owner, or a member of the project.
func GetProject(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string) (Project, error) {
	if err := requireMember(actor, projectID); err != nil {
		return Project{}, err
	}
	return loadProject(ctx, tx, projectID)
}

func loadProject(ctx context.Context, tx pgx.Tx, id string) (Project, error) {
	var p Project
	err := tx.QueryRow(ctx, `SELECT id, slug, name, max_active_leases, default_task_max_cost_microusd FROM project
		WHERE id = $1`, id).Scan(&p.ID, &p.Slug, &p.Name, &p.MaxActiveLeases, &p.DefaultTaskMaxCost)
	if errors.Is(err, pgx.ErrNoRows) {
		return Project{}, notFound("project")
	}
	if err != nil {
		return Project{}, err
	}
	rows, err := tx.Query(ctx, `SELECT root FROM project_repo WHERE project_id = $1 ORDER BY root`, id)
	if err != nil {
		return Project{}, err
	}
	if p.Repos, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return Project{}, err
	}
	p.Budget, err = scanBudget(tx.QueryRow(ctx,
		`SELECT `+budgetCols+` FROM budget WHERE level = 'PROJECT' AND project_id = $1`, id))
	return p, err
}

// RegisterRepo registers a repository (or git worktree) root for a project
// (PM-H-01; owner only; actor scope). The root is stored as a realpath, must be
// an existing directory, and must not be inside, equal to, or contain a root
// registered for any project, else 422 (§0.4, AC-H-108). Registrations are
// serialized by an advisory lock, so two nested roots cannot be added at once.
//
// The trusted ignore list of the root starts as the root .gitignore stored in
// the HEAD commit at this moment — not the file in the working tree, and no
// nested .gitignore (spec S2 D.1). A root without a commit starts with none.
func RegisterRepo(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID, path string) (string, error) {
	if err := requireOwner(actor); err != nil {
		return "", err
	}
	if !validID(projectID) {
		return "", notFound("project")
	}
	if _, err := loadProject(ctx, tx, projectID); err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		return "", kerr.New(kerr.Invalid, "repo root must be an absolute path")
	}
	root, err := guard.Canonical(path, "")
	if err != nil {
		return "", kerr.New(kerr.Invalid, "repo root cannot be resolved")
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return "", kerr.New(kerr.Invalid, "repo root must be an existing directory")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kch.project_repo', 0))`); err != nil {
		return "", err
	}
	rows, err := tx.Query(ctx, `SELECT root FROM project_repo`)
	if err != nil {
		return "", err
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	for _, r := range roots {
		if guard.Inside(r, root) || guard.Inside(root, r) {
			return "", kerr.New(kerr.Invalid, "repo root is nested with a registered repo root")
		}
	}
	gctx, cancel := context.WithTimeout(ctx, gitBudget)
	defer cancel()
	ignore, err := verify.RootIgnoreAtHead(gctx, root)
	if err != nil {
		return "", fmt.Errorf("register repo: default ignore list: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_repo (project_id, root, ignore_patterns) VALUES ($1, $2, $3)`,
		projectID, root, ignore); err != nil {
		return "", conflict(err, "repo root is already registered")
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "REPO_REGISTERED", Actor: actor.ID,
		Payload: map[string]any{"ignore_patterns": len(ignore)}})
	return root, err
}

// gitBudget bounds the git work of one registration or assignment.
const gitBudget = 90 * time.Second

// RepoIgnore returns the trusted ignore list of a registered repo root
// (spec S2 D.1). Any authenticated scope may read it, as it may read the roots.
func RepoIgnore(ctx context.Context, tx pgx.Tx, root string) ([]string, error) {
	var patterns []string
	err := tx.QueryRow(ctx, `SELECT ignore_patterns FROM project_repo WHERE root = $1`, root).Scan(&patterns)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("repo root")
	}
	return patterns, err
}

// SetRepoIgnore replaces the trusted ignore list of one repo root of a project
// (spec S2 D.1; owner only; actor scope). root "" names the only root of a
// project that has exactly one. Patterns use the syntax of a root .gitignore;
// comments and blank lines are dropped.
func SetRepoIgnore(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID, root string, patterns []string) (string, []string, error) {
	if err := requireOwner(actor); err != nil {
		return "", nil, err
	}
	p, err := GetProject(ctx, tx, actor, projectID)
	if err != nil {
		return "", nil, err
	}
	if root == "" && len(p.Repos) == 1 {
		root = p.Repos[0]
	}
	clean, err := verify.CleanIgnore(patterns)
	if err != nil {
		return "", nil, err
	}
	tag, err := tx.Exec(ctx, `UPDATE project_repo SET ignore_patterns = $3 WHERE project_id = $1 AND root = $2`, p.ID, root, clean)
	if err != nil {
		return "", nil, err
	}
	if tag.RowsAffected() == 0 {
		return "", nil, kerr.New(kerr.Invalid, "path is not a registered repo root of this project")
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: p.ID, Type: "REPO_IGNORE_SET", Actor: actor.ID,
		Payload: map[string]any{"ignore_patterns": len(clean)}})
	return root, clean, err
}
