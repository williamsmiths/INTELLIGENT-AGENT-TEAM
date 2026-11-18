package work

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// CreatePrincipal creates an agent principal with its first membership and
// returns its token (PM-H-02; owner only; actor scope). The token is returned
// this one time; only its hash is stored.
func CreatePrincipal(ctx context.Context, tx pgx.Tx, actor authn.Principal, in NewPrincipal) (Identity, string, error) {
	if err := requireOwner(actor); err != nil {
		return Identity{}, "", err
	}
	if in.Enforcement == "" {
		in.Enforcement = "SUPERVISED"
	}
	if strings.TrimSpace(in.Name) == "" || (in.Enforcement != "HOOK" && in.Enforcement != "SUPERVISED") {
		return Identity{}, "", kerr.New(kerr.Invalid, "name is required and enforcement must be HOOK or SUPERVISED")
	}
	if err := checkMembership(ctx, tx, in.ProjectID, in.Role); err != nil {
		return Identity{}, "", err
	}
	token, hash, err := authn.NewToken()
	if err != nil {
		return Identity{}, "", err
	}
	var id string
	err = tx.QueryRow(ctx, `INSERT INTO principal (kind, name, openbot_agent_id, token_hash, enforcement)
		VALUES ('agent', $1, nullif($2, ''), $3, $4) RETURNING id`,
		in.Name, in.OpenBotAgentID, hash, in.Enforcement).Scan(&id)
	if err != nil {
		return Identity{}, "", conflict(err, "this OpenBot agent already has a principal")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_member (project_id, principal_id, role) VALUES ($1, $2, $3)`,
		in.ProjectID, id, in.Role); err != nil {
		return Identity{}, "", err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: in.ProjectID, Type: "PRINCIPAL_CREATED",
		Actor: actor.ID, Payload: map[string]any{"principal_id": id, "role": in.Role}}); err != nil {
		return Identity{}, "", err
	}
	out, err := loadIdentity(ctx, tx, id)
	return out, token, err
}

// AddMembership adds an existing agent principal to another project (PM-H-02;
// owner only; actor scope).
func AddMembership(ctx context.Context, tx pgx.Tx, actor authn.Principal, principalID, projectID, role string) (Identity, error) {
	if err := requireOwner(actor); err != nil {
		return Identity{}, err
	}
	if !validID(principalID) {
		return Identity{}, notFound("principal")
	}
	who, err := loadIdentity(ctx, tx, principalID)
	if err != nil {
		return Identity{}, err
	}
	if who.Kind != "agent" {
		return Identity{}, kerr.New(kerr.Invalid, "only an agent principal can be a project member")
	}
	if err := checkMembership(ctx, tx, projectID, role); err != nil {
		return Identity{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO project_member (project_id, principal_id, role) VALUES ($1, $2, $3)`,
		projectID, principalID, role); err != nil {
		return Identity{}, conflict(err, "principal is already a member of this project")
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "MEMBER_ADDED",
		Actor: actor.ID, Payload: map[string]any{"principal_id": principalID, "role": role}}); err != nil {
		return Identity{}, err
	}
	return loadIdentity(ctx, tx, principalID)
}

// checkMembership validates the project and role given in a request body (422).
func checkMembership(ctx context.Context, tx pgx.Tx, projectID, role string) error {
	if role != "lead" && role != "worker" {
		return kerr.New(kerr.Invalid, "role must be lead or worker")
	}
	var ok bool
	if validID(projectID) {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM project WHERE id = $1)`, projectID).Scan(&ok); err != nil {
			return err
		}
	}
	if !ok {
		return kerr.New(kerr.Invalid, "project does not exist")
	}
	return nil
}

// Me returns the caller with its memberships and its live lease, which names
// the project and task every agent request is bound to (GET /me, AC-H-083;
// actor scope). An overdue lease is not reported (T-H-23).
func Me(ctx context.Context, tx pgx.Tx, actor authn.Principal) (Identity, error) {
	return loadIdentity(ctx, tx, actor.ID)
}

// LookupPrincipal loads any principal with its project roles, for the Runtime
// to notify the holder of a lease. Caller must use a trusted scope.
func LookupPrincipal(ctx context.Context, tx pgx.Tx, principalID string) (authn.Principal, error) {
	who, err := loadIdentity(ctx, tx, principalID)
	if err != nil {
		return authn.Principal{}, err
	}
	p := authn.Principal{ID: who.ID, Kind: who.Kind, Name: who.Name, OpenBotAgentID: who.OpenBotAgentID,
		Enforcement: who.Enforcement, Roles: map[string]string{}}
	for _, m := range who.Memberships {
		p.Roles[m.ProjectID] = m.Role
	}
	return p, nil
}

func loadIdentity(ctx context.Context, tx pgx.Tx, id string) (Identity, error) {
	if !validID(id) {
		return Identity{}, notFound("principal")
	}
	var who Identity
	err := tx.QueryRow(ctx, `SELECT id, kind, name, COALESCE(openbot_agent_id, ''), enforcement FROM principal WHERE id = $1`,
		id).Scan(&who.ID, &who.Kind, &who.Name, &who.OpenBotAgentID, &who.Enforcement)
	if errors.Is(err, pgx.ErrNoRows) {
		return Identity{}, notFound("principal")
	}
	if err != nil {
		return Identity{}, err
	}
	rows, err := tx.Query(ctx, `SELECT project_id, role FROM project_member WHERE principal_id = $1 ORDER BY project_id`, id)
	if err != nil {
		return Identity{}, err
	}
	if who.Memberships, err = pgx.CollectRows(rows, pgx.RowToStructByPos[Membership]); err != nil {
		return Identity{}, err
	}
	who.Lease, err = scanLease(tx.QueryRow(ctx,
		`SELECT `+leaseCols+` FROM lease WHERE principal_id = $1 AND state = 'ACTIVE' AND expires_at >= now()`, id))
	return who, err
}
