package memory

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
)

// StageSemantic is the fifth retrieval stage (spec S2 B.1): vector similarity.
// It only appends rows the structured stages did not return (AC-S2-021).
const StageSemantic = "semantic"

// Values of Info.Semantic.
const (
	SemanticOK          = "ok"          // the stage ran, or was not needed because earlier stages filled the limit
	SemanticUnavailable = "unavailable" // the sidecar or the vector query failed or was too slow (AC-S2-023)
	SemanticOff         = "off"         // no searcher installed, or the query has no text
)

// Info says how a search was answered beyond its hits.
type Info struct {
	Semantic string
}

// SemanticQuery is what the vector stage needs for one query text.
type SemanticQuery struct {
	Model  string
	Dim    int
	Vector []float32 // not modified by this package
	MinSim float64   // minimum cosine similarity of a hit
}

// SemanticSearcher embeds a query text. It must honour ctx: the caller gives
// it semanticDeadline and goes on without it afterwards.
type SemanticSearcher interface {
	Query(ctx context.Context, text string) (SemanticQuery, error)
}

// semanticDeadline bounds the embedding call of one search (AC-S2-023). Only
// that call is bounded: the vector SQL runs in the caller's transaction with
// the caller's context, like the other stages.
const semanticDeadline = 800 * time.Millisecond

var semantic atomic.Pointer[SemanticSearcher]

// UseSemantic installs the searcher of the semantic stage; nil turns it off.
func UseSemantic(s SemanticSearcher) {
	if s == nil {
		semantic.Store(nil)
		return
	}
	semantic.Store(&s)
}

// EnableSemantic installs a searcher backed by the sidecar client (query
// embeddings go through the client's bounded cache) and returns the worker
// that embeds knowledge in the background; the caller runs it. A nil client
// turns the stage off and returns nil (Run on a nil worker returns at once).
func EnableSemantic(pool *pgxpool.Pool, client *embed.Client, minSim float64) *embed.Worker {
	if client == nil {
		UseSemantic(nil)
		return nil
	}
	UseSemantic(clientSearcher{client: client, minSim: minSim})
	return &embed.Worker{Pool: pool, Client: client}
}

type clientSearcher struct {
	client *embed.Client
	minSim float64
}

func (s clientSearcher) Query(ctx context.Context, text string) (SemanticQuery, error) {
	vec, err := s.client.EmbedQuery(ctx, text)
	return SemanticQuery{Model: s.client.Model, Dim: s.client.Dim, Vector: vec, MinSim: s.minSim}, err
}

// embedQuery asks the installed searcher within semanticDeadline. The status is
// SemanticOff without a searcher and SemanticUnavailable on any failure.
func embedQuery(ctx context.Context, text string) (SemanticQuery, string) {
	sp := semantic.Load()
	if sp == nil {
		return SemanticQuery{}, SemanticOff
	}
	ctx, cancel := context.WithTimeout(ctx, semanticDeadline)
	defer cancel()
	type answer struct {
		q   SemanticQuery
		err error
	}
	done := make(chan answer, 1) // buffered: a late answer is dropped, not leaked
	go func() {
		q, err := (*sp).Query(ctx, text)
		done <- answer{q, err}
	}()
	select {
	case a := <-done:
		if a.err != nil || a.q.Dim <= 0 || len(a.q.Vector) != a.q.Dim || !embed.ValidModel(a.q.Model) {
			return SemanticQuery{}, SemanticUnavailable
		}
		return a.q, SemanticOK
	case <-ctx.Done():
		return SemanticQuery{}, SemanticUnavailable
	}
}

// semanticFill returns up to slots rows that pass f, are not in seen and are at
// least MinSim similar to text, best first. f is the caller's own visibility
// filter, the one the lexical stages use, so scope is decided in the same SQL
// statement and never afterwards (AC-S2-022); row-level security of knowledge
// and knowledge_embedding applies on top. Any failure leaves the caller's
// transaction usable and yields no rows (AC-S2-023).
func semanticFill(ctx context.Context, tx pgx.Tx, f filter, text string, seen []string, slots int) ([]Hit, string) {
	if semantic.Load() == nil {
		return nil, SemanticOff
	}
	if slots <= 0 {
		return nil, SemanticOK // AC-S2-021: nothing to add, the sidecar is not asked
	}
	q, status := embedQuery(ctx, text)
	if status != SemanticOK {
		return nil, status
	}
	// A savepoint: a failing vector statement must not abort the caller's transaction.
	sp, err := tx.Begin(ctx)
	if err != nil {
		return nil, SemanticUnavailable
	}
	hits, err := semanticStage(ctx, sp, f, q, seen, slots)
	if err != nil {
		_ = sp.Rollback(ctx)
		return nil, SemanticUnavailable
	}
	if err := sp.Commit(ctx); err != nil {
		return nil, SemanticUnavailable
	}
	return hits, SemanticOK
}

func semanticStage(ctx context.Context, tx pgx.Tx, f filter, q SemanticQuery, seen []string, limit int) ([]Hit, error) {
	// If the planner walks the HNSW index, keep walking until enough rows pass
	// the scope filter instead of filtering a fixed candidate list (pgvector ≥ 0.8).
	// An older pgvector does not know the setting; its own savepoint keeps that harmless.
	if cfg, err := tx.Begin(ctx); err == nil {
		if _, err := cfg.Exec(ctx, `SELECT set_config('hnsw.iterative_scan', 'strict_order', true)`); err != nil {
			_ = cfg.Rollback(ctx)
		} else if err := cfg.Commit(ctx); err != nil {
			return nil, wrap("search stage "+StageSemantic, err)
		}
	}
	if seen == nil {
		seen = []string{}
	}
	n := len(f.args)
	// The cast and the model literal match knowledge_embedding_bge_m3_idx. The
	// query vector is a scalar sub-select so that its text is parsed once per
	// statement, not once per row, whatever plan the statement cache holds.
	dist := fmt.Sprintf("(e.embedding::vector(%d) <=> (SELECT $%d::text::vector(%d)))", q.Dim, n+1, q.Dim)
	sql := fmt.Sprintf(`SELECT %s, (1 - %s)::float8 AS score
		FROM knowledge_embedding e JOIN knowledge k ON k.id = e.knowledge_id
		WHERE %s AND e.model = '%s' AND e.dim = %d AND e.content_hash = k.content_hash
		  AND k.id <> ALL($%d::uuid[]) AND %s <= $%d::float8
		ORDER BY %s, k.id LIMIT $%d`,
		itemCols, dist, f.where, q.Model, q.Dim, n+2, dist, n+3, dist, n+4)
	args := append(append([]any{}, f.args...), embed.Literal(q.Vector), seen, 1-q.MinSim, limit)
	return queryHits(ctx, tx, StageSemantic, sql, args)
}

// withSemantic appends the semantic stage to the hits of the structured stages.
func withSemantic(ctx context.Context, tx pgx.Tx, f filter, q string, limit int, hits []Hit) ([]Hit, Info) {
	if q == "" {
		return hits, Info{Semantic: SemanticOff}
	}
	seen := make([]string, len(hits))
	for i, h := range hits {
		seen[i] = h.Item.ID
	}
	more, status := semanticFill(ctx, tx, f, q, seen, limit-len(hits))
	return append(hits, more...), Info{Semantic: status}
}
