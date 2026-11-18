package work

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Additions of spec S2 part A (isolation): reference roots of a project, the
// specialty of a principal, the roots an agent may read, and the hook install
// a FULL assignment needs (PM-S2-04, AC-S2-007, AC-S2-011).

// Specialties are the values principal.specialty may take.
var Specialties = []string{"product", "engineering", "design", "quality"}

// PrincipalView is a principal as the Owner manages it: identity, specialty
// and the roots its sandbox and its tool allowlist are generated from.
type PrincipalView struct {
	Identity
	Specialty      string   `json:"specialty,omitempty"`
	RepoRoots      []string `json:"repo_roots"`      // repo roots of the projects it is a member of
	ReferenceRoots []string `json:"reference_roots"` // read-only roots of those projects
}

// Roots are the directories an agent may read because of its memberships.
type Roots struct {
	Repos      []string
	References []string
}

// PrincipalRoots returns the repo roots and reference roots of every project
// the principal is a member of. Caller must use a trusted scope.
func PrincipalRoots(ctx context.Context, tx pgx.Tx, principalID string) (Roots, error) {
	var r Roots
	if !validID(principalID) {
		return r, nil
	}
	rows, err := tx.Query(ctx, `SELECT r.root FROM project_repo r JOIN project_member m ON m.project_id = r.project_id
		WHERE m.principal_id = $1 ORDER BY r.root`, principalID)
	if err != nil {
		return r, err
	}
	if r.Repos, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		return r, err
	}
	rows, err = tx.Query(ctx, `SELECT DISTINCT ref FROM project p JOIN project_member m ON m.project_id = p.id,
		unnest(p.reference_roots) AS ref WHERE m.principal_id = $1 ORDER BY ref`, principalID)
	if err != nil {
		return r, err
	}
	r.References, err = pgx.CollectRows(rows, pgx.RowTo[string])
	return r, err
}

// GetPrincipal returns a principal with its specialty and roots (PM-S2-04;
// owner only; actor scope). `kch enroll` generates the sandbox from it.
func GetPrincipal(ctx context.Context, tx pgx.Tx, actor authn.Principal, principalID string) (PrincipalView, error) {
	if err := requireOwner(actor); err != nil {
		return PrincipalView{}, err
	}
	who, err := loadIdentity(ctx, tx, principalID)
	if err != nil {
		return PrincipalView{}, err
	}
	v := PrincipalView{Identity: who}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(specialty, '') FROM principal WHERE id = $1`, principalID).Scan(&v.Specialty); err != nil {
		return PrincipalView{}, err
	}
	roots, err := PrincipalRoots(ctx, tx, principalID)
	v.RepoRoots, v.ReferenceRoots = orEmpty(roots.Repos), orEmpty(roots.References)
	return v, err
}

// SetSpecialty sets or clears ("") the specialty of an agent principal
// (PUT /v1/principals/{id}, PM-S2-04; owner only; actor scope).
func SetSpecialty(ctx context.Context, tx pgx.Tx, actor authn.Principal, principalID, specialty string) (PrincipalView, error) {
	if err := requireOwner(actor); err != nil {
		return PrincipalView{}, err
	}
	valid := specialty == ""
	for _, s := range Specialties {
		valid = valid || s == specialty
	}
	if !valid {
		return PrincipalView{}, kerr.New(kerr.Invalid, "specialty must be product, engineering, design, quality or empty")
	}
	who, err := loadIdentity(ctx, tx, principalID)
	if err != nil {
		return PrincipalView{}, err
	}
	if who.Kind != "agent" {
		return PrincipalView{}, kerr.New(kerr.Invalid, "only an agent principal has a specialty")
	}
	if _, err := tx.Exec(ctx, `UPDATE principal SET specialty = nullif($2, '') WHERE id = $1`, principalID, specialty); err != nil {
		return PrincipalView{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{Type: "PRINCIPAL_UPDATED", Actor: actor.ID,
		Payload: map[string]any{"principal_id": principalID, "specialty": specialty}}); err != nil {
		return PrincipalView{}, err
	}
	return GetPrincipal(ctx, tx, actor, principalID)
}

// SetReferenceRoots replaces the read-only reference roots of a project
// (PUT /v1/projects/{p}/reference-roots, PM-S2-04; owner only; actor scope).
// Each root is stored as a realpath, must be an existing directory and must
// not lie inside a repo root registered for another project, else 422: a
// reference root would otherwise open another project's code for reading.
func SetReferenceRoots(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string, paths []string) ([]string, error) {
	if err := requireOwner(actor); err != nil {
		return nil, err
	}
	if !validID(projectID) {
		return nil, notFound("project")
	}
	if _, err := loadProject(ctx, tx, projectID); err != nil {
		return nil, err
	}
	// Same lock as RegisterRepo: a repo root and a reference root cannot cross each other unseen.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kch.project_repo', 0))`); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT root FROM project_repo WHERE project_id <> $1`, projectID)
	if err != nil {
		return nil, err
	}
	foreign, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	roots := []string{}
	for _, p := range paths {
		if !filepath.IsAbs(p) {
			return nil, kerr.New(kerr.Invalid, "reference root must be an absolute path")
		}
		root, err := guard.Canonical(p, "")
		if err != nil {
			return nil, kerr.New(kerr.Invalid, "reference root cannot be resolved")
		}
		if st, err := os.Stat(root); err != nil || !st.IsDir() {
			return nil, kerr.New(kerr.Invalid, "reference root must be an existing directory")
		}
		for _, f := range foreign {
			if guard.Inside(f, root) || guard.Inside(root, f) {
				return nil, kerr.New(kerr.Invalid, "reference root overlaps a repo root of another project")
			}
		}
		if !seen[root] {
			seen[root] = true
			roots = append(roots, root)
		}
	}
	sort.Strings(roots)
	if _, err := tx.Exec(ctx, `UPDATE project SET reference_roots = $2 WHERE id = $1`, projectID, roots); err != nil {
		return nil, err
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "REFERENCE_ROOTS_SET", Actor: actor.ID,
		Payload: map[string]any{"count": len(roots)}})
	return roots, err
}

// requireHookInstall refuses a FULL lease for a HOOK principal whose managed
// settings are not registered or are flagged as tampered (AC-S2-011): FULL
// promises enforcement, and without an intact install nothing enforces it.
func requireHookInstall(ctx context.Context, tx pgx.Tx, principalID, mode string) error {
	if mode != "FULL" {
		return nil
	}
	var enforcement string
	var registered, tampered bool
	err := tx.QueryRow(ctx, `SELECT p.enforcement, h.principal_id IS NOT NULL, COALESCE(h.tampered, false)
		FROM principal p LEFT JOIN hook_install h ON h.principal_id = p.id WHERE p.id = $1`, principalID).
		Scan(&enforcement, &registered, &tampered)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // not a principal: the membership check already refused it
	}
	if err != nil {
		return err
	}
	switch {
	case enforcement != "HOOK":
		return nil
	case !registered:
		return kerr.New(kerr.Conflict, "assignee has no registered hook install; run `kch enroll` for its workspace before a FULL assignment")
	case tampered:
		return kerr.New(kerr.Conflict, "the hook install of the assignee is flagged as tampered; restore it or run `kch enroll` again")
	}
	return nil
}

func orEmpty(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
