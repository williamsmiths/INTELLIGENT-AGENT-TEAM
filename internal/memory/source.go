package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Source is one immutable raw source (PM-H-04).
type Source struct {
	ID          string
	ProjectID   string
	Kind        string
	Origin      string // OWNER or AGENT, set by the server
	URI         string
	Content     string
	ContentHash string
	CreatedBy   string
	CreatedAt   time.Time
}

// SourceInput is what a caller may say about a source. Origin is not an input.
type SourceInput struct {
	ProjectID string
	Kind      string
	URI       string
	Content   string
}

const sourceCols = `id::text, project_id::text, kind, origin, uri, content, content_hash, created_by::text, created_at`

func scanSource(row pgx.Row) (Source, error) {
	var s Source
	err := row.Scan(&s.ID, &s.ProjectID, &s.Kind, &s.Origin, &s.URI, &s.Content, &s.ContentHash, &s.CreatedBy, &s.CreatedAt)
	return s, err
}

// CreateSource stores an immutable source in a project the actor belongs to
// (PM-H-04). Origin comes from the actor, never from the input.
func CreateSource(ctx context.Context, tx pgx.Tx, actor authn.Principal, in SourceInput) (Source, error) {
	if _, err := requireMember(ctx, tx, actor, in.ProjectID); err != nil {
		return Source{}, err
	}
	if strings.TrimSpace(in.Kind) == "" || in.Content == "" {
		return Source{}, kerr.New(kerr.Invalid, "source needs kind and content")
	}
	origin := "AGENT"
	if actor.IsOwner() {
		origin = "OWNER"
	}
	sum := sha256.Sum256([]byte(in.Content))
	s, err := scanSource(tx.QueryRow(ctx,
		`INSERT INTO source (project_id, kind, origin, uri, content, content_hash, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+sourceCols,
		in.ProjectID, strings.TrimSpace(in.Kind), origin, in.URI, in.Content, hex.EncodeToString(sum[:]), actor.ID))
	if err != nil {
		return Source{}, wrap("insert source", err)
	}
	return s, nil
}

// GetSource reads one source; outside the actor's projects it does not exist (PM-H-04).
func GetSource(ctx context.Context, tx pgx.Tx, actor authn.Principal, id string) (Source, error) {
	if !isUUID(id) {
		return Source{}, kerr.New(kerr.NotFound, "source not found")
	}
	s, err := scanSource(tx.QueryRow(ctx, `SELECT `+sourceCols+` FROM source WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Source{}, kerr.New(kerr.NotFound, "source not found")
	}
	if err != nil {
		return Source{}, wrap("load source", err)
	}
	if actor.RoleIn(s.ProjectID) == "" {
		return Source{}, kerr.New(kerr.NotFound, "source not found")
	}
	return s, nil
}

// sourcesInProject reports whether every id is a source of the project (T-H-30).
func sourcesInProject(ctx context.Context, tx pgx.Tx, projectID string, ids []string) (bool, error) {
	seen := map[string]bool{}
	for _, id := range ids {
		if !isUUID(id) {
			return false, nil
		}
		seen[strings.ToLower(id)] = true
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM source WHERE project_id = $1 AND id = ANY($2::uuid[])`,
		projectID, ids).Scan(&n)
	if err != nil {
		return false, wrap("check sources", err)
	}
	return n == len(seen), nil
}
