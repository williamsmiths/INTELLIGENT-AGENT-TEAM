package verify

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

func TestEvidence(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	projectA, taskA, projectB, taskB, agent := seed(t, pool)
	repo, base := newRepo(t)
	write(t, repo, "src/a.go", "package a // the work\n")

	req := request(t, repo, base, Check{ID: "unit", Command: "echo ok"})
	req.ProjectID, req.TaskID = projectA, taskA
	res := run(t, req)
	if !res.AllPass {
		t.Fatalf("run: %+v", res)
	}
	member := store.Scope{Role: "agent", Principal: agent, Projects: []string{projectA}}
	count := func() (n int) {
		t.Helper()
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM evidence WHERE task_id = $1`, taskA).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	record := func(sc store.Scope, req Request, res Result) error {
		return store.InScope(ctx, pool, sc, func(tx pgx.Tx) error { return Record(ctx, tx, req, res) })
	}
	hasPass := func(sc store.Scope, task string, revision int, candidate string, ids ...string) (ok bool) {
		t.Helper()
		if err := store.InScope(ctx, pool, sc, func(tx pgx.Tx) (err error) {
			ok, err = HasPassFor(ctx, tx, task, revision, candidate, ids)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	latest := func(sc store.Scope, task string) (rows []Evidence) {
		t.Helper()
		if err := store.InScope(ctx, pool, sc, func(tx pgx.Tx) (err error) {
			rows, err = Latest(ctx, tx, task)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}

	t.Run("AC-H-042 evidence under an agent scope is refused by the database", func(t *testing.T) {
		if err := record(member, req, res); err == nil {
			t.Fatal("an agent scope inserted evidence")
		}
		if err := record(store.Scope{Role: "owner"}, req, res); err == nil {
			t.Fatal("an owner scope inserted evidence")
		}
		if n := count(); n != 0 {
			t.Fatalf("%d evidence rows exist", n)
		}
	})
	t.Run("AC-H-042 a result that Run did not produce is refused", func(t *testing.T) {
		zero := 0
		claimed := Result{Candidate: res.Candidate, AllPass: true, Items: []Item{{ID: newID(), CheckID: "unit", Command: "true", Result: ResultPass, ExitCode: &zero}}}
		if err := record(store.Runtime, req, claimed); !errors.Is(err, ErrForeignResult) {
			t.Fatalf("hand-built result: %v", err)
		}
		other := req
		other.ProjectID, other.TaskID = projectB, taskB
		if err := record(store.Runtime, other, res); !errors.Is(err, ErrForeignResult) {
			t.Fatalf("result recorded for another task: %v", err)
		}
		forged := res
		forged.Items = []Item{{ID: newID(), CheckID: "unit", Command: "true", Result: ResultPass}}
		if err := record(store.Runtime, req, forged); err == nil {
			t.Fatal("Record accepted PASS without exit code 0")
		}
		if n := count(); n != 0 {
			t.Fatalf("%d evidence rows exist", n)
		}
	})
	t.Run("AC-H-040 the runtime records and members read the evidence", func(t *testing.T) {
		if err := record(store.Runtime, req, res); err != nil {
			t.Fatalf("Record: %v", err)
		}
		rows := latest(member, taskA)
		if len(rows) != 2 || rows[0].CheckID != ScopeCheckID || rows[1].CheckID != "unit" {
			t.Fatalf("rows = %+v", rows)
		}
		for i, row := range rows {
			want := item(t, res, row.CheckID)
			if row.Result != ResultPass || row.ExitCode == nil || *row.ExitCode != 0 || row.Candidate != res.Candidate ||
				row.LogSHA256 != want.LogSHA256 || row.LogPath != want.LogPath || row.ID != want.ID ||
				row.Revision != 1 || row.RunnerID != "test-runner" || row.ProjectID != projectA || row.TaskID != taskA {
				t.Fatalf("row %d = %+v", i, row)
			}
		}
		if rows[0].Command != "scope: diff since "+base || rows[0].Detail["base_commit"] != base {
			t.Fatalf("scope row = %+v", rows[0])
		}
		if !hasPass(member, taskA, 1, res.Candidate, "unit") {
			t.Fatal("HasPassFor is false for the recorded run")
		}
		for name, ok := range map[string]bool{
			"other candidate":   hasPass(member, taskA, 1, "0000000", "unit"),
			"other revision":    hasPass(member, taskA, 2, res.Candidate, "unit"),
			"check never run":   hasPass(member, taskA, 1, res.Candidate, "unit", "lint"),
			"other task":        hasPass(store.Runtime, taskB, 1, res.Candidate, "unit"),
			"empty candidate":   hasPass(member, taskA, 1, "", "unit"),
			"outsider's scope":  hasPass(store.Scope{Role: "agent", Principal: agent, Projects: []string{projectB}}, taskA, 1, res.Candidate, "unit"),
			"scope alone asked": !hasPass(member, taskA, 1, res.Candidate),
		} {
			if ok {
				t.Errorf("HasPassFor wrong for: %s", name)
			}
		}
		if rows := latest(store.Scope{Role: "agent", Principal: agent, Projects: []string{projectB}}, taskA); len(rows) != 0 {
			t.Fatalf("an agent of another project reads %d evidence rows", len(rows))
		}
	})
	t.Run("AC-H-045 recording the same result again adds nothing", func(t *testing.T) {
		if err := record(store.Runtime, req, res); err != nil {
			t.Fatalf("second Record: %v", err)
		}
		if n := count(); n != 2 {
			t.Fatalf("%d evidence rows, want 2", n)
		}
	})
	t.Run("AC-H-041 a newer FAIL on the same candidate overrides the PASS", func(t *testing.T) {
		failing := req
		failing.Checks = []Check{{ID: "unit", Command: "exit 3"}}
		again := run(t, failing)
		if again.Candidate != res.Candidate {
			t.Fatal("candidate moved between runs")
		}
		if err := record(store.Runtime, failing, again); err != nil {
			t.Fatal(err)
		}
		if hasPass(member, taskA, 1, res.Candidate, "unit") {
			t.Fatal("HasPassFor still true after a newer FAIL")
		}
		rows := latest(member, taskA)
		if len(rows) != 2 || rows[1].Result != ResultFail || rows[1].ExitCode == nil || *rows[1].ExitCode != 3 {
			t.Fatalf("latest = %+v", rows)
		}
		if n := count(); n != 4 {
			t.Fatalf("%d evidence rows, want 4 (evidence is append-only)", n)
		}
	})
	t.Run("AC-H-040 PASS without a real exit code 0 never counts", func(t *testing.T) {
		// Rows written around Record (only possible under the runtime scope) still cannot satisfy T-H-03.
		err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO evidence (project_id,task_id,task_revision,candidate_hash,check_id,command,result,runner_id,started_at,finished_at)
				SELECT $1,$2,1,'c0ffee1',c,'true','PASS','r',now(),now() FROM unnest(ARRAY['unit','scope']) c`, projectB, taskB)
			return err
		})
		if err != nil {
			t.Logf("the database refused PASS with a null exit code: %v", err)
			return
		}
		if hasPass(store.Runtime, taskB, 1, "c0ffee1", "unit") {
			t.Fatal("PASS rows with a null exit code satisfied HasPassFor")
		}
	})
}
