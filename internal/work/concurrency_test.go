package work_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AC-H-086: two projects, four agents each, all working at once. No row gets
// the wrong project, every project's budget equals its own ledger, SYSTEM is
// the sum of both, and nothing deadlocks.
func TestAC_H_086_TwoProjectsInParallel(t *testing.T) {
	e := newEnv(t)
	type seat struct {
		agent authn.Principal
		task  work.Task
	}
	var seats []seat
	projects := []work.Project{e.project("alpha", 1000, 100), e.project("beta", 1000, 100)}
	for _, p := range projects {
		for i := range 4 {
			agent := e.agent(fmt.Sprintf("%s-%d", p.Slug, i), p.ID, "worker", "HOOK")
			seats = append(seats, seat{agent, e.runnable(p.ID, fmt.Sprintf("T-%d", i))})
		}
	}
	const rounds = 12
	errs := make(chan error, len(seats)*4)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for _, s := range seats {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := e.assign(e.owner, s.task.ID, s.agent.ID, ""); err != nil {
				errs <- fmt.Errorf("assign %s: %w", s.agent.Name, err)
				return
			}
			for r := range rounds {
				// One hook event: read the lease, heartbeat, acknowledge, note a write (runtime + holder scope).
				var held *work.Held
				err := e.rt(func(tx pgx.Tx) (err error) {
					held, err = work.ActiveLease(e.ctx, tx, s.agent.ID)
					return err
				})
				if err == nil && (held == nil || held.Task.ID != s.task.ID || held.Lease.ProjectID != s.task.ProjectID) {
					err = fmt.Errorf("round %d: wrong lease %+v", r, held)
				}
				if err == nil {
					err = e.as(s.agent, func(tx pgx.Tx) error {
						if err := work.TouchLease(e.ctx, tx, held.Lease.ID); err != nil {
							return err
						}
						if _, err := work.AckContext(e.ctx, tx, held.Lease.ID); err != nil {
							return err
						}
						if err := work.MarkWrote(e.ctx, tx, held.Lease.ID); err != nil {
							return err
						}
						_, err := work.GetBudget(e.ctx, tx, s.agent, s.task.ID)
						return err
					})
				}
				if err == nil { // post-tool: one tool call; tailer: one usage record and one turn
					err = e.rt(func(tx pgx.Tx) error {
						for _, entry := range []work.LedgerEntry{
							{Source: "hook_tool", PrincipalID: s.agent.ID, Usage: work.Usage{ToolCalls: 1}},
							{Source: "openbot_usage", OpenBotAgentID: s.agent.OpenBotAgentID,
								Usage: work.Usage{Cost: usd(0.01), InputTokens: 100, OutputTokens: 7}},
							{Source: "openbot_turn", OpenBotAgentID: s.agent.OpenBotAgentID, Usage: work.Usage{Turns: 1}},
						} {
							entry.ExternalID = fmt.Sprintf("%s/%d", s.agent.ID, r)
							entry.OccurredAt = time.Now()
							if _, err := work.RecordUsage(e.ctx, tx, entry); err != nil {
								return err
							}
						}
						return nil
					})
				}
				if err == nil && r == rounds/2 { // the owner steers this task in the middle of the run
					err = e.as(e.owner, func(tx pgx.Tx) error {
						_, err := work.Feedback(e.ctx, tx, e.owner, s.task.ID, "feedback for "+s.agent.Name)
						return err
					})
				}
				if err != nil {
					errs <- fmt.Errorf("%s round %d: %w", s.agent.Name, r, err)
					return
				}
			}
			if err := e.rt(func(tx pgx.Tx) error {
				_, err := work.ReleaseLease(e.ctx, tx, s.agent, s.task.ID)
				return err
			}); err != nil {
				errs <- fmt.Errorf("release %s: %w", s.agent.Name, err)
			}
		}()
	}
	close(start)
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(90 * time.Second):
		t.Fatal("agents did not finish: deadlock")
	}
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if t.Failed() {
		t.FailNow()
	}

	// No row carries a project other than the one of its task.
	wrong := scalar[int](e, `SELECT
		(SELECT count(*) FROM lease l JOIN task t ON t.id = l.task_id WHERE l.project_id <> t.project_id)
		+ (SELECT count(*) FROM budget_ledger g JOIN task t ON t.id = g.task_id WHERE g.project_id <> t.project_id)
		+ (SELECT count(*) FROM journal j JOIN task t ON t.id = j.task_id WHERE j.project_id <> t.project_id)
		+ (SELECT count(*) FROM journal j JOIN source s ON s.id = (j.payload->>'source_id')::uuid
			WHERE j.type = 'TASK_FEEDBACK' AND s.project_id <> j.project_id)
		+ (SELECT count(*) FROM budget b JOIN task t ON t.id = b.task_id WHERE b.project_id <> t.project_id)
		+ (SELECT count(*) FROM budget_ledger WHERE task_id IS NULL)`)
	if wrong != 0 {
		t.Fatalf("%d rows with a wrong or missing project", wrong)
	}
	// Every budget equals its own ledger, on every dimension.
	const sums = `SELECT COALESCE(sum(cost_microusd), 0)::bigint, COALESCE(sum(input_tokens), 0)::bigint,
		COALESCE(sum(tool_calls), 0)::bigint, COALESCE(sum(turns), 0)::bigint FROM budget_ledger WHERE `
	const used = `SELECT used_cost_microusd, used_input_tokens, used_tool_calls, used_turns FROM budget WHERE `
	read := func(sql string, args ...any) (out [4]int64) {
		e.must(e.rt(func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, sql, args...).Scan(&out[0], &out[1], &out[2], &out[3])
		}))
		return out
	}
	perProject := [4]int64{4 * rounds * usd(0.01), 4 * rounds * 100, 4 * rounds, 4 * rounds}
	var system [4]int64
	for _, p := range projects {
		got, ledger := read(used+`level = 'PROJECT' AND project_id = $1`, p.ID), read(sums+`project_id = $1`, p.ID)
		if got != ledger || got != perProject {
			t.Fatalf("project %s: budget %v, ledger %v, want %v", p.Slug, got, ledger, perProject)
		}
		for i := range system {
			system[i] += got[i]
		}
	}
	for _, s := range seats {
		got, ledger := read(used+`level = 'TASK' AND task_id = $1`, s.task.ID), read(sums+`task_id = $1`, s.task.ID)
		if got != ledger || got[2] != rounds {
			t.Fatalf("task of %s: budget %v, ledger %v", s.agent.Name, got, ledger)
		}
		if v := e.view(s.task.ID); v.Task.State != "RUNNABLE" || v.Task.Revision != 2 || v.Lease != nil {
			t.Fatalf("task of %s ended as %+v", s.agent.Name, v.Task)
		}
	}
	if got := read(used + `level = 'SYSTEM'`); got != system {
		t.Fatalf("SYSTEM budget %v, want the sum of the projects %v", got, system)
	}
}
