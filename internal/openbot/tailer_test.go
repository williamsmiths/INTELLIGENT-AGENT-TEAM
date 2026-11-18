package openbot

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// The fixture has the two host tables with the host's own DDL. The real
// database of the host is never read or copied.
const hostDDL = `
CREATE TABLE orchestration_events (
  sequence INTEGER PRIMARY KEY AUTOINCREMENT, event_id TEXT NOT NULL UNIQUE, command_id TEXT NOT NULL,
  aggregate_type TEXT NOT NULL, aggregate_id TEXT NOT NULL, event_type TEXT NOT NULL, occurred_at TEXT NOT NULL,
  payload_json TEXT NOT NULL CHECK(json_valid(payload_json)));
CREATE TABLE agent_usage_records (
  agent_id TEXT NOT NULL, record_id TEXT NOT NULL, session_id TEXT NOT NULL, turn_id TEXT NOT NULL,
  provider TEXT NOT NULL, model TEXT NOT NULL, occurred_at TEXT NOT NULL,
  tokens_json TEXT NOT NULL CHECK(json_valid(tokens_json)), estimated_cost_usd REAL, rate_basis TEXT,
  recorded_at TEXT NOT NULL, PRIMARY KEY(agent_id, record_id));`

type hostDB struct {
	t    *testing.T
	path string
	db   *sql.DB
	seq  int
}

func newHostDB(t *testing.T) *hostDB {
	path := filepath.Join(t.TempDir(), "openbot.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(hostDDL); err != nil {
		t.Fatal(err)
	}
	return &hostDB{t: t, path: path, db: db}
}

func iso(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }

// usage inserts one usage record as the host writes it; cost < 0 stores NULL.
func (h *hostDB) usage(agent, record, turn string, at time.Time, tokens string, cost float64) {
	h.t.Helper()
	h.seq++
	var c any
	if cost >= 0 {
		c = cost
	}
	if _, err := h.db.Exec(`INSERT INTO agent_usage_records VALUES (?,?,?,?,?,?,?,?,?,?,?)`, agent, record, "sess-1", turn,
		"claude", "claude-opus", iso(at), tokens, c, "api", iso(at.Add(time.Duration(h.seq)*time.Millisecond))); err != nil {
		h.t.Fatal(err)
	}
}

func (h *hostDB) event(eventType, thread, payload string, at time.Time) {
	h.t.Helper()
	h.seq++
	if _, err := h.db.Exec(`INSERT INTO orchestration_events (event_id, command_id, aggregate_type, aggregate_id,
		event_type, occurred_at, payload_json) VALUES (?,?,?,?,?,?,?)`, fmt.Sprintf("ev-%d", h.seq), "cmd", "thread", thread,
		eventType, iso(at), payload); err != nil {
		h.t.Fatal(err)
	}
}

// digest is the content of every file of the host database directory.
func (h *hostDB) digest() string {
	h.t.Helper()
	entries, err := os.ReadDir(filepath.Dir(h.path))
	if err != nil {
		h.t.Fatal(err)
	}
	sum := sha256.New()
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(h.path), e.Name()))
		if err != nil {
			h.t.Fatal(err)
		}
		fmt.Fprintf(sum, "%s:%d:%x;", e.Name(), len(raw), sha256.Sum256(raw))
	}
	return fmt.Sprintf("%x", sum.Sum(nil))
}

