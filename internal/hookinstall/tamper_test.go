package hookinstall

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

func newAgent(t *testing.T, pool *pgxpool.Pool, name string) (id string) {
	t.Helper()
	ctx := context.Background()
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO principal (kind,name,token_hash) VALUES ('agent',$1,$2) RETURNING id`, name, []byte(name)).Scan(&id)
	}); err != nil {
		t.Fatalf("seed principal: %v", err)
	}
	return id
}

// journalPayloads returns the payloads of every event of one type, oldest first.
func journalPayloads(t *testing.T, pool *pgxpool.Pool, eventType string) []map[string]any {
	t.Helper()
	ctx := context.Background()
	out := []map[string]any{}
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT payload FROM journal WHERE type = $1 AND project_id IS NULL ORDER BY seq`, eventType)
		if err != nil {
			return err
		}
		raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		out = out[:0]
		for _, raw := range raws {
			var payload map[string]any
			if err := json.Unmarshal(raw, &payload); err != nil {
				return err
			}
			out = append(out, payload)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestScan_AC_H_070(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	w, v := newAgent(t, pool, "w"), newAgent(t, pool, "v")
	paths := map[string]string{}
	install := func(principal string) {
		t.Helper()
		workspace := paths[principal+"/workspace"]
		if workspace == "" {
			workspace = t.TempDir()
			paths[principal+"/workspace"] = workspace
		}
		path, sum, err := Install(testSettings(workspace))
		if err != nil {
			t.Fatal(err)
		}
		paths[principal] = path
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			return Register(ctx, tx, principal, path, sum)
		}); err != nil {
			t.Fatalf("Register: %v", err)
		}
	}
	scan := func() (found []Tamper) {
		t.Helper()
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) (err error) {
			found, err = Scan(ctx, tx)
			return err
		}); err != nil {
			t.Fatalf("Scan: %v", err)
		}
		return found
	}
	flagged := func(principal string) (tampered bool) {
		t.Helper()
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT tampered FROM hook_install WHERE principal_id = $1`, principal).Scan(&tampered)
		}); err != nil {
			t.Fatal(err)
		}
		return tampered
	}
	events := func() []map[string]any { return journalPayloads(t, pool, EventTampered) }
	install(w)
	install(v)

	t.Run("AC-H-070 untouched files are not reported", func(t *testing.T) {
		if found := scan(); len(found) != 0 || len(events()) != 0 || flagged(w) {
			t.Fatalf("found %v, events %v", found, events())
		}
	})
	t.Run("AC-H-070 a changed file is reported once", func(t *testing.T) {
		if err := os.WriteFile(paths[w], []byte(`{"hooks":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		found := scan()
		if len(found) != 1 || found[0].PrincipalID != w || found[0].Reason != "changed" || found[0].SettingsPath != paths[w] || found[0].Seq == 0 {
			t.Fatalf("found = %+v", found)
		}
		want := []map[string]any{{"principal_id": w, "reason": "changed"}}
		if got := events(); !reflect.DeepEqual(got, want) {
			t.Fatalf("journal = %v, want %v (ids and reason codes only)", got, want)
		}
		if !flagged(w) || flagged(v) {
			t.Fatalf("tampered flags: w=%v v=%v", flagged(w), flagged(v))
		}
		if found := scan(); len(found) != 0 || len(events()) != 1 {
			t.Fatalf("the same change was reported again: %v, %d events", found, len(events()))
		}
	})
	t.Run("AC-H-070 a further change is reported again", func(t *testing.T) {
		if err := os.WriteFile(paths[w], []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if found := scan(); len(found) != 1 || found[0].Reason != "changed" {
			t.Fatalf("found = %+v", found)
		}
		if found := scan(); len(found) != 0 || len(events()) != 2 {
			t.Fatalf("found %v, %d events", found, len(events()))
		}
	})
	t.Run("AC-H-070 a deleted file is reported once as missing", func(t *testing.T) {
		if err := os.Remove(paths[v]); err != nil {
			t.Fatal(err)
		}
		found := scan()
		if len(found) != 1 || found[0].PrincipalID != v || found[0].Reason != "missing" {
			t.Fatalf("found = %+v", found)
		}
		all := events()
		if last := all[len(all)-1]; !reflect.DeepEqual(last, map[string]any{"principal_id": v, "reason": "missing"}) {
			t.Fatalf("last event = %v", last)
		}
		if found := scan(); len(found) != 0 || len(events()) != 3 || !flagged(v) {
			t.Fatalf("found %v, %d events, flagged=%v", found, len(events()), flagged(v))
		}
	})
	t.Run("AC-H-070 registering again after a reinstall clears the flag", func(t *testing.T) {
		install(w)
		install(v)
		if flagged(w) || flagged(v) {
			t.Fatal("tampered flag survived Register")
		}
		if found := scan(); len(found) != 0 || len(events()) != 3 {
			t.Fatalf("found %v, %d events", found, len(events()))
		}
	})
	t.Run("AC-H-070 an agent scope cannot register or scan", func(t *testing.T) {
		agent := store.Scope{Role: "agent", Principal: w}
		if err := store.InScope(ctx, pool, agent, func(tx pgx.Tx) error {
			return Register(ctx, tx, w, paths[w], "0000")
		}); err == nil {
			t.Fatal("an agent replaced its own recorded hash")
		}
		if err := os.WriteFile(paths[w], []byte(`{"x":1}`), 0o600); err != nil {
			t.Fatal(err)
		}
		var found []Tamper
		err := store.InScope(ctx, pool, agent, func(tx pgx.Tx) (err error) {
			found, err = Scan(ctx, tx)
			return err
		})
		if len(found) != 0 || len(events()) != 3 {
			t.Fatalf("agent-scoped scan: found %v, err %v, %d events", found, err, len(events()))
		}
		if found := scan(); len(found) != 1 || found[0].PrincipalID != w {
			t.Fatalf("the runtime scan must still see the change: %v", found)
		}
	})
}
