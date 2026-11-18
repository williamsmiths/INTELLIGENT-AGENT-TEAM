package intel

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// UsageSum is an amount on the five measured dimensions of S1 §2.4.
type UsageSum struct {
	Cost         int64 `json:"cost_microusd"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	ToolCalls    int64 `json:"tool_calls"`
	Turns        int64 `json:"turns"`
}

// ModelSum is the usage of one provider and model.
type ModelSum struct {
	Provider string   `json:"provider"`
	Model    string   `json:"model"`
	Usage    UsageSum `json:"usage"`
}

// CostReport is GET /v1/tasks/{id}/cost (C.7).
type CostReport struct {
	TaskID         string              `json:"task_id"`
	Total          UsageSum            `json:"total"`
	ByPhase        map[string]UsageSum `json:"by_phase"` // DISCOVERY, BUILD, REWORK, COLLAB; UNSTAMPED for rows written before part C
	ByModel        []ModelSum          `json:"by_model"`
	Packets        int                 `json:"packets_injected"`
	PacketChars    int64               `json:"packet_chars_injected"`
	Attempts       int                 `json:"attempts"`
	FailedAttempts int                 `json:"failed_attempts"`
	Consults       int                 `json:"consults"`
	RunnerSeconds  float64             `json:"runner_seconds"`
	// The cost of Memory and of Intelligence is not derived here (C.7): it is
	// the difference between the A/B/C configurations on the same task.
	MemoryIntelligence string `json:"memory_intelligence_cost"`
}

// ArmDifference is the value of CostReport.MemoryIntelligence.
const ArmDifference = "measured_by_arm_difference: compare configurations A (OBSERVE), B (CONTEXT), C (FULL) on the same task; not estimated here"

const sums = `COALESCE(sum(cost_microusd),0), COALESCE(sum(input_tokens),0), COALESCE(sum(output_tokens),0),
	COALESCE(sum(tool_calls),0), COALESCE(sum(turns),0)`

// Cost reads the report of one task in the caller's scope.
func Cost(ctx context.Context, tx pgx.Tx, taskID string) (CostReport, error) {
	r := CostReport{TaskID: taskID, ByPhase: map[string]UsageSum{}, ByModel: []ModelSum{}, MemoryIntelligence: ArmDifference}
	rows, err := tx.Query(ctx, `SELECT COALESCE(phase, 'UNSTAMPED'), `+sums+` FROM budget_ledger WHERE task_id = $1 GROUP BY 1 ORDER BY 1`, taskID)
	if err != nil {
		return r, err
	}
	var phase string
	var u UsageSum
	if _, err := pgx.ForEachRow(rows, []any{&phase, &u.Cost, &u.InputTokens, &u.OutputTokens, &u.ToolCalls, &u.Turns}, func() error {
		r.ByPhase[phase] = u
		r.Total = add(r.Total, u)
		return nil
	}); err != nil {
		return r, err
	}
	rows, err = tx.Query(ctx, `SELECT provider, model, `+sums+` FROM budget_ledger
		WHERE task_id = $1 AND (provider <> '' OR model <> '') GROUP BY 1, 2 ORDER BY 1, 2`, taskID)
	if err != nil {
		return r, err
	}
	var m ModelSum
	if _, err := pgx.ForEachRow(rows, []any{&m.Provider, &m.Model, &m.Usage.Cost, &m.Usage.InputTokens, &m.Usage.OutputTokens,
		&m.Usage.ToolCalls, &m.Usage.Turns}, func() error {
		r.ByModel = append(r.ByModel, m)
		return nil
	}); err != nil {
		return r, err
	}
	err = tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM packet_log WHERE task_id = $1), (SELECT COALESCE(sum(chars), 0) FROM packet_log WHERE task_id = $1),
			(SELECT count(*) FROM attempt WHERE task_id = $1), (SELECT count(*) FROM attempt WHERE task_id = $1 AND NOT all_pass),
			(SELECT count(*) FROM consult WHERE task_id = $1),
			(SELECT COALESCE(sum(extract(epoch FROM finished_at - started_at)), 0)::float8 FROM evidence WHERE task_id = $1)`,
		taskID).Scan(&r.Packets, &r.PacketChars, &r.Attempts, &r.FailedAttempts, &r.Consults, &r.RunnerSeconds)
	return r, err
}

func add(a, b UsageSum) UsageSum {
	return UsageSum{a.Cost + b.Cost, a.InputTokens + b.InputTokens, a.OutputTokens + b.OutputTokens, a.ToolCalls + b.ToolCalls, a.Turns + b.Turns}
}

// LogPacket records one injected packet (C.7). Runtime scope.
func LogPacket(ctx context.Context, tx pgx.Tx, projectID, taskID, leaseID, principalID, event, leaseMode, intelMode string, chars int, hash string) error {
	_, err := tx.Exec(ctx, `INSERT INTO packet_log (project_id, task_id, lease_id, principal_id, event, lease_mode, intel_mode, chars, packet_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, projectID, taskID, leaseID, principalID, event, leaseMode, intelMode, chars, hash)
	return err
}
