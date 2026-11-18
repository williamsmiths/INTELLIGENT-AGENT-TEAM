package work

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
)

// subUsageSQL takes $1..$5 back from the totals of the rows matched by where
// and recomputes the state from the new totals. It only corrects amounts that
// were charged to the wrong budget (a consult turn, spec S2 C.7), so an
// EXHAUSTED state those amounts caused is lifted with them (REV-005 #5); a
// budget still at or over its limit stays EXHAUSTED.
func subUsageSQL(where string) string {
	var sets []string
	for i, d := range dims {
		sets = append(sets, fmt.Sprintf("used_%s = greatest(used_%s - $%d, 0)", d, d, i+1))
	}
	state := stateCase(
		func(i int) string { return fmt.Sprintf("greatest(used_%s - $%d, 0)", dims[i], i+1) },
		func(i int) string { return "max_" + dims[i] })
	return "UPDATE budget SET " + strings.Join(sets, ", ") + ", state = " + state +
		", updated_at = now() WHERE " + where + " RETURNING state"
}

var (
	subTaskUsage    = subUsageSQL("level = 'TASK' AND task_id = $6")
	subProjectUsage = subUsageSQL("level = 'PROJECT' AND project_id = $6")
)

// AttributeCollab moves to the asker's task the ledger rows of the host turn
// that handled a consult message, once that turn is known (spec S2 C.7,
// REV-004 #10). Usage of that turn recorded before the turn id was resolved
// sits on the answerer's own lease, or on no budget; it is taken back from
// there and added to the asker's TASK and PROJECT, and to SYSTEM only when it
// was on no budget before, so every amount counts exactly once. Rows of other
// turns of the answerer are not touched; a turn shared by two consults goes
// to the oldest. Caller must use store.Runtime scope.
func AttributeCollab(ctx context.Context, tx pgx.Tx, consultID string) (int, error) {
	var project, task, answerer, turn string
	err := tx.QueryRow(ctx, `SELECT project_id::text, task_id::text, answerer::text, turn_id FROM consult c
		WHERE id = $1 AND turn_id <> '' AND NOT EXISTS (SELECT 1 FROM consult o WHERE o.answerer = c.answerer
			AND o.turn_id = c.turn_id AND (o.created_at, o.id) < (c.created_at, c.id))`, consultID).Scan(&project, &task, &answerer, &turn)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	rows, err := tx.Query(ctx, `SELECT id, COALESCE(project_id::text, ''), COALESCE(task_id::text, ''),
			cost_microusd, input_tokens, output_tokens, tool_calls, turns
		FROM budget_ledger WHERE principal_id = $1 AND turn_id = $2 AND phase IS DISTINCT FROM 'COLLAB' ORDER BY id FOR UPDATE`, answerer, turn)
	if err != nil {
		return 0, err
	}
	type row struct {
		id            int64
		project, task string
		u             Usage
	}
	var list []row
	var r row
	if _, err := pgx.ForEachRow(rows, []any{&r.id, &r.project, &r.task, &r.u.Cost, &r.u.InputTokens, &r.u.OutputTokens,
		&r.u.ToolCalls, &r.u.Turns}, func() error {
		list = append(list, r)
		return nil
	}); err != nil {
		return 0, err
	}
	var taskState string
	from := map[string]Task{} // the tasks the amounts were taken from, with their TASK budget state after it
	for _, r := range list {
		var st string
		if r.task != "" {
			if err := tx.QueryRow(ctx, subTaskUsage, append(r.u.args(), r.task)...).Scan(&st); err != nil {
				return 0, err
			}
			from[r.task] = Task{ID: r.task, ProjectID: r.project, State: st}
			if err := tx.QueryRow(ctx, subProjectUsage, append(r.u.args(), r.project)...).Scan(&st); err != nil {
				return 0, err
			}
		}
		if err := tx.QueryRow(ctx, addTaskUsage, append(r.u.args(), task)...).Scan(&taskState); err != nil {
			return 0, err
		}
		if err := tx.QueryRow(ctx, addProjectUsage, append(r.u.args(), project)...).Scan(&st); err != nil {
			return 0, err
		}
		if r.task == "" { // the system total already holds amounts that were on another task
			if err := tx.QueryRow(ctx, addSystemUsage, r.u.args()...).Scan(&st); err != nil {
				return 0, err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE budget_ledger SET project_id = $2, task_id = $3, phase = 'COLLAB' WHERE id = $1`,
			r.id, project, task); err != nil {
			return 0, err
		}
	}
	if len(list) == 0 {
		return 0, nil
	}
	if taskState == "EXHAUSTED" {
		if _, err := exhaustTask(ctx, tx, Task{ID: task, ProjectID: project}, ""); err != nil {
			return 0, err
		}
	}
	// A task the amounts were charged to by mistake may have gone to
	// WAITING_DECISION for its budget (T-H-06); once its budget is no longer
	// EXHAUSTED it continues (T-H-07, REV-005 #5). Tasks in id order.
	ids := slices.Sorted(maps.Keys(from))
	for _, id := range ids {
		if t := from[id]; t.State != "EXHAUSTED" {
			if err := resumeTask(ctx, tx, Task{ID: t.ID, ProjectID: t.ProjectID}, ""); err != nil {
				return 0, err
			}
		}
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: project, TaskID: task, Type: "COLLAB_ATTRIBUTED",
		Payload: map[string]any{"consult_id": consultID, "ledger_rows": len(list)}})
	return len(list), err
}
