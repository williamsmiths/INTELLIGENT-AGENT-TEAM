package embed_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/embed/embedtest"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// db is a fresh database with an owner, one project and one agent member.
// Knowledge is seeded with SQL here; the memory package's own tests go through its API.
type db struct {
	t              *testing.T
	ctx            context.Context
	pool           *pgxpool.Pool
	owner, agent   store.Scope
	project, other string
}

func newDB(t *testing.T) *db {
	t.Helper()
	d := &db{t: t, ctx: context.Background(), pool: testdb.New(t)}
	var ownerID, agentID string
	d.must(store.Runtime, func(tx pgx.Tx) error {
		if err := tx.QueryRow(d.ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ('owner','owner','o') RETURNING id`).Scan(&ownerID); err != nil {
			return err
		}
		return tx.QueryRow(d.ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ('agent','agent','a') RETURNING id`).Scan(&agentID)
	})
	d.owner = store.Scope{Role: "owner", Principal: ownerID}
	d.must(d.owner, func(tx pgx.Tx) error {
		for slug, id := range map[string]*string{"alpha": &d.project, "beta": &d.other} {
			if err := tx.QueryRow(d.ctx, `INSERT INTO project (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(d.ctx, `INSERT INTO project_member (project_id, principal_id, role) VALUES ($1, $2, 'worker')`, d.project, agentID)
		return err
	})
	d.agent = store.Scope{Role: "agent", Principal: agentID, Projects: []string{d.project}}
	return d
}

func (d *db) must(sc store.Scope, fn func(tx pgx.Tx) error) {
	d.t.Helper()
	if err := store.InScope(d.ctx, d.pool, sc, fn); err != nil {
		d.t.Fatalf("as %s: %v", sc.Role, err)
	}
}

// knowledge inserts one current PROJECT-level row as the owner and returns its id.
func (d *db) knowledge(projectID, key, title string) string {
	d.t.Helper()
	var id string
	d.must(d.owner, func(tx pgx.Tx) error {
		return tx.QueryRow(d.ctx, `INSERT INTO knowledge (project_id, key, version, kind, aspect, trust, title, body, content_hash, created_by)
			VALUES ($1, $2, 1, 'rule', 'TO_BE', 'AUTHORITATIVE', $3, 'nội dung', 'hash-' || $2, $4) RETURNING id`,
			projectID, key, title, d.owner.Principal).Scan(&id)
	})
	return id
}

func (d *db) count(where string, args ...any) int {
	d.t.Helper()
	var n int
	d.must(store.Runtime, func(tx pgx.Tx) error {
		return tx.QueryRow(d.ctx, `SELECT count(*) FROM knowledge_embedding WHERE `+where, args...).Scan(&n)
	})
	return n
}

func (d *db) step(w *embed.Worker, want int) {
	d.t.Helper()
	if n, err := w.Step(d.ctx); err != nil || n != want {
		d.t.Fatalf("Step embedded %d rows (err %v), want %d", n, err, want)
	}
}

func TestWorker(t *testing.T) {
	t.Run("AC-S2-020 a content hash is embedded once per model", func(t *testing.T) {
		d, fake := newDB(t), embedtest.New(t)
		w := &embed.Worker{Pool: d.pool, Client: fake.Client(), Batch: 2}
		ids := []string{d.knowledge(d.project, "R-1", "một"), d.knowledge(d.project, "R-2", "hai"), d.knowledge(d.other, "R-3", "ba")}
		d.step(w, 2)
		d.step(w, 1)
		d.step(w, 0)
		if got := fake.Texts(); len(got) != 3 || got[0] != "một\nnội dung" || fake.Requests() != 2 {
			t.Fatalf("sidecar got %q in %d requests", got, fake.Requests())
		}
		if n := d.count(`model = $1 AND dim = 1024 AND vector_dims(embedding) = 1024 AND content_hash LIKE 'hash-R-%'`, embed.DefaultModel); n != 3 {
			t.Fatalf("%d rows stored with model, dim and hash, want 3", n)
		}
		// A trust change is not a content change; a version that stopped being current is not embedded again.
		d.must(d.owner, func(tx pgx.Tx) error {
			_, err := tx.Exec(d.ctx, `UPDATE knowledge SET is_current = false, trust = 'HISTORICAL', ended_reason = 'invalidated' WHERE id = $1`, ids[0])
			return err
		})
		d.step(w, 0)
		// Only a stored hash that differs from the knowledge row's makes a row pending again.
		d.must(store.Runtime, func(tx pgx.Tx) error {
			_, err := tx.Exec(d.ctx, `UPDATE knowledge_embedding SET content_hash = 'stale' WHERE knowledge_id = $1`, ids[1])
			return err
		})
		d.step(w, 1)
		d.step(w, 0)
		if fake.Requests() != 3 || d.count(`content_hash = 'stale'`) != 0 || d.count(`true`) != 3 {
			t.Fatalf("after a stale hash: %d requests, %d rows", fake.Requests(), d.count(`true`))
		}
		// Another model is another row for the same knowledge.
		other := fake.Client()
		other.Model = "another/model"
		fake.SetModel("another/model")
		d.step(&embed.Worker{Pool: d.pool, Client: other}, 2)
		if d.count(`true`) != 5 {
			t.Fatalf("%d rows after a second model, want 5", d.count(`true`))
		}
	})

	t.Run("AC-S2-023 sidecar errors leave rows pending; a rejected row does not block the queue", func(t *testing.T) {
		d, fake := newDB(t), embedtest.New(t)
		c := fake.Client()
		c.Timeout = 150 * time.Millisecond
		w := &embed.Worker{Pool: d.pool, Client: c, Batch: 4}
		for i := range 4 {
			d.knowledge(d.project, fmt.Sprintf("R-%d", i), fmt.Sprintf("mục %d", i))
		}
		fake.SetStatus(503)
		if n, err := w.Step(d.ctx); n != 0 || !errors.Is(err, embed.ErrUnavailable) || d.count(`true`) != 0 {
			t.Fatalf("503: %d rows, %v", n, err)
		}
		fake.SetStatus(0)
		fake.SetDelay(2 * time.Second) // too slow twice: the batch shrinks 4 → 2 → 1
		for range 2 {
			if n, err := w.Step(d.ctx); n != 0 || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("timeout: %d rows, %v", n, err)
			}
		}
		fake.SetDelay(0)
		d.step(w, 1)
		d.step(w, 2)
		d.step(w, 1)
		d.step(w, 0)

		bad := d.knowledge(d.project, "BAD", "x "+embedtest.RejectMarker)
		good := d.knowledge(d.project, "GOOD", "mục tốt")
		d.step(w, 1) // the batch is refused, each row is tried alone, the good one is stored
		if d.count(`knowledge_id = $1`, good) != 1 || d.count(`knowledge_id = $1`, bad) != 0 {
			t.Fatal("the good row was not embedded past the rejected one")
		}
		before := fake.Requests()
		d.step(w, 0) // the rejected row is on hold, not retried at once
		if fake.Requests() != before {
			t.Fatal("a rejected row was sent again immediately")
		}
	})

	t.Run("AC-S2-023 Run retries after the sidecar comes back", func(t *testing.T) {
		d, fake := newDB(t), embedtest.New(t)
		fake.SetStatus(503)
		d.knowledge(d.project, "R-1", "một")
		ctx, cancel := context.WithCancel(d.ctx)
		done := make(chan struct{})
		w := &embed.Worker{Pool: d.pool, Client: fake.Client(), Interval: 10 * time.Millisecond}
		go func() { w.Run(ctx); close(done) }()
		time.Sleep(80 * time.Millisecond)
		if d.count(`true`) != 0 {
			t.Fatal("a row was stored while the sidecar failed")
		}
		fake.SetStatus(0)
		deadline := time.Now().Add(10 * time.Second)
		for d.count(`true`) != 1 {
			if time.Now().After(deadline) {
				t.Fatal("the worker did not retry after the sidecar recovered")
			}
			time.Sleep(20 * time.Millisecond)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not stop when its context ended")
		}
		(*embed.Worker)(nil).Run(d.ctx) // semantic off: returns at once
	})
}