// AC-H-060: turns and usage are read by cursor, exactly once, across a stop and a restart; SQLite is never written.
func TestAC_H_060_TailerResumesAndNeverWrites(t *testing.T) {
	e := newHarness(t)
	host := newHostDB(t)
	t0 := time.Now()
	// The host writes the real payload under "detail", next to message content the harness must not take.
	started := func(turn string) string {
		return `{"detail":{"turnId":"` + turn + `"},"changedMessages":[{"text":"SECRET CHAT"}],"activeTurnId":"` + turn + `"}`
	}
	host.event("turn.started", "thread-1", started("turn-1"), t0)
	host.event("conversation.snapshot-updated", "thread-1", `{"detail":{"messageCount":3}}`, t0)
	host.usage(e.agentID, "rec-1", "turn-1", t0, `{"uncachedInput":2,"cachedInput":169479,"cacheCreation":677,"output":8951}`, 0.30)
	host.usage(e.agentID, "rec-2", "turn-1", t0, `{"uncachedInput":10,"cachedInput":0,"cacheCreation":0,"output":5}`, 0.0500004)
	host.usage(e.agentID, "rec-3", "baseline", t0, `{"uncachedInput":1,"cachedInput":1,"cacheCreation":1,"output":1}`, -1)
	host.event("turn.completed", "thread-1", `{"detail":{"turnId":"turn-1","status":"completed"}}`, t0.Add(time.Second))
	host.event("turn.started", "thread-1", started("turn-2"), t0.Add(2*time.Second))
	host.usage(e.agentID, "rec-4", "turn-2", t0.Add(2*time.Second), `{"uncachedInput":100,"output":50}`, 0.10)
	host.event("turn.completed", "thread-1", `{"turnId":"turn-2","status":"failed"}`, t0.Add(3*time.Second))
	host.usage(e.agentID, "rec-0", "turn-0", t0.Add(-time.Hour), `{"uncachedInput":7,"output":7}`, 9.99) // before the lease
	before := host.digest()

	// The first tailer stops after two small steps; a new one resumes from the stored cursors.
	total := Stats{}
	add := func(st Stats, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		total.Usage, total.Duplicates, total.Turns = total.Usage+st.Usage, total.Duplicates+st.Duplicates, total.Turns+st.Turns
		total.Unattributed, total.More = total.Unattributed+st.Unattributed, st.More
	}
	first := NewTailer(e.pool, host.path)
	first.Batch = 1
	for range 2 {
		add(first.Step(e.ctx))
	}
	if total.Usage != 2 || total.Turns != 2 || !total.More {
		t.Fatalf("after two steps of one row: %+v", total)
	}
	if _, err := first.db.Exec(`UPDATE agent_usage_records SET model = 'x'`); err == nil {
		t.Fatal("the tailer's SQLite handle accepted a write")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := NewTailer(e.pool, host.path)
	second.Batch = 2
	defer second.Close()
	for steps := 0; steps == 0 || total.More; steps++ {
		add(second.Step(e.ctx))
	}
	if total.Usage != 5 || total.Turns != 4 || total.Duplicates != 0 || total.Unattributed != 1 {
		t.Fatalf("after the restart: %+v", total)
	}
	check := func(when string) {
		t.Helper()
		got := e.row(`SELECT used_cost_microusd, used_input_tokens, used_output_tokens, used_turns FROM budget
			WHERE level = 'TASK' AND task_id = $1`, e.taskID)
		// rec-1 + rec-2 + rec-3 (no cost) + rec-4; rec-0 is outside the lease.
		want := [4]int64{300000 + 50000 + 100000, 2 + 169479 + 677 + 10 + 3 + 100, 8951 + 5 + 1 + 50, 2}
		if got != want {
			t.Fatalf("%s: task budget %v, want %v", when, got, want)
		}
		if n := e.row(`SELECT (SELECT count(*) FROM budget_ledger WHERE source = 'openbot_usage'),
			(SELECT count(*) FROM budget_ledger WHERE source = 'openbot_turn'),
			(SELECT count(*) FROM budget_ledger WHERE task_id IS NULL), (SELECT count(*) FROM agent_turn)`); n != [4]int64{5, 3, 2, 3} {
			t.Fatalf("%s: usage rows, turn rows, unattributed rows, turns = %v", when, n)
		}
	}
	check("after the restart")
	turns := func(when string) {
		t.Helper()
		for id, status := range map[string]string{"turn-1": "completed", "turn-2": "failed"} {
			var got string
			e.must(e.rt(func(tx pgx.Tx) error {
				return tx.QueryRow(e.ctx, `SELECT status || '|' || openbot_agent_id || '|' || thread_id || '|' ||
					COALESCE(task_id::text, '-') || '|' || (started_at IS NOT NULL)::text || '|' || (completed_at IS NOT NULL)::text
					FROM agent_turn WHERE turn_id = $1`, id).Scan(&got)
			}))
			if want := status + "|" + e.agentID + "|thread-1|" + e.taskID + "|true|true"; got != want {
				t.Fatalf("%s: %s = %s, want %s", when, id, got, want)
			}
		}
	}
	turns("after the restart")
	if n := e.row(`SELECT count(*), 0, 0, 0 FROM journal WHERE payload::text LIKE '%SECRET%'`); n[0] != 0 {
		t.Fatal("chat content reached the harness")
	}

	// Nothing new: a step changes nothing. Cursors lost: everything is read again and counted once.
	add(second.Step(e.ctx))
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := tx.Exec(e.ctx, `UPDATE tail_cursor SET value = CASE name WHEN 'openbot_events' THEN '0' ELSE '{}' END`)
		return err
	}))
	second.Batch = 100
	add(second.Step(e.ctx))
	if total.Usage != 5 || total.Duplicates != 5 || total.Turns != 8 {
		t.Fatalf("after reading everything again: %+v", total)
	}
	check("after reading everything again")
	turns("after reading everything again")
	if after := host.digest(); after != before {
		t.Fatal("the host database directory changed")
	}
}
