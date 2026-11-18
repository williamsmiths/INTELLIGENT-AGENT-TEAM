package memory

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Relation is one edge of the SQL knowledge graph (spec §0). It has no trust of
// its own: an item pulled in through it carries its own trust.
type Relation struct {
	ID        string
	ProjectID string
	FromKey   string
	ToKey     string
	Type      string
	CreatedBy string
	ValidFrom time.Time
	ValidTo   *time.Time // nil while live
}

// RelationInput describes a new relation between two keys of one project.
type RelationInput struct {
	ProjectID string
	FromKey   string
	ToKey     string
	Type      string
}

const relationCols = `id::text, project_id::text, from_key, to_key, type, created_by::text, valid_from, valid_to`

func scanRelation(row pgx.Row) (Relation, error) {
	var r Relation
	err := row.Scan(&r.ID, &r.ProjectID, &r.FromKey, &r.ToKey, &r.Type, &r.CreatedBy, &r.ValidFrom, &r.ValidTo)
	return r, err
}

// CreateRelation links two current PROJECT-level keys of the same project
// (PM-H-21). Keys of another project, missing keys and AGENT / GLOBAL keys are 422 (AC-H-103).
func CreateRelation(ctx context.Context, tx pgx.Tx, actor authn.Principal, in RelationInput) (Relation, error) {
	if _, err := requireMember(ctx, tx, actor, in.ProjectID); err != nil {
		return Relation{}, err
	}
	from, to := strings.TrimSpace(in.FromKey), strings.TrimSpace(in.ToKey)
	if !oneOf(in.Type, relationTypes) || from == "" || to == "" || from == to {
		return Relation{}, kerr.New(kerr.Invalid, "relation needs a valid type and two different keys")
	}
	var ends int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM knowledge
		WHERE project_id = $1 AND level = 'PROJECT' AND is_current AND key IN ($2, $3)`, in.ProjectID, from, to).Scan(&ends)
	if err != nil {
		return Relation{}, wrap("check relation ends", err)
	}
	if ends != 2 {
		return Relation{}, kerr.New(kerr.Invalid, "both ends must be current PROJECT-level keys of this project")
	}
	var dup bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM relation
		WHERE project_id = $1 AND from_key = $2 AND to_key = $3 AND type = $4 AND valid_to IS NULL)`,
		in.ProjectID, from, to, in.Type).Scan(&dup)
	if err != nil {
		return Relation{}, wrap("check relation", err)
	}
	if dup {
		return Relation{}, kerr.New(kerr.Conflict, "this relation is already live")
	}
	r, err := scanRelation(tx.QueryRow(ctx, `INSERT INTO relation (project_id, from_key, to_key, type, created_by)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+relationCols, in.ProjectID, from, to, in.Type, actor.ID))
	if uniqueViolation(err) != "" {
		return Relation{}, kerr.New(kerr.Conflict, "this relation is already live")
	}
	return r, wrap("insert relation", err)
}

// CloseRelation ends a relation (valid_to); it is never deleted. Owner, or the
// agent that created it (PM-H-21); another member gets 403 (AC-H-103).
func CloseRelation(ctx context.Context, tx pgx.Tx, actor authn.Principal, id string) (Relation, error) {
	if !isUUID(id) {
		return Relation{}, kerr.New(kerr.NotFound, "relation not found")
	}
	r, err := scanRelation(tx.QueryRow(ctx, `SELECT `+relationCols+` FROM relation WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && actor.RoleIn(r.ProjectID) == "") {
		return Relation{}, kerr.New(kerr.NotFound, "relation not found")
	}
	if err != nil {
		return Relation{}, wrap("load relation", err)
	}
	if !actor.IsOwner() && r.CreatedBy != actor.ID {
		return Relation{}, kerr.New(kerr.Forbidden, "only the owner or the creator closes a relation")
	}
	if r.ValidTo != nil {
		return Relation{}, kerr.New(kerr.Conflict, "relation is already closed")
	}
	out, err := scanRelation(tx.QueryRow(ctx, `UPDATE relation SET valid_to = now()
		WHERE id = $1 AND valid_to IS NULL RETURNING `+relationCols, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Relation{}, kerr.New(kerr.Conflict, "relation is already closed")
	}
	return out, wrap("close relation", err)
}

// ListRelations returns the live relations touching one key of a project (PM-H-21).
func ListRelations(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID, key string) ([]Relation, error) {
	if _, err := requireMember(ctx, tx, actor, projectID); err != nil {
		return nil, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, kerr.New(kerr.Invalid, "key is required")
	}
	rows, err := tx.Query(ctx, `SELECT `+relationCols+` FROM relation
		WHERE project_id = $1 AND valid_to IS NULL AND (from_key = $2 OR to_key = $2)
		ORDER BY type, from_key, to_key, id`, projectID, key)
	if err != nil {
		return nil, wrap("list relations", err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Relation, error) { return scanRelation(row) })
	return out, wrap("list relations", err)
}
