package memory

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

const docExpr = `kch_unaccent(k.title || ' ' || k.body)` // must match knowledge_trgm_idx

// stageSQL describes one retrieval stage; $Q stands for the query text parameter.
type stageSQL struct {
	name  string
	cond  string
	score string
	order string
}

// Stages in retrieval order (spec §0): exact key → unaccented FTS → trigram.
// FTS uses plainto_tsquery so operator characters in code-like queries are
// plain text. The trigram stage uses word similarity (query against the best
// matching part of title + body) at PostgreSQL's default threshold.
var stages = []stageSQL{
	{StageKey, `k.key = $Q`, `1::float8`,
		`(k.level = 'PROJECT') DESC, k.level, k.version DESC, k.id`},
	{StageFTS, `k.search @@ plainto_tsquery('simple', kch_unaccent($Q))`,
		`ts_rank(k.search, plainto_tsquery('simple', kch_unaccent($Q)))::float8`,
		`score DESC, k.key, k.version DESC, k.id`},
	{StageTrigram, `kch_unaccent($Q) <% ` + docExpr,
		`word_similarity(kch_unaccent($Q), ` + docExpr + `)::float8`,
		`score DESC, k.key, k.version DESC, k.id`},
}

// searchStages runs the structured stages, then lets the semantic stage fill
// what is left of the limit (spec S2 B.1: semantic never reorders or replaces).
func searchStages(ctx context.Context, tx pgx.Tx, f filter, q string, limit int) ([]Hit, Info, error) {
	hits, err := runStages(ctx, tx, f, q, limit)
	if err != nil {
		return nil, Info{}, err
	}
	hits, info := withSemantic(ctx, tx, f, q, limit, hits)
	return hits, info, nil
}

// runStages runs the stages in order until limit hits are found; a row found by
// an earlier stage is not reported again. An empty query lists by filters only.
func runStages(ctx context.Context, tx pgx.Tx, f filter, q string, limit int) ([]Hit, error) {
	if q == "" {
		sql := fmt.Sprintf(`SELECT %s, 0::float8 AS score FROM knowledge k WHERE %s
			ORDER BY k.level, k.key, k.version DESC, k.id LIMIT $%d`, itemCols, f.where, len(f.args)+1)
		return queryHits(ctx, tx, StageList, sql, append(append([]any{}, f.args...), limit))
	}
	hits := []Hit{}
	seen := []string{}
	n := len(f.args)
	for _, st := range stages {
		if len(hits) >= limit {
			break
		}
		qArg := "$" + strconv.Itoa(n+1)
		cond := strings.ReplaceAll(st.cond, "$Q", qArg)
		score := strings.ReplaceAll(st.score, "$Q", qArg)
		sql := fmt.Sprintf(`SELECT %s, %s AS score FROM knowledge k
			WHERE %s AND %s AND k.id <> ALL($%d::uuid[]) ORDER BY %s LIMIT $%d`,
			itemCols, score, f.where, cond, n+2, st.order, n+3)
		more, err := queryHits(ctx, tx, st.name, sql, append(append([]any{}, f.args...), q, seen, limit-len(hits)))
		if err != nil {
			return nil, err
		}
		for _, h := range more {
			seen = append(seen, h.Item.ID)
		}
		hits = append(hits, more...)
	}
	return hits, nil
}

func queryHits(ctx context.Context, tx pgx.Tx, stage, sql string, args []any) ([]Hit, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, wrap("search stage "+stage, err)
	}
	hits, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Hit, error) {
		h := Hit{Stage: stage}
		err := row.Scan(append(itemDest(&h.Item), &h.Score)...)
		return h, err
	})
	return hits, wrap("search stage "+stage, err)
}
