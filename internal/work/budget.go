package work

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// SetSystemBudget replaces the SYSTEM limits (PM-H-15, owner only; actor scope).
// The SYSTEM cost limit can never be below the sum of the PROJECT cost limits (AC-H-082).
func SetSystemBudget(ctx context.Context, tx pgx.Tx, actor authn.Principal, l Limits) (Budget, error) {
	if err := requireOwner(actor); err != nil {
		return Budget{}, err
	}
	if l.negative() {
		return Budget{}, kerr.New(kerr.Invalid, "limits must not be negative")
	}
	b, err := scanBudget(tx.QueryRow(ctx, setSystemLimits, l.args()...))
	if err != nil {
		return Budget{}, err
	}
	if err := checkProjectSum(ctx, tx); err != nil {
		return Budget{}, err
	}
	_, err = journal.Append(ctx, tx, journal.Event{Type: "SYSTEM_BUDGET_SET", Actor: actor.ID,
		Payload: map[string]any{"state": b.State}})
	return b, err
}

// checkProjectSum locks the SYSTEM row, then compares it with the sum of the
// PROJECT cost limits in a second statement, so two limit changes that run at
// the same time cannot both pass (§0: sum of PROJECT max_cost <= SYSTEM max_cost).
func checkProjectSum(ctx context.Context, tx pgx.Tx) error {
	var sysMax *int64
	if err := tx.QueryRow(ctx,
		`SELECT max_cost_microusd FROM budget WHERE level = 'SYSTEM' FOR UPDATE`).Scan(&sysMax); err != nil {
		return err
	}
	if sysMax == nil {
		return nil
	}
	var over bool
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(sum(max_cost_microusd), 0) > $1 FROM budget WHERE level = 'PROJECT'`, *sysMax).Scan(&over); err != nil {
		return err
	}
	if over {
		return kerr.New(kerr.Invalid, "sum of PROJECT max_cost exceeds SYSTEM max_cost")
	}
	return nil
}

// SetTaskBudget replaces the TASK limits (PM-H-15, owner only; actor scope) and
// applies the state change at once: below the used amount the budget is
// EXHAUSTED and a RUNNING task waits (T-H-06); above it a waiting task
// continues (T-H-07, AC-H-034, AC-H-036).
func SetTaskBudget(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string, l Limits) (BudgetView, error) {
	if err := requireOwner(actor); err != nil {
		return BudgetView{}, err
	}
	t, err := loadTask(ctx, tx, taskID, false)
	if err != nil {
		return BudgetView{}, err
	}
	if l.negative() {
		return BudgetView{}, kerr.New(kerr.Invalid, "limits must not be negative")
	}
	b, err := scanBudget(tx.QueryRow(ctx, setTaskLimits, append(l.args(), taskID)...))
	if errors.Is(err, pgx.ErrNoRows) {
		return BudgetView{}, notFound("task budget")
	}
	if err != nil {
		return BudgetView{}, err
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID,
		Type: "TASK_BUDGET_SET", Actor: actor.ID, Payload: map[string]any{"state": b.State}}); err != nil {
		return BudgetView{}, err
	}
	if b.State == "EXHAUSTED" {
		_, err = exhaustTask(ctx, tx, t, actor.ID)
	} else {
		err = resumeTask(ctx, tx, t, actor.ID)
	}
	if err != nil {
		return BudgetView{}, err
	}
	return budgetView(ctx, tx, t)
}

// GetBudget returns the TASK limits, totals and state with the PROJECT and
// SYSTEM states (PM-H-16; actor scope).
func GetBudget(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (BudgetView, error) {
	t, _, err := viewTask(ctx, tx, actor, taskID)
	if err != nil {
		return BudgetView{}, err
	}
	return budgetView(ctx, tx, t)
}

// EffectiveBudgetState is the worst state of the TASK, PROJECT and SYSTEM
// budgets of a task: the state that blocks tools (§2.1, AC-H-015). Read only;
// any scope that sees the task.
func EffectiveBudgetState(ctx context.Context, tx pgx.Tx, taskID string) (string, error) {
	t, err := loadTask(ctx, tx, taskID, false)
	if err != nil {
		return "", err
	}
	v, err := budgetView(ctx, tx, t)
	return v.Effective, err
}

func budgetView(ctx context.Context, tx pgx.Tx, t Task) (BudgetView, error) {
	rows, err := tx.Query(ctx, `SELECT `+budgetCols+` FROM budget
		WHERE (level = 'TASK' AND task_id = $1) OR (level = 'PROJECT' AND project_id = $2) OR level = 'SYSTEM'`,
		t.ID, t.ProjectID)
	if err != nil {
		return BudgetView{}, err
	}
	defer rows.Close()
	v := BudgetView{ProjectState: "OK", SystemState: "OK"}
	found := false
	for rows.Next() {
		b, err := scanBudget(rows)
		if err != nil {
			return BudgetView{}, err
		}
		switch b.Level {
		case "TASK":
			v.Task, found = b, true
		case "PROJECT":
			v.ProjectState = b.State
		case "SYSTEM":
			v.SystemState = b.State
		}
	}
	if err := rows.Err(); err != nil {
		return BudgetView{}, err
	}
	if !found {
		return BudgetView{}, notFound("task budget")
	}
	v.Effective = worst(v.Task.State, v.ProjectState, v.SystemState)
	return v, nil
}
