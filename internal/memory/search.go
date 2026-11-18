package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Retrieval stages, in the order they run (spec §0 "Retrieval theo thứ tự").
const (
	StageKey     = "key"     // exact key match
	StageFTS     = "fts"     // unaccented full-text search
	StageTrigram = "trigram" // trigram word similarity
	StageList    = "list"    // no query text: filters only
	// StageSemantic (semantic.go) runs last and only for the slots left empty.
)

// SearchQuery searches one project plus the confirmed GLOBAL items (PM-H-09).
type SearchQuery struct {
	ProjectID string
	Q         string
	Kind      string
	Trust     string // HISTORICAL rows appear only when asked for here (AC-H-008)
	Aspect    string
	Level     string
	Limit     int
}

// GlobalQuery lists or searches GLOBAL experiences (GET /v1/global/knowledge).
type GlobalQuery struct {
	Q     string
	Trust string
	Limit int
}

// Hit is one search result and the stage that found it.
type Hit struct {
	Item  Item
	Stage string
	Score float64 // 1 for a key match, ts_rank for fts, word similarity for trigram
}

// filter is the visibility and filter part of a search, shared by every stage.
type filter struct {
	where string
	args  []any
}

func (f *filter) and(cond string, arg any) {
	f.args = append(f.args, arg)
	f.where += fmt.Sprintf(" AND "+cond, len(f.args))
}

// applyCommon adds the kind / trust / aspect filters. Without trust=HISTORICAL
// only current versions are searched.
func (f *filter) applyCommon(kind, trust, aspect string) error {
	if (kind != "" && !oneOf(kind, kinds)) || (trust != "" && !oneOf(trust, trusts)) || (aspect != "" && !oneOf(aspect, aspects)) {
		return kerr.New(kerr.Invalid, "unknown kind, trust or aspect filter")
	}
	if trust != TrustHistorical {
		f.where += " AND k.is_current"
	}
	if trust != "" {
		f.and("k.trust = $%d", trust)
	}
	if kind != "" {
		f.and("k.kind = $%d", kind)
	}
	if aspect != "" {
		f.and("k.aspect = $%d", aspect)
	}
	return nil
}

// Search looks in one project and in the confirmed GLOBAL items: exact key, then
// full-text, then trigram, de-duplicated (AC-H-008). AGENT-level rows are the
// actor's own; the owner sees all (AC-H-004). A non-member gets 404. When a
// semantic searcher is installed its hits are appended (see SearchWithInfo).
func Search(ctx context.Context, tx pgx.Tx, actor authn.Principal, q SearchQuery) ([]Hit, error) {
	hits, _, err := SearchWithInfo(ctx, tx, actor, q)
	return hits, err
}

// SearchWithInfo is Search plus how the semantic stage went (spec S2 B.1). The
// semantic stage runs after the structured stages, under the same visibility
// filter, and only fills the slots they left (AC-S2-021, AC-S2-022). If it is
// unavailable the structured hits are returned unchanged and Info says so (AC-S2-023).
func SearchWithInfo(ctx context.Context, tx pgx.Tx, actor authn.Principal, q SearchQuery) ([]Hit, Info, error) {
	if _, err := requireMember(ctx, tx, actor, q.ProjectID); err != nil {
		return nil, Info{}, err
	}
	if q.Level != "" && !oneOf(q.Level, levels) {
		return nil, Info{}, kerr.New(kerr.Invalid, "unknown level filter")
	}
	f := filter{
		where: `((k.project_id = $1 AND (k.level = 'PROJECT' OR (k.level = 'AGENT' AND ($3 OR k.created_by = $2))))
		         OR (k.level = 'GLOBAL' AND k.trust = 'AUTHORITATIVE'))`,
		args: []any{q.ProjectID, actor.ID, actor.IsOwner()},
	}
	if q.Level != "" {
		f.and("k.level = $%d", q.Level)
	}
	if err := f.applyCommon(q.Kind, q.Trust, q.Aspect); err != nil {
		return nil, Info{}, err
	}
	return searchStages(ctx, tx, f, strings.TrimSpace(q.Q), clampLimit(q.Limit))
}

// ListGlobal lists or searches GLOBAL experiences. The owner sees every
// proposal; an agent sees confirmed items and its own proposals (PM-H-09, AC-H-085).
func ListGlobal(ctx context.Context, tx pgx.Tx, actor authn.Principal, q GlobalQuery) ([]Hit, error) {
	f := filter{
		where: `(k.level = 'GLOBAL' AND ($2 OR k.trust = 'AUTHORITATIVE' OR k.created_by = $1))`,
		args:  []any{actor.ID, actor.IsOwner()},
	}
	if err := f.applyCommon("", q.Trust, ""); err != nil {
		return nil, err
	}
	hits, _, err := searchStages(ctx, tx, f, strings.TrimSpace(q.Q), clampLimit(q.Limit))
	return hits, err
}
