package hookinstall

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// AC-S2-005: what the hook client reports is judged against the registered
// install; a deviation is journaled once per distinct state.
func TestObserve_AC_S2_005(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	w := newAgent(t, pool, "w")
	workspace := t.TempDir()
	path, sum, err := Install(testSettings(workspace))
	if err != nil {
		t.Fatal(err)
	}
	observe := func(sum string, extras ...string) (ok, registered bool) {
		t.Helper()
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) (err error) {
			ok, registered, err = Observe(ctx, tx, w, sum, extras)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ok, registered
	}
	events := func() []map[string]any { return journalPayloads(t, pool, EventTampered) }

	if ok, registered := observe(sum); ok || registered || len(events()) != 0 {
		t.Fatalf("a principal without an install: ok=%v registered=%v", ok, registered)
	}
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error { return Register(ctx, tx, w, path, sum) }); err != nil {
		t.Fatal(err)
	}
	if ok, registered := observe(sum); !ok || !registered || len(events()) != 0 {
		t.Fatalf("the registered hash: ok=%v registered=%v events=%v", ok, registered, events())
	}
	other := strings.Repeat("ab", 32)
	for range 3 {
		if ok, _ := observe(other); ok {
			t.Fatal("a changed hash was accepted")
		}
	}
	if got := events(); !reflect.DeepEqual(got, []map[string]any{{"principal_id": w, "reason": "changed"}}) {
		t.Fatalf("journal = %v: one event per distinct hash, ids and reason codes only", got)
	}
	// The right hash with an extra config file is still a deviation, and a new one.
	if ok, _ := observe(sum, ExtraMCP, "../../etc/passwd"); ok || len(events()) != 2 || events()[1]["reason"] != "extra_config" {
		t.Fatalf("extra config: events %v", events())
	}
	if ok, _ := observe("not-a-hash\n<script>"); ok || len(events()) != 3 {
		t.Fatalf("a value that is not a hash: events %v", events())
	}
	var seen string
	_ = store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT seen_sha256 FROM hook_install WHERE principal_id = $1`, w).Scan(&seen)
	})
	if seen != HashUnreadable {
		t.Fatalf("a client-chosen string was stored: %q", seen)
	}
	if ok, _ := observe(sum); !ok || len(events()) != 3 {
		t.Fatal("the restored file was not accepted again")
	}

	// The scan sees the same extra files on disk.
	if err := os.WriteFile(filepath.Join(workspace, ".claude", ExtraSettingsLocal), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	var found []Tamper
	scan := func() {
		t.Helper()
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) (err error) {
			found, err = Scan(ctx, tx)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	if scan(); len(found) != 1 || found[0].Reason != "extra_config" || len(events()) != 4 {
		t.Fatalf("scan with settings.local.json: %+v, %d events", found, len(events()))
	}
	// The hook client reporting the same state does not journal it again.
	if ok, _ := observe(sum, ExtraSettingsLocal); ok || len(events()) != 4 {
		t.Fatalf("the same state was journaled twice: %d events", len(events()))
	}
	if scan(); len(found) != 0 || len(events()) != 4 {
		t.Fatalf("second scan: %+v", found)
	}
}

// AC-S2-005: a refused pre-tool of an enrolled workspace is journaled as HOOK_FAILCLOSED.
func TestIngestFailClosed_AC_S2_005(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "failopen.log")
	when := time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)
	for _, f := range []FailOpen{{At: when, Agent: "agent-w", Event: "pre-tool", Closed: true}, {At: when, Agent: "agent-w", Event: "stop"}} {
		if err := AppendFailOpen(path, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		_, err := IngestFailOpen(ctx, tx, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	closed, open := journalPayloads(t, pool, EventFailClosed), journalPayloads(t, pool, EventFailOpen)
	if len(closed) != 1 || closed[0]["event"] != "pre-tool" || len(open) != 1 || open[0]["event"] != "stop" {
		t.Fatalf("closed %v, open %v", closed, open)
	}
}
