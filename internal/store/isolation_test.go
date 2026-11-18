package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// Isolation is enforced by PostgreSQL itself, not by callers remembering a WHERE clause.
func TestRowLevelSecurityIsolatesProjects(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	owner := store.Scope{Role: "owner"}

	var pa, pb, ownerID, agentA, agentB string
	err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		for _, r := range []struct {
			kind, name, tok string
			out             *string
		}{{"owner", "owner", "t0", &ownerID}, {"agent", "a", "t1", &agentA}, {"agent", "b", "t2", &agentB}} {
			if err := tx.QueryRow(ctx, `INSERT INTO principal (kind,name,token_hash) VALUES ($1,$2,$3) RETURNING id`,
				r.kind, r.name, []byte(r.tok)).Scan(r.out); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	owner.Principal = ownerID
	err = store.InScope(ctx, pool, owner, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO project (slug,name) VALUES ('alpha','Alpha') RETURNING id`).Scan(&pa); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO project (slug,name) VALUES ('beta','Beta') RETURNING id`).Scan(&pb); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO knowledge (level,project_id,key,version,kind,aspect,trust,title,created_by,content_hash) VALUES
			('PROJECT',$1,'REQ-1',1,'requirement','TO_BE','AUTHORITATIVE','alpha secret rule',$3,'h1'),
			('PROJECT',$2,'REQ-1',1,'requirement','TO_BE','AUTHORITATIVE','beta secret rule',$3,'h2')`, pa, pb, ownerID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	inA := store.Scope{Role: "agent", Principal: agentA, Projects: []string{pa}}

	// A query with no project filter at all still returns only project A.
	var titles []string
	err = store.InScope(ctx, pool, inA, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT title FROM knowledge ORDER BY title`)
		if err != nil {
			return err
		}
		titles, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0] != "alpha secret rule" {
		t.Fatalf("agent in A sees %v, want only alpha", titles)
	}

	// Writing into the other project is refused by the database.
	err = store.InScope(ctx, pool, inA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO knowledge (level,project_id,key,version,kind,aspect,trust,basis,title,created_by,content_hash)
			VALUES ('PROJECT',$1,'X',1,'rule','TO_BE','CANDIDATE','inferred','leak',$2,'h3')`, pb, agentA)
		return err
	})
	if err == nil {
		t.Fatal("agent of A inserted into project B")
	}

	// An agent cannot write AUTHORITATIVE knowledge even inside its own project.
	err = store.InScope(ctx, pool, inA, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO knowledge (level,project_id,key,version,kind,aspect,trust,title,created_by,content_hash)
			VALUES ('PROJECT',$1,'Y',1,'rule','TO_BE','AUTHORITATIVE','self-promoted',$2,'h4')`, pa, agentA)
		return err
	})
	if err == nil {
		t.Fatal("agent wrote AUTHORITATIVE knowledge")
	}

	// Nor can it promote an existing candidate by UPDATE, forge evidence, or delete history.
	for name, sql := range map[string]string{
		"promote":  `UPDATE knowledge SET trust='AUTHORITATIVE' WHERE created_by = '` + agentA + `'`,
		"evidence": `INSERT INTO evidence (project_id,task_id,task_revision,candidate_hash,check_id,command,exit_code,result,runner_id,started_at,finished_at) SELECT '` + pa + `', gen_random_uuid(),1,'c','unit','true',0,'PASS','fake',now(),now()`,
		"delete":   `DELETE FROM knowledge`,
		"ledger":   `INSERT INTO budget_ledger (source,external_id,occurred_at) VALUES ('hook_tool','x',now())`,
	} {
		seed := store.InScope(ctx, pool, inA, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO knowledge (level,project_id,key,version,kind,aspect,trust,basis,title,created_by,content_hash)
				VALUES ('PROJECT',$1,$3,1,'rule','TO_BE','CANDIDATE','inferred','mine',$2,$3) ON CONFLICT DO NOTHING`, pa, agentA, "C-"+name)
			return err
		})
		if seed != nil {
			t.Fatalf("seed %s: %v", name, seed)
		}
		var affected int64
		err := store.InScope(ctx, pool, inA, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql)
			affected = tag.RowsAffected()
			return err
		})
		if err == nil && affected > 0 {
			t.Fatalf("%s: agent statement succeeded on %d rows", name, affected)
		}
	}

	// Default deny: a query on the pool that forgets InScope sees nothing (AC-H-087).
	var bare int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM knowledge) + (SELECT count(*) FROM project) + (SELECT count(*) FROM principal)`).Scan(&bare); err != nil || bare != 0 {
		t.Fatalf("unscoped query sees %d rows, err=%v", bare, err)
	}

	// A worker cannot create or change tasks; a lead of A can, but not in B; identity columns are frozen.
	var taskA string
	leadA := store.Scope{Role: "agent", Principal: agentB, Projects: []string{pa}, LeadOf: []string{pa}}
	if err := store.InScope(ctx, pool, leadA, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO task (project_id,key,title,created_by) VALUES ($1,'T-1','t',$2) RETURNING id`, pa, agentB).Scan(&taskA)
	}); err != nil {
		t.Fatalf("lead creates task in own project: %v", err)
	}
	for name, sql := range map[string]string{
		"worker inserts task":     `INSERT INTO task (project_id,key,title,created_by) VALUES ('` + pa + `','T-2','t','` + agentA + `')`,
		"worker updates task":     `UPDATE task SET state='VERIFIED' WHERE id='` + taskA + `'`,
		"worker moves knowledge":  `UPDATE knowledge SET project_id='` + pb + `' WHERE created_by='` + agentA + `'`,
		"worker changes level":    `UPDATE knowledge SET level='GLOBAL' WHERE created_by='` + agentA + `'`,
		"lease on foreign task":   `INSERT INTO lease (project_id,task_id,principal_id,epoch,task_revision,expires_at) VALUES ('` + pb + `','` + taskA + `','` + agentA + `',1,1,now())`,
		"worker inserts lease":    `INSERT INTO lease (project_id,task_id,principal_id,epoch,task_revision,expires_at) VALUES ('` + pa + `','` + taskA + `','` + agentA + `',1,1,now())`,
		"worker registers a repo": `INSERT INTO project_repo (project_id,root) VALUES ('` + pa + `','/x')`,
	} {
		var affected int64
		err := store.InScope(ctx, pool, inA, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, sql)
			affected = tag.RowsAffected()
			return err
		})
		if err == nil && affected > 0 {
			t.Fatalf("%s: succeeded on %d rows", name, affected)
		}
	}
	// Even the runtime cannot attach a lease to a task of another project (composite foreign key).
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lease (project_id,task_id,principal_id,epoch,task_revision,expires_at) VALUES ($1,$2,$3,1,1,now())`, pb, taskA, agentA)
		return err
	}); err == nil {
		t.Fatal("lease attached to a task of another project")
	}

	// Agent B (member of nothing) sees no project rows at all.
	var n int
	err = store.InScope(ctx, pool, store.Scope{Role: "agent", Principal: agentB}, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM knowledge) + (SELECT count(*) FROM project)`).Scan(&n)
	})
	if err != nil || n != 0 {
		t.Fatalf("outsider sees %d rows, err=%v", n, err)
	}
}
