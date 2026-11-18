package embed_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/embed/embedtest"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// C.11: only the runtime writes knowledge_embedding; nobody deletes; a row is
// readable exactly when its knowledge row is (AC-S2-022, table level).
func TestEmbeddingTableAccess(t *testing.T) {
	d, fake := newDB(t), embedtest.New(t)
	mine, theirs := d.knowledge(d.project, "R-1", "một"), d.knowledge(d.other, "R-2", "hai")
	d.step(&embed.Worker{Pool: d.pool, Client: fake.Client()}, 2)
	vec := embed.Literal(fake.Vector("x"))

	for name, sc := range map[string]store.Scope{"agent": d.agent, "owner": d.owner} {
		err := store.InScope(d.ctx, d.pool, sc, func(tx pgx.Tx) error {
			_, err := tx.Exec(d.ctx, `INSERT INTO knowledge_embedding (knowledge_id, model, dim, content_hash, embedding)
				VALUES ($1, 'm', 1024, 'h', $2::text::vector)`, mine, vec)
			return err
		})
		if err == nil {
			t.Errorf("%s inserted an embedding", name)
		}
		d.must(sc, func(tx pgx.Tx) error {
			tag, err := tx.Exec(d.ctx, `UPDATE knowledge_embedding SET embedding = $1::text::vector`, vec)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("%s rewrote %d embeddings", name, tag.RowsAffected())
			}
			return err
		})
	}
	for name, sc := range map[string]store.Scope{"agent": d.agent, "owner": d.owner, "runtime": store.Runtime} {
		err := store.InScope(d.ctx, d.pool, sc, func(tx pgx.Tx) error {
			_, err := tx.Exec(d.ctx, `DELETE FROM knowledge_embedding`)
			return err
		})
		if err == nil {
			t.Errorf("%s may DELETE from knowledge_embedding", name)
		}
	}
	d.must(d.agent, func(tx pgx.Tx) error {
		rows, err := tx.Query(d.ctx, `SELECT knowledge_id::text FROM knowledge_embedding ORDER BY embedding <=> $1::text::vector`, vec)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if len(ids) != 1 || ids[0] != mine {
			t.Errorf("agent of %s sees embeddings %v, want only %s (not %s)", d.project, ids, mine, theirs)
		}
		return err
	})
	var n int // a connection without a scope sees nothing
	if err := d.pool.QueryRow(d.ctx, `SELECT count(*) FROM knowledge_embedding`).Scan(&n); err != nil || n != 0 {
		t.Errorf("no scope: %d rows, %v", n, err)
	}
}
