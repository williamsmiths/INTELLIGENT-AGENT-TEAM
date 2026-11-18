package work_test

import (
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func (e *env) view(taskID string) work.TaskView {
	e.t.Helper()
	var v work.TaskView
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		v, err = work.GetTask(e.ctx, tx, e.owner, taskID)
		return err
	}))
	return v
}

// usage records one OpenBot usage row of the agent, as the tailer does.
func (e *env) usage(agent authn.Principal, id string, usdCost float64, at time.Time) (work.UsageResult, error) {
	var res work.UsageResult
	err := e.rt(func(tx pgx.Tx) (err error) {
		res, err = work.RecordUsage(e.ctx, tx, work.LedgerEntry{Source: "openbot_usage", ExternalID: id,
			OpenBotAgentID: agent.OpenBotAgentID, Provider: "anthropic", Model: "claude",
			Usage: work.Usage{Cost: usd(usdCost), InputTokens: 10, OutputTokens: 5}, OccurredAt: at})
		return err
	})
	return res, err
}

// scalar reads one value with the runtime scope, to assert on rows directly.
func scalar[T any](e *env, sql string, args ...any) T {
	e.t.Helper()
	var v T
	e.must(e.rt(func(tx pgx.Tx) error { return tx.QueryRow(e.ctx, sql, args...).Scan(&v) }))
	return v
}

// events returns the event types of a project journal, read as the owner.
func (e *env) events(projectID string) []string {
	e.t.Helper()
	var out []string
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		evs, err := work.ListEvents(e.ctx, tx, e.owner, projectID, 0, 500)
		for _, ev := range evs {
			out = append(out, ev.Type)
		}
		return err
	}))
	return out
}
