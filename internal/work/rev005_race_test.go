package work_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// REV-005 #1: a waiting reason removed while a lease release is committing.
// The release locks the task, ends the lease and leaves a WAITING_DECISION
// task alone. The removal must decide RUNNING vs RUNNABLE from the lease as it
// is after the release, never from a snapshot taken before it: the invariant
// is "RUNNING ⇒ exactly one ACTIVE lease". A barrier makes the release commit
// only once the removal is blocked on the task row.
func TestREV005_1_WaitingReasonRemovalRacesLeaseRelease(t *testing.T) {
	e := newEnv(t)
	p := e.project("alpha", 1000, 100)
	w := e.agent("w", p.ID, "worker", "HOOK")
	paths := []struct {
		name   string
		wait   func(taskID string) error                 // puts the task in WAITING_DECISION
		remove func(taskID string, pid chan<- int) error // the racing transaction
	}{
		{"T-S2-02 question answered",
			func(id string) error {
				return e.rt(func(tx pgx.Tx) error {
					_, err := work.AddWaitingReason(e.ctx, tx, id, work.ReasonQuestion, "QUESTION_BLOCKING", w.ID, nil)
					return err
				})
			},
			func(id string, pid chan<- int) error {
				return e.rt(func(tx pgx.Tx) error {
					if err := sendPid(tx, e, pid); err != nil {
						return err
					}
					_, _, err := work.RemoveWaitingReason(e.ctx, tx, id, work.ReasonQuestion)
					return err
				})
			}},
		{"T-H-07 budget raised (resumeTask)",
			func(id string) error {
				return e.as(e.owner, func(tx pgx.Tx) error {
					_, err := work.SetTaskBudget(e.ctx, tx, e.owner, id, work.Limits{MaxCost: cost(0)})
					return err
				})
			},
			func(id string, pid chan<- int) error {
				return e.as(e.owner, func(tx pgx.Tx) error {
					if err := sendPid(tx, e, pid); err != nil {
						return err
					}
					_, err := work.SetTaskBudget(e.ctx, tx, e.owner, id, work.Limits{MaxCost: cost(50)})
					return err
				})
			}},
	}
	const rounds = 15
	for pi, path := range paths {
		t.Run(path.name, func(t *testing.T) {
			for r := range rounds {
				task := e.running(p.ID, fmt.Sprintf("RACE-%d-%d", pi, r), w)
				e.must(path.wait(task.ID))
				if st := e.taskState(task.ID); st.State != "WAITING_DECISION" {
					t.Fatalf("round %d: setup state %s", r, st.State)
				}
				e.raceRelease(t, w, task.ID, path.remove)
				st := e.taskState(task.ID)
				if st.State != "RUNNABLE" || len(st.WaitingReasons) != 0 {
					t.Fatalf("round %d: after the race the task is %s %v, want RUNNABLE with no reason", r, st.State, st.WaitingReasons)
				}
				if n := e.brokenRunning(); n != 0 {
					t.Fatalf("round %d: %d RUNNING task(s) without exactly one ACTIVE lease", r, n)
				}
			}
		})
	}
}

// raceRelease runs the holder's release in one transaction and, while it is
// still open, starts remove; it commits only once remove waits on a lock.
func (e *env) raceRelease(t *testing.T, holder authn.Principal, taskID string, remove func(string, chan<- int) error) {
	t.Helper()
	pid := make(chan int, 1)
	done := make(chan error, 1)
	err := e.rt(func(tx pgx.Tx) error {
		if _, err := work.ReleaseLease(e.ctx, tx, holder, taskID); err != nil {
			return err
		}
		go func() { done <- remove(taskID, pid) }()
		var other int
		select {
		case other = <-pid:
		case err := <-done:
			return fmt.Errorf("remover finished before it reported its backend: %v", err)
		case <-time.After(10 * time.Second):
			return fmt.Errorf("remover did not start")
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			var waiting bool
			if err := e.pool.QueryRow(e.ctx, `SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, other).Scan(&waiting); err != nil {
				return err
			}
			if waiting {
				return nil // commit the release while the remover is blocked
			}
			select {
			case err := <-done:
				return fmt.Errorf("remover did not wait for the release: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("remover never blocked on the task")
			}
			time.Sleep(2 * time.Millisecond)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("remover: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("remover did not finish")
	}
}

func sendPid(tx pgx.Tx, e *env, pid chan<- int) error {
	var n int
	if err := tx.QueryRow(e.ctx, `SELECT pg_backend_pid()`).Scan(&n); err != nil {
		return err
	}
	pid <- n
	return nil
}

func (e *env) taskState(id string) work.Task {
	e.t.Helper()
	var task work.Task
	e.must(e.rt(func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT state, waiting_reasons FROM task WHERE id = $1`, id).Scan(&task.State, &task.WaitingReasons)
	}))
	return task
}

