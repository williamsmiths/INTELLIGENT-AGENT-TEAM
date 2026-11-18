package memory

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// CreateInput describes a new knowledge item (version 1).
type CreateInput struct {
	Level     string // PROJECT (default), AGENT or GLOBAL
	ProjectID string // required for PROJECT and AGENT, empty for GLOBAL
	Key       string
	Kind      string
	Content
}

// checkContent applies the trust rules of T-H-30 / T-H-32 / T-H-33 to the content
// of a new version. A wrong role is 403 before any 422 (AC-H-003).
func checkContent(ctx context.Context, tx pgx.Tx, actor authn.Principal, level, projectID, kind string, c *Content) error {
	if c.Trust == TrustAuthoritative && !actor.IsOwner() {
		return kerr.New(kerr.Forbidden, "only the owner writes AUTHORITATIVE knowledge") // PM-H-06
	}
	if !oneOf(c.Trust, []string{TrustCandidate, TrustObserved, TrustAuthoritative}) {
		return kerr.New(kerr.Invalid, "trust must be CANDIDATE, OBSERVED or AUTHORITATIVE")
	}
	if !oneOf(c.Aspect, aspects) {
		return kerr.New(kerr.Invalid, "aspect must be AS_IS or TO_BE")
	}
	c.Title = strings.TrimSpace(c.Title)
	c.Scope = strings.TrimSpace(c.Scope)
	if c.Title == "" {
		return kerr.New(kerr.Invalid, "title is required")
	}
	if c.Basis != "" && !oneOf(c.Basis, bases) {
		return kerr.New(kerr.Invalid, "basis must be proposed, inferred or unknown")
	}
	if c.Trust == TrustCandidate && c.Basis == "" {
		return kerr.New(kerr.Invalid, "a CANDIDATE item needs a basis") // PM-H-05
	}
	if c.Trust != TrustCandidate {
		c.Basis = ""
	}
	if level == LevelGlobal {
		// GLOBAL holds generalized experience only, never project data (spec §0, AC-H-085).
		if kind != "experience" || strings.TrimSpace(c.AppliesWhen) == "" || strings.TrimSpace(c.Limits) == "" {
			return kerr.New(kerr.Invalid, "GLOBAL knowledge must be kind experience with applies_when and limits")
		}
		if len(c.SourceIDs) > 0 || c.Trust == TrustObserved {
			return kerr.New(kerr.Invalid, "GLOBAL knowledge has no source_ids")
		}
		return nil
	}
	if c.Trust == TrustObserved && (c.Aspect != AspectAsIs || len(c.SourceIDs) == 0) {
		return kerr.New(kerr.Invalid, "OBSERVED needs aspect AS_IS and at least one source") // T-H-30
	}
	if len(c.SourceIDs) > 0 {
		ok, err := sourcesInProject(ctx, tx, projectID, c.SourceIDs)
		if err != nil {
			return err
		}
		if !ok {
			return kerr.New(kerr.Invalid, "source_ids must be sources of the same project")
		}
	}
	return nil
}

// CreateKnowledge adds version 1 of a key (T-H-30, T-H-33; PM-H-05, PM-H-06).
// Writing content the same author already stores returns the existing row with
// Created=false and no journal event (AC-H-101).
func CreateKnowledge(ctx context.Context, tx pgx.Tx, actor authn.Principal, in CreateInput) (WriteResult, error) {
	level := in.Level
	if level == "" {
		level = LevelProject
	}
	projectID := in.ProjectID
	if level == LevelGlobal {
		if projectID != "" {
			return WriteResult{}, kerr.New(kerr.Invalid, "GLOBAL knowledge has no project")
		}
	} else if _, err := requireMember(ctx, tx, actor, projectID); err != nil {
		return WriteResult{}, err // AC-H-004: outside the project it does not exist
	}
	key := strings.TrimSpace(in.Key)
	if !oneOf(level, levels) || !oneOf(in.Kind, kinds) || key == "" {
		return WriteResult{}, kerr.New(kerr.Invalid, "level, kind and key must be valid")
	}
	c := in.Content
	if err := checkContent(ctx, tx, actor, level, projectID, in.Kind, &c); err != nil {
		return WriteResult{}, err
	}
	if level == LevelGlobal && c.Trust != TrustCandidate {
		return WriteResult{}, kerr.New(kerr.Invalid, "a GLOBAL experience is proposed as CANDIDATE and confirmed by the owner")
	}
	it := Item{Level: level, ProjectID: projectID, Key: key, NS: nsFor(level, c.Trust, actor.ID), Version: 1,
		Kind: in.Kind, Aspect: c.Aspect, Trust: c.Trust, Basis: c.Basis, Scope: c.Scope, Title: c.Title, Body: c.Body,
		AppliesWhen: c.AppliesWhen, Limits: c.Limits, SourceIDs: c.SourceIDs, CreatedBy: actor.ID,
		ContentHash: contentHash(level, projectID, actor.ID, in.Kind, c)}

	// AC-H-101: same author, same content → the old row, whatever key was sent.
	if old, found, err := currentByHash(ctx, tx, it); err != nil || found {
		return WriteResult{Item: old}, err
	}
	var used bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM knowledge k
		WHERE k.project_id IS NOT DISTINCT FROM nullif($1,'')::uuid AND k.level = $2
		  AND k.ns IS NOT DISTINCT FROM nullif($3,'')::uuid AND k.key = $4)`,
		projectID, level, it.NS, key).Scan(&used)
	if err != nil {
		return WriteResult{}, wrap("check key", err)
	}
	if used {
		return WriteResult{}, kerr.New(kerr.Conflict, "key %q is already used; supersede its current version or choose another key", key)
	}
	out, ok, err := insertVersion(ctx, tx, it)
	if err != nil {
		return WriteResult{}, err
	}
	if !ok { // a concurrent identical write won the race
		old, found, err := currentByHash(ctx, tx, it)
		if err == nil && !found {
			err = kerr.New(kerr.Conflict, "the same content is being written concurrently")
		}
		return WriteResult{Item: old}, err
	}
	res := WriteResult{Item: out, Created: true}
	ev := journal.Event{ProjectID: projectID, Type: EventAdded, Payload: eventPayload(out, nil)}
	return res, emit(ctx, tx, actor, ev, &res.Deferred)
}

// currentByHash finds the current row of the same author with the same content.
func currentByHash(ctx context.Context, tx pgx.Tx, it Item) (Item, bool, error) {
	old, err := scanItem(tx.QueryRow(ctx, `SELECT `+itemCols+` FROM knowledge k
		WHERE k.project_id IS NOT DISTINCT FROM nullif($1,'')::uuid AND k.level = $2
		  AND k.created_by = $3 AND k.content_hash = $4 AND k.is_current`,
		it.ProjectID, it.Level, it.CreatedBy, it.ContentHash))
	if errors.Is(err, pgx.ErrNoRows) {
		return Item{}, false, nil
	}
	return old, err == nil, wrap("look up content hash", err)
}
