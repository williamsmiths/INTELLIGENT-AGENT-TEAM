// Package authn loads the principal of a caller and its database scope.
// Identity and project membership come only from here (spec §0: never from
// request bodies or model output).
//
// In production the caller is named by process attestation (spec S2 A.1): the
// attested OpenBot agent id selects the principal (ByAgent), and a peer of the
// owner socket is the owner principal (Owner). The bearer token lookup
// (Authenticate) is kept for tests only; harnessd has no way to enable it.
package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// ErrUnauthenticated means the token is missing or unknown.
var ErrUnauthenticated = errors.New("unauthenticated")

// Principal is the caller of one request.
type Principal struct {
	ID             string
	Kind           string // "owner" or "agent"
	Name           string
	OpenBotAgentID string
	Enforcement    string            // HOOK or SUPERVISED
	Roles          map[string]string // project id -> "lead" | "worker"
}

// IsOwner reports the global owner.
func (p Principal) IsOwner() bool { return p.Kind == "owner" }

// RoleIn returns "owner", "lead", "worker" or "" for a project.
func (p Principal) RoleIn(projectID string) string {
	if p.IsOwner() {
		return "owner"
	}
	return p.Roles[projectID]
}

// Scope is the row-level-security scope of this principal.
func (p Principal) Scope() store.Scope {
	if p.IsOwner() {
		return store.Scope{Role: "owner", Principal: p.ID}
	}
	projects := make([]string, 0, len(p.Roles))
	var leadOf []string
	for id, role := range p.Roles {
		projects = append(projects, id)
		if role == "lead" {
			leadOf = append(leadOf, id)
		}
	}
	return store.Scope{Role: "agent", Principal: p.ID, Projects: projects, LeadOf: leadOf}
}

// NewToken returns a fresh token and the hash to store. The token is shown once.
func NewToken() (token string, hash []byte, err error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", nil, err
	}
	token = "kch_" + base64.RawURLEncoding.EncodeToString(buf)
	return token, Hash(token), nil
}

// Hash is the stored form of a token.
func Hash(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// Authenticate loads the principal of a token with its project roles.
func Authenticate(ctx context.Context, pool *pgxpool.Pool, token string) (Principal, error) {
	if token == "" {
		return Principal{}, ErrUnauthenticated
	}
	return load(ctx, pool, `token_hash = $1`, Hash(token))
}

// ByAgent loads the agent principal enrolled for an attested OpenBot agent id
// (spec S2 A.1). An agent without a principal is ErrUnauthenticated.
func ByAgent(ctx context.Context, pool *pgxpool.Pool, openBotAgentID string) (Principal, error) {
	if openBotAgentID == "" {
		return Principal{}, ErrUnauthenticated
	}
	return load(ctx, pool, `kind = 'agent' AND openbot_agent_id = $1`, openBotAgentID)
}

// Owner loads the owner principal: who a peer of the owner socket is (PM-S2-01).
func Owner(ctx context.Context, pool *pgxpool.Pool) (Principal, error) {
	return load(ctx, pool, `kind = $1`, "owner")
}

// load reads one principal with its project roles under the runtime scope.
func load(ctx context.Context, pool *pgxpool.Pool, where string, arg any) (Principal, error) {
	var p Principal
	err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		var agentID *string
		err := tx.QueryRow(ctx,
			`SELECT id, kind, name, openbot_agent_id, enforcement FROM principal WHERE `+where+` ORDER BY created_at LIMIT 1`,
			arg).Scan(&p.ID, &p.Kind, &p.Name, &agentID, &p.Enforcement)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnauthenticated
		}
		if err != nil {
			return err
		}
		if agentID != nil {
			p.OpenBotAgentID = *agentID
		}
		p.Roles = map[string]string{}
		rows, err := tx.Query(ctx, `SELECT project_id, role FROM project_member WHERE principal_id = $1`, p.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pid, role string
			if err := rows.Scan(&pid, &role); err != nil {
				return err
			}
			p.Roles[pid] = role
		}
		return rows.Err()
	})
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}
