package work

import (
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// dims are the five measured dimensions of a budget (§2.4), in column order.
var dims = [5]string{"cost_microusd", "input_tokens", "output_tokens", "tool_calls", "turns"}

const budgetCols = `level, COALESCE(project_id::text,''), COALESCE(task_id::text,''),
	max_cost_microusd, max_input_tokens, max_output_tokens, max_tool_calls, max_turns,
	used_cost_microusd, used_input_tokens, used_output_tokens, used_tool_calls, used_turns, state`

// stateCase is the §2.4 state as SQL: EXHAUSTED at 100% of any limit, WARN at
// 80%, else OK. A NULL limit does not bound its dimension. The state is always
// computed in the same statement that changes the totals or the limits, so it
// can never disagree with them.
func stateCase(used, limit func(i int) string) string {
	var full, warn []string
	for i := range dims {
		u, m := used(i), limit(i)
		full = append(full, fmt.Sprintf("(%s IS NOT NULL AND %s >= %s)", m, u, m))
		warn = append(warn, fmt.Sprintf("(%s IS NOT NULL AND %s * 5 >= %s * 4)", m, u, m))
	}
	return "CASE WHEN " + strings.Join(full, " OR ") + " THEN 'EXHAUSTED' WHEN " +
		strings.Join(warn, " OR ") + " THEN 'WARN' ELSE 'OK' END"
}

// addUsageSQL adds $1..$5 to the totals of the rows matched by where and returns the new state.
func addUsageSQL(where string) string {
	var sets []string
	for i, d := range dims {
		sets = append(sets, fmt.Sprintf("used_%s = used_%s + $%d", d, d, i+1))
	}
	state := stateCase(
		func(i int) string { return fmt.Sprintf("(used_%s + $%d)", dims[i], i+1) },
		func(i int) string { return "max_" + dims[i] })
	return "UPDATE budget SET " + strings.Join(sets, ", ") + ", state = " + state +
		", updated_at = now() WHERE " + where + " RETURNING state"
}

// setLimitsSQL replaces the five limits ($1..$5, NULL = no limit) and recomputes the state at once.
func setLimitsSQL(where string) string {
	var sets []string
	for i, d := range dims {
		sets = append(sets, fmt.Sprintf("max_%s = $%d::bigint", d, i+1))
	}
	state := stateCase(
		func(i int) string { return "used_" + dims[i] },
		func(i int) string { return fmt.Sprintf("$%d::bigint", i+1) })
	return "UPDATE budget SET " + strings.Join(sets, ", ") + ", state = " + state +
		", updated_at = now() WHERE " + where + " RETURNING " + budgetCols
}

var (
	addTaskUsage    = addUsageSQL("level = 'TASK' AND task_id = $6")
	addProjectUsage = addUsageSQL("level = 'PROJECT' AND project_id = $6")
	addSystemUsage  = addUsageSQL("level = 'SYSTEM'")
	setTaskLimits   = setLimitsSQL("level = 'TASK' AND task_id = $6")
	setProjLimits   = setLimitsSQL("level = 'PROJECT' AND project_id = $6")
	setSystemLimits = setLimitsSQL("level = 'SYSTEM'")
)

func (l Limits) args() []any {
	return []any{l.MaxCost, l.MaxInputTokens, l.MaxOutputTokens, l.MaxToolCalls, l.MaxTurns}
}

func (u Usage) args() []any {
	return []any{u.Cost, u.InputTokens, u.OutputTokens, u.ToolCalls, u.Turns}
}

func scanBudget(row pgx.Row) (Budget, error) {
	var b Budget
	err := row.Scan(&b.Level, &b.ProjectID, &b.TaskID,
		&b.Limits.MaxCost, &b.Limits.MaxInputTokens, &b.Limits.MaxOutputTokens, &b.Limits.MaxToolCalls, &b.Limits.MaxTurns,
		&b.Used.Cost, &b.Used.InputTokens, &b.Used.OutputTokens, &b.Used.ToolCalls, &b.Used.Turns, &b.State)
	return b, err
}

// rank orders budget states so the worst one wins.
func rank(state string) int {
	switch state {
	case "EXHAUSTED":
		return 2
	case "WARN":
		return 1
	}
	return 0
}

func worst(states ...string) string {
	out := "OK"
	for _, s := range states {
		if rank(s) > rank(out) {
			out = s
		}
	}
	return out
}
