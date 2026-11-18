package hookinstall

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

var at = time.Date(2026, 11, 17, 9, 30, 0, 0, time.UTC)

func appendRaw(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// AC-H-071, client side: one JSON line per fail-open in a local log.
func TestFailOpenLog_AC_H_071(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "kch", "failopen.log")
	if got, err := ReadFailOpenLog(path); err != nil || len(got) != 0 {
		t.Fatalf("missing log: %v, %v", got, err)
	}
	first := FailOpen{At: at, Agent: "agent-w", Event: "pre-tool"}
	second := FailOpen{At: at.Add(time.Second), Agent: "agent-w", Event: "stop"}
	for _, f := range []FailOpen{first, second} {
		if err := AppendFailOpen(path, f); err != nil {
			t.Fatal(err)
		}
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode: %v %v", info, err)
	}
	raw, _ := os.ReadFile(path)
	want := `{"at":"2026-11-17T09:30:00Z","agent":"agent-w","event":"pre-tool"}` + "\n" +
		`{"at":"2026-11-17T09:30:01Z","agent":"agent-w","event":"stop"}` + "\n"
	if string(raw) != want {
		t.Fatalf("log =\n%s", raw)
	}
	appendRaw(t, path, "not json\n{\"at\":\"2026-11-17T09:30:02Z\",\"agent\":\"agent-w\",\"event\":\"rm -rf\"}\n{\"at\":\"2026-11-17T09:30:03Z\",\"agent\":\"agent-w\",\"event\":\"post-tool\"}")
	got, err := ReadFailOpenLog(path)
	if err != nil || !reflect.DeepEqual(got, []FailOpen{first, second}) {
		t.Fatalf("records = %+v, %v (bad lines and an unfinished last line must be skipped)", got, err)
	}
	if err := AppendFailOpen(path, FailOpen{At: at, Agent: "agent-w", Event: "unknown"}); err == nil {
		t.Fatal("an unknown hook event was appended")
	}
}

// AC-H-071, server side: each fail-open line becomes one HOOK_FAILOPEN event, once.
func TestIngestFailOpen_AC_H_071(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "failopen.log")
	ingest := func() (n int) {
		t.Helper()
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) (err error) {
			n, err = IngestFailOpen(ctx, tx, path)
			return err
		}); err != nil {
			t.Fatalf("IngestFailOpen: %v", err)
		}
		return n
	}
	events := func() []map[string]any { return journalPayloads(t, pool, EventFailOpen) }
	cursor := func() (offset int64) {
		t.Helper()
		var value string
		if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT value FROM tail_cursor WHERE name = $1`, "hook_failopen:"+path).Scan(&value)
		}); err != nil {
			t.Fatal(err)
		}
		offset, _ = strconv.ParseInt(value, 10, 64)
		return offset
	}
	size := func() int64 {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}

	if n := ingest(); n != 0 || len(events()) != 0 {
		t.Fatalf("no log yet: ingested %d", n)
	}
	for _, f := range []FailOpen{{At: at, Agent: "agent-w", Event: "pre-tool"}, {At: at.Add(time.Second), Agent: "agent-v", Event: "prompt-submit"}} {
		if err := AppendFailOpen(path, f); err != nil {
			t.Fatal(err)
		}
	}
	if n := ingest(); n != 2 {
		t.Fatalf("ingested %d, want 2", n)
	}
	want := []map[string]any{
		{"agent": "agent-w", "event": "pre-tool", "at": "2026-11-17T09:30:00Z"},
		{"agent": "agent-v", "event": "prompt-submit", "at": "2026-11-17T09:30:01Z"},
	}
	if got := events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}
	if n := ingest(); n != 0 || len(events()) != 2 || cursor() != size() {
		t.Fatalf("second ingest: %d new, %d events, cursor %d of %d", n, len(events()), cursor(), size())
	}

	// A bad line does not block, and a line still being written waits for its newline.
	appendRaw(t, path, "garbage from somebody\n")
	appendRaw(t, path, `{"at":"2026-11-17T09:30:02Z","agent":"agent-w","event":"stop"}`)
	if n := ingest(); n != 0 || len(events()) != 2 {
		t.Fatalf("unfinished line ingested: %d", n)
	}
	appendRaw(t, path, "\n")
	if n := ingest(); n != 1 || len(events()) != 3 || cursor() != size() {
		t.Fatalf("after the newline: %d new, %d events", n, len(events()))
	}
	if n := ingest(); n != 0 || len(events()) != 3 {
		t.Fatalf("line ingested twice: %d", n)
	}

	// A log that was replaced by a shorter one is read from its start.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := AppendFailOpen(path, FailOpen{At: at.Add(time.Minute), Agent: "agent-w", Event: "post-tool"}); err != nil {
		t.Fatal(err)
	}
	if n := ingest(); n != 1 || len(events()) != 4 {
		t.Fatalf("after rotation: %d new, %d events", n, len(events()))
	}

	// The cursor and the system journal are runtime-only: an agent scope cannot ingest.
	if err := AppendFailOpen(path, FailOpen{At: at.Add(2 * time.Minute), Agent: "agent-w", Event: "pre-tool"}); err != nil {
		t.Fatal(err)
	}
	if err := store.InScope(ctx, pool, store.Scope{Role: "agent", Principal: newAgent(t, pool, "w")}, func(tx pgx.Tx) error {
		_, err := IngestFailOpen(ctx, tx, path)
		return err
	}); err == nil {
		t.Fatal("an agent scope ingested the fail-open log")
	}
	if len(events()) != 4 {
		t.Fatalf("%d events after the refused ingest", len(events()))
	}
}
