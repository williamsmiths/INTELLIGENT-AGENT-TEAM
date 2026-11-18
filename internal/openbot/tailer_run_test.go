package openbot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// AC-H-061: usage of an OpenBot agent without a principal is counted and skipped; the tailer goes on.
func TestAC_H_061_UnknownAgentIsCountedAndSkipped(t *testing.T) {
	e := newHarness(t)
	host := newHostDB(t)
	now := time.Now()
	host.usage("agent-nobody", "rec-1", "turn-x", now, `{"uncachedInput":5,"output":5}`, 1.00)
	host.usage("agent-nobody", "rec-2", "baseline", now, `{"uncachedInput":5,"output":5}`, 1.00)
	host.usage(e.agentID, "rec-1", "turn-1", now, `{"uncachedInput":5,"output":5}`, 0.25) // same record id, another agent
	host.usage(e.agentID, "rec-bad", "turn-1", now, `{"uncachedInput":5,"output":5}`, 0.25)
	if _, err := host.db.Exec(`UPDATE agent_usage_records SET occurred_at = 'yesterday' WHERE record_id = 'rec-bad'`); err != nil {
		t.Fatal(err)
	}
	host.usage(e.agentID, "rec-3", "turn-1", now, `{"uncachedInput":5,"output":5}`, 0.25)

	tail := NewTailer(e.pool, host.path)
	defer tail.Close()
	st, err := tail.Step(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.UnknownAgent != 2 || st.Invalid != 1 || st.Usage != 2 || st.Unattributed != 0 || st.More {
		t.Fatalf("stats = %+v", st)
	}
	got := e.row(`SELECT (SELECT count(*) FROM budget_ledger), (SELECT count(*) FROM budget_ledger WHERE openbot_agent_id = 'agent-nobody'),
		(SELECT used_cost_microusd FROM budget WHERE level = 'TASK' AND task_id = $1),
		(SELECT count(*) FROM agent_turn WHERE turn_id = 'turn-x')`, e.taskID)
	if got != [4]int64{3, 0, 500000, 0} { // two usage rows and one turn of the known agent
		t.Fatalf("ledger rows, unknown rows, task cost, unknown turns = %v", got)
	}
	// The skipped rows are behind the cursor: the next step reads nothing again.
	if st, err = tail.Step(e.ctx); err != nil || st != (Stats{}) {
		t.Fatalf("second step: %+v, %v", st, err)
	}
}

// A host database that does not exist is an error; the tailer never creates it.
func TestTailerNeverCreatesTheHostDatabase(t *testing.T) {
	e := newHarness(t)
	path := filepath.Join(t.TempDir(), "missing", "openbot.db")
	tail := NewTailer(e.pool, path)
	defer tail.Close()
	if _, err := tail.Step(e.ctx); err == nil {
		t.Fatal("step on a missing database succeeded")
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tailer created something: %v", err)
	}
}

// Run drains a backlog, then steps once per tick (no busy loop), keeps going after an error, and stops with ctx.
func TestTailerRunLoop(t *testing.T) {
	e := newHarness(t)
	host := newHostDB(t)
	now := time.Now()
	for i, id := range []string{"r1", "r2", "r3", "r4", "r5"} {
		host.usage(e.agentID, id, "turn-1", now.Add(time.Duration(i)*time.Millisecond), `{"uncachedInput":1,"output":1}`, 0.01)
	}
	tail := NewTailer(e.pool, host.path)
	tail.Batch = 2
	defer tail.Close()
	var mu sync.Mutex
	var steps, recorded int
	drained := make(chan struct{})
	tail.OnStep = func(st Stats, err error) {
		mu.Lock()
		defer mu.Unlock()
		if err != nil {
			t.Errorf("step: %v", err)
		}
		steps++
		if recorded += st.Usage; recorded == 5 && !st.More {
			select {
			case <-drained:
			default:
				close(drained)
			}
		}
	}
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- tail.Run(ctx, 40*time.Millisecond) }()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("backlog was not drained")
	}
	mu.Lock()
	backlogSteps, backlogTime := steps, time.Since(started)
	mu.Unlock()
	if backlogSteps != 3 { // 2 + 2 + 1 rows, without waiting for ticks in between
		t.Fatalf("backlog took %d steps (%v), want 3", backlogSteps, backlogTime)
	}
	time.Sleep(260 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	mu.Lock()
	idle := steps - backlogSteps
	mu.Unlock()
	if idle < 2 || idle > 9 { // about 260 ms / 40 ms ticks; a busy loop would make thousands
		t.Fatalf("%d idle steps in 260 ms at a 40 ms interval", idle)
	}
	if got := e.row(`SELECT count(*), 0, 0, 0 FROM budget_ledger WHERE source = 'openbot_usage'`); got[0] != 5 {
		t.Fatalf("%d usage rows, want 5", got[0])
	}
}

// The live host keeps its database open in WAL mode and writes while the
// tailer reads: rows still in the WAL are seen, the host is never blocked, and
// a step changes neither the database file nor the WAL.
func TestTailerReadsLiveWALDatabase(t *testing.T) {
	e := newHarness(t)
	host := newHostDB(t)
	var mode string
	if err := host.db.QueryRow(`PRAGMA journal_mode = WAL`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal mode %q, %v", mode, err)
	}
	content := func() string {
		t.Helper()
		var out string
		for _, name := range []string{host.path, host.path + "-wal"} {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			out += string(raw)
		}
		return out
	}
	tail := NewTailer(e.pool, host.path)
	defer tail.Close()
	now := time.Now()
	for i, id := range []string{"r1", "r2", "r3"} {
		host.usage(e.agentID, id, "turn-1", now.Add(time.Duration(i)*time.Millisecond), `{"uncachedInput":1,"output":1}`, 0.01)
		host.event("turn.started", "thread-1", `{"detail":{"turnId":"turn-1"}}`, now)
		before := content()
		st, err := tail.Step(e.ctx)
		if err != nil || st.Usage != 1 || st.Turns != 1 {
			t.Fatalf("step %d: %+v, %v", i, st, err)
		}
		if content() != before {
			t.Fatalf("step %d changed the host database or its WAL", i)
		}
	}
	if got := e.row(`SELECT count(*), 0, 0, 0 FROM budget_ledger WHERE source = 'openbot_usage'`); got[0] != 3 {
		t.Fatalf("%d usage rows, want 3", got[0])
	}
}