// brokenRunning counts RUNNING tasks that do not have exactly one ACTIVE lease.
func (e *env) brokenRunning() int {
	e.t.Helper()
	var n int
	e.must(e.rt(func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*) FROM task t WHERE t.state = 'RUNNING'
			AND (SELECT count(*) FROM lease l WHERE l.task_id = t.id AND l.state = 'ACTIVE') <> 1`).Scan(&n)
	}))
	return n
}

// REV-005 #5: the usage of a consult turn recorded before the turn id is known
// lands on the answerer's own task and may exhaust its TASK budget. Moving it
// to the asker must give the answerer's task its budget state back, and the
// task its RUNNING state.
func TestREV005_5_CollabMoveLiftsTheAnswerersBudgetWait(t *testing.T) {
	e := newEnv(t)
	p := e.project("alpha", 1000, 100)
	asker := e.agent("asker", p.ID, "worker", "HOOK")
	answerer := e.agent("answerer", p.ID, "worker", "HOOK")
	ask := e.running(p.ID, "ASK", asker)
	own := e.running(p.ID, "OWN", answerer)
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SetTaskBudget(e.ctx, tx, e.owner, own.ID, work.Limits{MaxCost: cost(0.05)})
		return err
	}))
	var consult string
	e.must(e.rt(func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `INSERT INTO consult (project_id, task_id, lease_id, asker, answerer, specialty, question, question_hash, expires_at)
			VALUES ($1, $2, (SELECT id FROM lease WHERE task_id = $2 AND state = 'ACTIVE'), $3, $4, 'product', 'q?', 'h', now() + interval '15 minutes')
			RETURNING id::text`, p.ID, ask.ID, asker.ID, answerer.ID).Scan(&consult)
	}))
	e.must(e.rt(func(tx pgx.Tx) error { // the consult turn, before its id is known: on the answerer's own task
		_, err := work.RecordUsage(e.ctx, tx, work.LedgerEntry{Source: "openbot_usage", ExternalID: "u1", OpenBotAgentID: answerer.OpenBotAgentID,
			Usage: work.Usage{Cost: usd(0.08)}, OccurredAt: time.Now(), TurnID: "turn-c"})
		return err
	}))
	if st := e.taskState(own.ID); st.State != "WAITING_DECISION" || !slices.Equal(st.WaitingReasons, []string{"budget"}) {
		t.Fatalf("setup: answerer's task %s %v", st.State, st.WaitingReasons)
	}
	e.must(e.rt(func(tx pgx.Tx) error {
		if _, err := tx.Exec(e.ctx, `UPDATE consult SET turn_id = 'turn-c' WHERE id = $1`, consult); err != nil {
			return err
		}
		n, err := work.AttributeCollab(e.ctx, tx, consult)
		if err == nil && n != 1 {
			err = fmt.Errorf("moved %d rows", n)
		}
		return err
	}))
	var ownBudget, askBudget work.BudgetView
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		if ownBudget, err = work.GetBudget(e.ctx, tx, e.owner, own.ID); err != nil {
			return err
		}
		askBudget, err = work.GetBudget(e.ctx, tx, e.owner, ask.ID)
		return err
	}))
	if ownBudget.Task.State == "EXHAUSTED" || ownBudget.Task.Used.Cost != 0 || askBudget.Task.Used.Cost != usd(0.08) {
		t.Fatalf("budgets after the move: answerer %+v asker %+v", ownBudget.Task, askBudget.Task)
	}
	if st := e.taskState(own.ID); st.State != "RUNNING" || len(st.WaitingReasons) != 0 {
		t.Fatalf("answerer's task after the move: %s %v, want RUNNING with no reason", st.State, st.WaitingReasons)
	}
	if n := e.brokenRunning(); n != 0 {
		t.Fatalf("%d RUNNING task(s) without exactly one ACTIVE lease", n)
	}
}
