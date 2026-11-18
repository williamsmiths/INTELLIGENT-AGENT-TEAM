package embed

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// oneByOne isolates the rows a batch was rejected for: each row is tried alone
// and a row the sidecar still refuses is put on hold with a growing delay, so
// one bad row cannot block the queue.
func (w *Worker) oneByOne(ctx context.Context, rows []pendingRow) ([]pendingRow, [][]float32, error) {
	var ok []pendingRow
	var vecs [][]float32
	for _, r := range rows {
		v, err := w.Client.Embed(ctx, []string{r.text})
		switch {
		case errors.Is(err, ErrRejected):
			h := w.hold[r.id]
			if h == nil {
				h = &held{delay: maxBackoff / 2}
				if w.hold == nil {
					w.hold = map[string]*held{}
				}
				w.hold[r.id] = h
			}
			h.delay = min(maxHold, h.delay*2)
			h.until = time.Now().Add(h.delay)
			w.logf("embed: knowledge %s rejected by the sidecar, retry in %s: %v", r.id, h.delay, err)
		case err != nil:
			return ok, vecs, err
		default:
			ok, vecs = append(ok, r), append(vecs, v[0])
		}
	}
	return ok, vecs, nil
}

// save upserts the vectors. Only the runtime scope may write this table.
func (w *Worker) save(ctx context.Context, rows []pendingRow, vecs [][]float32) error {
	return store.InScope(ctx, w.Pool, store.Runtime, func(tx pgx.Tx) error {
		for i, r := range rows {
			if _, err := tx.Exec(ctx, `INSERT INTO knowledge_embedding (knowledge_id, model, dim, content_hash, embedding)
				VALUES ($1, $2, $3, $4, $5::text::vector)
				ON CONFLICT (knowledge_id, model) DO UPDATE SET dim = EXCLUDED.dim,
					content_hash = EXCLUDED.content_hash, embedding = EXCLUDED.embedding, created_at = now()`,
				r.id, w.Client.Model, w.Client.Dim, r.hash, Literal(vecs[i])); err != nil {
				return err
			}
			delete(w.hold, r.id)
		}
		return nil
	})
}
