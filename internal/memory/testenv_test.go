package memory_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// env is one fresh database with two projects and the principals the
// acceptance criteria talk about. Only principals, projects, memberships and
// tasks are seeded with SQL; everything else goes through the package API.
type env struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	A, B  string          // project ids
	Owner authn.Principal // global owner
	L     authn.Principal // lead of A
	W, V  authn.Principal // workers of A
	X     authn.Principal // worker of B only
	Z     authn.Principal // worker of A and B
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{t: t, ctx: context.Background(), pool: testdb.New(t)}
	mk := func(kind, name string) authn.Principal {
		p := authn.Principal{Kind: kind, Name: name, Roles: map[string]string{}}
		err := store.InScope(e.ctx, e.pool, store.Runtime, func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ($1, $2, $3) RETURNING id`,
				kind, name, []byte("token-"+name)).Scan(&p.ID)
		})
		if err != nil {
			t.Fatalf("seed principal %s: %v", name, err)
		}
		return p
	}
	e.Owner = mk("owner", "owner")
	e.L, e.W, e.V, e.X, e.Z = mk("agent", "lead-L"), mk("agent", "worker-W"), mk("agent", "worker-V"), mk("agent", "worker-X"), mk("agent", "worker-Z")
	e.A, e.B = e.project("alpha"), e.project("beta")
	e.join(&e.L, e.A, "lead")
	e.join(&e.W, e.A, "worker")
	e.join(&e.V, e.A, "worker")
	e.join(&e.X, e.B, "worker")
	e.join(&e.Z, e.A, "worker")
	e.join(&e.Z, e.B, "worker")
	return e
}

func (e *env) project(slug string) string {
	e.t.Helper()
	var id string
	e.must(e.Owner, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO project (slug, name) VALUES ($1, $1) RETURNING id`, slug).Scan(&id)
	})
	return id
}

func (e *env) join(p *authn.Principal, projectID, role string) {
	e.t.Helper()
	e.must(e.Owner, func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `INSERT INTO project_member (project_id, principal_id, role) VALUES ($1, $2, $3)`, projectID, p.ID, role)
		return err
	})
	p.Roles[projectID] = role
}

// as runs fn in one transaction under the actor's real scope.
func (e *env) as(actor authn.Principal, fn func(tx pgx.Tx) error) error {
	return store.InScope(e.ctx, e.pool, actor.Scope(), fn)
}

func (e *env) must(actor authn.Principal, fn func(tx pgx.Tx) error) {
	e.t.Helper()
	if err := e.as(actor, fn); err != nil {
		e.t.Fatalf("as %s: %v", actor.Name, err)
	}
}

// task seeds a task that pins the given requirement keys (the task module is another package).
func (e *env) task(projectID, key, state string, pins ...string) string {
	e.t.Helper()
	var id string
	if pins == nil {
		pins = []string{}
	}
	e.must(e.Owner, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO task (project_id, key, title, state, requirement_keys, created_by)
			VALUES ($1, $2, $2, $3, $4, $5) RETURNING id`, projectID, key, state, pins, e.Owner.ID).Scan(&id)
	})
	return id
}

func (e *env) create(actor authn.Principal, in memory.CreateInput) (memory.WriteResult, error) {
	var res memory.WriteResult
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		res, err = memory.CreateKnowledge(e.ctx, tx, actor, in)
		return err
	})
	return res, err
}

func (e *env) mustCreate(actor authn.Principal, in memory.CreateInput) memory.Item {
	e.t.Helper()
	res, err := e.create(actor, in)
	if err != nil {
		e.t.Fatalf("create %s by %s: %v", in.Key, actor.Name, err)
	}
	return res.Item
}

func (e *env) search(actor authn.Principal, q memory.SearchQuery) ([]memory.Hit, error) {
	var hits []memory.Hit
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		hits, err = memory.Search(e.ctx, tx, actor, q)
		return err
	})
	return hits, err
}

// row is the stored state of one knowledge version, read back for assertions.
type row struct {
	Trust, EndedReason, ReplacedBy, NS, ProjectID string
	Version                                       int
	Current                                       bool
}

func (e *env) row(id string) row {
	e.t.Helper()
	var r row
	e.must(e.Owner, func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT trust, ended_reason, coalesce(replaced_by::text,''), coalesce(ns::text,''),
			coalesce(project_id::text,''), version, is_current FROM knowledge WHERE id = $1`, id).
			Scan(&r.Trust, &r.EndedReason, &r.ReplacedBy, &r.NS, &r.ProjectID, &r.Version, &r.Current)
	})
	return r
}

// events returns the journal event types of a project in order, as the owner sees them.
func (e *env) events(projectID string) []string {
	e.t.Helper()
	var types []string
	e.must(e.Owner, func(tx pgx.Tx) error {
		rows, err := tx.Query(e.ctx, `SELECT type FROM journal WHERE project_id IS NOT DISTINCT FROM nullif($1,'')::uuid ORDER BY seq`, projectID)
		if err != nil {
			return err
		}
		types, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	return types
}

func wantKind(t *testing.T, err error, kind kerr.Kind, what string) {
	t.Helper()
	if !kerr.Is(err, kind) {
		t.Fatalf("%s: got %v (kind %d), want kind %d", what, err, kerr.KindOf(err), kind)
	}
}

func cand(title string) memory.Content {
	return memory.Content{Aspect: memory.AspectToBe, Trust: memory.TrustCandidate, Basis: "proposed", Title: title}
}

func auth(title string) memory.Content {
	return memory.Content{Aspect: memory.AspectToBe, Trust: memory.TrustAuthoritative, Title: title}
}

func hitKeys(hits []memory.Hit) []string {
	keys := make([]string, 0, len(hits))
	for _, h := range hits {
		keys = append(keys, h.Item.Key)
	}
	return keys
}
