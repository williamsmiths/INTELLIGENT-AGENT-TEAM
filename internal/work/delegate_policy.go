package work

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// DelegatePolicy is what the Owner sets for every task a Lead delegates in a
// project (spec S2 E.1-4, E.3, PM-S2-21): the checks and the protected paths
// are copied into the task; the Lead chooses neither.
type DelegatePolicy struct {
	RequiredChecks []Check  `json:"required_checks"`
	ProtectedPaths []string `json:"protected_paths"`
}

// Validate refuses a policy that would make every delegate fail later inside
// CreateTask (REV-006b N4): checks as CreateTask wants them, protected paths
// relative, non-empty and without "..".
func (p DelegatePolicy) Validate() error {
	if err := validateChecks(p.RequiredChecks); err != nil {
		return err
	}
	for _, g := range p.ProtectedPaths {
		if strings.TrimSpace(g) == "" || strings.HasPrefix(g, "/") || slices.Contains(strings.Split(g, "/"), "..") {
			return kerr.New(kerr.Invalid, "protected_paths must be non-empty paths relative to the repo root, without '..'")
		}
	}
	return nil
}

func (p DelegatePolicy) orEmpty() DelegatePolicy {
	if p.RequiredChecks == nil {
		p.RequiredChecks = []Check{}
	}
	p.ProtectedPaths = orEmpty(p.ProtectedPaths)
	return p
}

// loadDelegatePolicy reads the policy of a project the scope can see.
func loadDelegatePolicy(ctx context.Context, tx pgx.Tx, projectID string) (DelegatePolicy, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT delegate_policy FROM project WHERE id = $1`, projectID).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return DelegatePolicy{}, notFound("project")
	}
	if err != nil {
		return DelegatePolicy{}, err
	}
	var p DelegatePolicy
	if err := json.Unmarshal(raw, &p); err != nil {
		return DelegatePolicy{}, err
	}
	return p.orEmpty(), nil
}

// GetDelegatePolicy returns the policy to the Owner or a Lead of the project
// (spec S2 E.5, PM-S2-24; actor scope). A Worker gets 403.
func GetDelegatePolicy(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string) (DelegatePolicy, error) {
	if !validID(projectID) {
		return DelegatePolicy{}, notFound("project")
	}
	if err := requireManager(actor, projectID, "project"); err != nil {
		return DelegatePolicy{}, err
	}
	return loadDelegatePolicy(ctx, tx, projectID)
}

// SetDelegatePolicy replaces the policy of a project (PUT
// /v1/projects/{p}/delegate-policy, PM-S2-21; owner only; actor scope).
func SetDelegatePolicy(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string, p DelegatePolicy) (DelegatePolicy, error) {
	if err := requireOwner(actor); err != nil {
		return DelegatePolicy{}, err
	}
	if !validID(projectID) {
		return DelegatePolicy{}, notFound("project")
	}
	if err := p.Validate(); err != nil {
		return DelegatePolicy{}, err
	}
	p = p.orEmpty()
	raw, err := json.Marshal(p)
	if err != nil {
		return DelegatePolicy{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE project SET delegate_policy = $2 WHERE id = $1`, projectID, raw)
	if err != nil {
		return DelegatePolicy{}, err
	}
	if tag.RowsAffected() == 0 {
		return DelegatePolicy{}, notFound("project")
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "DELEGATE_POLICY_SET", Actor: actor.ID,
		Payload: map[string]any{"checks": len(p.RequiredChecks), "protected_paths": len(p.ProtectedPaths)}})
	return p, err
}
