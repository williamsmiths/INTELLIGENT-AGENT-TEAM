package work_test

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func (e *env) effective(scopeOf func(func(tx pgx.Tx) error) error, taskID string) string {
	e.t.Helper()
	var state string
	e.must(scopeOf(func(tx pgx.Tx) (err error) {
		state, err = work.EffectiveBudgetState(e.ctx, tx, taskID)
		return err
	}))
	return state
}

// AC-H-015: reaching the limit exhausts the budget and parks the task (T-H-06);
// the effective state is what the pre-tool hook denies on.
func TestAC_H_015_ExhaustedBudgetParksTask(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	asHolder := func(fn func(tx pgx.Tx) error) error { return e.as(w, fn) }

	if res, err := e.usage(w, "rec-1", 0.40, time.Now()); err != nil || res.Exhausted || res.TaskBudget != "OK" {
		t.Fatalf("first usage: %+v, %v", res, err)
	}
	res, err := e.usage(w, "rec-2", 0.60, time.Now()) // exactly at the limit: >= 100%
	e.must(err)
	if !res.Exhausted || res.TaskBudget != "EXHAUSTED" || res.ProjectState != "OK" {
		t.Fatalf("usage at the limit: %+v", res)
	}
	v := e.view(task.ID)
	if v.Task.State != "WAITING_DECISION" || v.Lease == nil || v.Budget.Task.State != "EXHAUSTED" {
		t.Fatalf("task %s, lease %v, budget %s", v.Task.State, v.Lease, v.Budget.Task.State)
	}
	if got := e.effective(asHolder, task.ID); got != "EXHAUSTED" {
		t.Fatalf("effective state read by the holder = %s", got)
	}
	payload := scalar[string](e, `SELECT payload::text FROM journal WHERE type = 'BUDGET_EXHAUSTED' AND task_id = $1`, task.ID)
	if payload == "" || scalar[int](e, `SELECT count(*) FROM journal WHERE type = 'BUDGET_EXHAUSTED'`) != 1 {
		t.Fatalf("BUDGET_EXHAUSTED journal: %s", payload)
	}
	// More usage after exhaustion still counts (limitation L7) but emits no second event.
	_, err = e.usage(w, "rec-3", 0.10, time.Now())
	e.must(err)
	if n := scalar[int](e, `SELECT count(*) FROM journal WHERE type = 'BUDGET_EXHAUSTED'`); n != 1 {
		t.Fatalf("%d BUDGET_EXHAUSTED events, want 1", n)
	}

	// PROJECT or SYSTEM exhaustion blocks tools too but leaves the task state alone.
	other := e.project("beta", 0.50, 5)
	x := e.agent("x", other.ID, "worker", "HOOK")
	free := e.running(other.ID, "T-1", x)
	res, err = e.usage(x, "rec-x", 0.50, time.Now())
	e.must(err)
	if res.TaskBudget != "OK" || res.ProjectState != "EXHAUSTED" || res.Exhausted {
		t.Fatalf("project exhaustion: %+v", res)
	}
	if st := e.view(free.ID).Task.State; st != "RUNNING" {
		t.Fatalf("task became %s on PROJECT exhaustion", st)
	}
	if got := e.effective(e.rt, free.ID); got != "EXHAUSTED" {
		t.Fatalf("effective state = %s, want EXHAUSTED", got)
	}
}

// AC-H-035: a SUPERVISED holder is only observed: the task waits and the event says so.
func TestAC_H_035_SupervisedIsObservedOnly(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	v := e.agent("v", a.ID, "worker", "SUPERVISED")
	task := e.running(a.ID, "T-1", v)
	res, err := e.usage(v, "rec-1", 1.50, time.Now())
	e.must(err)
	if !res.Exhausted || e.view(task.ID).Task.State != "WAITING_DECISION" {
		t.Fatalf("usage: %+v", res)
	}
	got := scalar[string](e, `SELECT payload->>'enforcement' FROM journal WHERE type = 'BUDGET_EXHAUSTED' AND task_id = $1`, task.ID)
	if got != "supervised" {
		t.Fatalf("enforcement in BUDGET_EXHAUSTED = %q, want supervised", got)
	}
	if n := scalar[int](e, `SELECT count(*) FROM journal WHERE type LIKE '%DENIED%' OR type LIKE '%BLOCKED%'`); n != 0 {
		t.Fatal("harness claims it blocked a supervised agent")
	}
}

// AC-H-036: WARN from 80% of any limit; lowering a limit below the used amount exhausts at once.
func TestAC_H_036_WarnAndLowering(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	_, err := e.usage(w, "rec-1", 0.79, time.Now())
	e.must(err)
	if st := e.budget(task.ID).Task.State; st != "OK" {
		t.Fatalf("79%% is %s, want OK", st)
	}
	_, err = e.usage(w, "rec-2", 0.01, time.Now())
	e.must(err)
	if b := e.budget(task.ID); b.Task.State != "WARN" || b.Effective != "WARN" || e.view(task.ID).Task.State != "RUNNING" {
		t.Fatalf("80%% is %s / %s", b.Task.State, b.Effective)
	}
	// Any dimension counts: 4 of 5 tool calls is WARN, a hook tool call is the fifth.
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetTaskBudget(e.ctx, tx, e.owner, task.ID, work.Limits{MaxCost: cost(100), MaxToolCalls: ptr(int64(5))})
		return err
	}))
	tool := func(id string, n int64) work.UsageResult {
		var res work.UsageResult
		e.must(e.rt(func(tx pgx.Tx) (err error) {
			res, err = work.RecordUsage(e.ctx, tx, work.LedgerEntry{Source: "hook_tool", ExternalID: id,
				PrincipalID: w.ID, Usage: work.Usage{ToolCalls: n}, OccurredAt: time.Now()})
			return err
		}))
		return res
	}
	if res := tool("toolu_1", 3); res.TaskBudget != "OK" {
		t.Fatalf("3 of 5 tool calls: %+v", res)
	}
	if res := tool("toolu_2", 1); res.TaskBudget != "WARN" {
		t.Fatalf("4 of 5 tool calls: %+v", res)
	}
	if res := tool("toolu_2", 1); !res.Duplicate { // AC-H-026: the same tool_use_id counts once
		t.Fatalf("repeated tool call: %+v", res)
	}
	if got := e.budget(task.ID).Task.Used.ToolCalls; got != 4 {
		t.Fatalf("tool calls = %d, want 4", got)
	}
	// The owner lowers the cost limit below what is used: EXHAUSTED at once, and the task waits.
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		b, err := work.SetTaskBudget(e.ctx, tx, e.owner, task.ID, work.Limits{MaxCost: cost(0.50)})
		if err == nil && (b.Task.State != "EXHAUSTED" || b.Task.Limits.MaxToolCalls != nil) {
			t.Fatalf("after lowering: %+v", b.Task)
		}
		return err
	}))
	if st := e.view(task.ID).Task.State; st != "WAITING_DECISION" {
		t.Fatalf("task is %s after lowering, want WAITING_DECISION", st)
	}
	e.must(e.as(e.owner, func(tx pgx.Tx) error { // the same holds for the PROJECT limit
		p, err := work.SetProjectLimits(e.ctx, tx, e.owner, a.ID, work.ProjectLimits{Budget: work.Limits{MaxCost: cost(0.10)}})
		if err == nil && p.Budget.State != "EXHAUSTED" {
			t.Fatalf("project budget after lowering: %+v", p.Budget)
		}
		return err
	}))
}
