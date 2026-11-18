package work_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func (e *env) budget(taskID string) work.BudgetView {
	e.t.Helper()
	var v work.BudgetView
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		v, err = work.GetBudget(e.ctx, tx, e.owner, taskID)
		return err
	}))
	return v
}

func (e *env) used(where string, args ...any) int64 {
	return scalar[int64](e, `SELECT used_cost_microusd FROM budget WHERE `+where, args...)
}

// AC-H-030: an OpenBot usage record adds to the budget of the task its agent holds.
func TestAC_H_030_UsageAddsToTaskBudget(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	res, err := e.usage(w, "rec-1", 0.30, time.Now())
	e.must(err)
	if !res.Recorded || res.TaskID != task.ID || res.ProjectID != a.ID || res.TaskBudget != "OK" || res.Exhausted {
		t.Fatalf("usage result = %+v", res)
	}
	b := e.budget(task.ID)
	if b.Task.Used.Cost != usd(0.30) || b.Task.Used.InputTokens != 10 || b.Task.Used.OutputTokens != 5 ||
		b.Task.State != "OK" || b.Effective != "OK" || *b.Task.Limits.MaxCost != usd(1) {
		t.Fatalf("budget = %+v", b)
	}
	got := scalar[string](e, `SELECT provider || '/' || model || '/' || principal_id::text FROM budget_ledger
		WHERE source = 'openbot_usage' AND external_id = 'rec-1' AND task_id = $1`, task.ID)
	if got != "anthropic/claude/"+w.ID {
		t.Fatalf("ledger row = %s", got)
	}
	// The holder reads the budget of its task; another worker does not (PM-H-16).
	e.must(e.as(w, func(tx pgx.Tx) error {
		_, err := work.GetBudget(e.ctx, tx, w, task.ID)
		return err
	}))
	v := e.agent("v", a.ID, "worker", "HOOK")
	wantKind(t, e.as(v, func(tx pgx.Tx) error {
		_, err := work.GetBudget(e.ctx, tx, v, task.ID)
		return err
	}), kerr.NotFound, "worker without the lease reads the budget")
}

// AC-H-031: totals accumulate across agents and providers, and up to PROJECT and SYSTEM.
func TestAC_H_031_AccumulatesAcrossAgents(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "SUPERVISED")
	task := e.running(a.ID, "T-1", w)
	_, err := e.usage(w, "rec-w", 0.30, time.Now())
	e.must(err)
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := work.ReleaseLease(e.ctx, tx, w, task.ID)
		return err
	}))
	_, err = e.assign(e.owner, task.ID, v.ID, "")
	e.must(err)
	_, err = e.usage(v, "rec-v", 0.40, time.Now())
	e.must(err)
	for what, got := range map[string]int64{
		"TASK":    e.used(`level = 'TASK' AND task_id = $1`, task.ID),
		"PROJECT": e.used(`level = 'PROJECT' AND project_id = $1`, a.ID),
		"SYSTEM":  e.used(`level = 'SYSTEM'`),
	} {
		if got != usd(0.70) {
			t.Fatalf("%s used %d, want %d", what, got, usd(0.70))
		}
	}
}

// AC-H-032: a record read twice counts once; usage outside every lease is kept but adds to nothing.
// AC-H-061 (ledger side): usage of an OpenBot agent without a principal is not stored.
func TestAC_H_032_IdempotentAndOutsideLease(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	before := time.Now().Add(-time.Hour)
	task := e.running(a.ID, "T-1", w)
	at := time.Now()
	for i := range 3 {
		res, err := e.usage(w, "rec-1", 0.30, at)
		e.must(err)
		if res.Recorded != (i == 0) || res.Duplicate != (i > 0) {
			t.Fatalf("pass %d: %+v", i, res)
		}
	}
	res, err := e.usage(w, "rec-old", 0.25, before)
	e.must(err)
	if !res.Recorded || res.TaskID != "" || res.ProjectID != "" || res.PrincipalID != w.ID {
		t.Fatalf("usage outside a lease = %+v", res)
	}
	if n := scalar[int](e, `SELECT count(*) FROM budget_ledger WHERE external_id = 'rec-old' AND task_id IS NULL
		AND project_id IS NULL AND cost_microusd = $1`, usd(0.25)); n != 1 {
		t.Fatal("usage outside a lease was not stored unattributed")
	}
	if b := e.budget(task.ID); b.Task.Used.Cost != usd(0.30) || e.used(`level = 'PROJECT' AND project_id = $1`, a.ID) != usd(0.30) ||
		e.used(`level = 'SYSTEM'`) != usd(0.30) {
		t.Fatalf("totals moved: task %+v", b.Task.Used)
	}
	var unknown work.UsageResult
	e.must(e.rt(func(tx pgx.Tx) (err error) {
		unknown, err = work.RecordUsage(e.ctx, tx, work.LedgerEntry{Source: "openbot_usage", ExternalID: "rec-x",
			OpenBotAgentID: "ob-nobody", Usage: work.Usage{Cost: 1}, OccurredAt: at})
		return err
	}))
	if !unknown.UnknownAgent || unknown.Recorded || scalar[int](e, `SELECT count(*) FROM budget_ledger`) != 2 {
		t.Fatalf("unknown agent: %+v", unknown)
	}
}

// AC-H-033: fifty usage records written in parallel add up exactly.
func TestAC_H_033_ConcurrentUsage(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 5)
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	at := time.Now()
	errs := make([]error, 50)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			// Every record is sent twice, as a tailer that restarted would.
			for range 2 {
				if _, err := e.usage(w, fmt.Sprintf("rec-%d", i), 0.01, at); err != nil {
					errs[i] = err
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		e.must(err)
	}
	b := e.budget(task.ID)
	rows := scalar[int](e, `SELECT count(*) FROM budget_ledger WHERE task_id = $1`, task.ID)
	sum := scalar[int64](e, `SELECT sum(cost_microusd)::bigint FROM budget_ledger WHERE task_id = $1`, task.ID)
	if b.Task.Used.Cost != usd(0.50) || rows != 50 || sum != usd(0.50) || b.Task.Used.InputTokens != 500 ||
		e.used(`level = 'PROJECT' AND project_id = $1`, a.ID) != usd(0.50) || e.used(`level = 'SYSTEM'`) != usd(0.50) {
		t.Fatalf("used %d from %d rows summing %d", b.Task.Used.Cost, rows, sum)
	}
}
