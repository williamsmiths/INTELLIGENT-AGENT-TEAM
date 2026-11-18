package work

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// LedgerEntry is one observed amount of usage: an OpenBot usage record, an
// OpenBot turn, or a tool call seen by a hook (PM-H-18).
type LedgerEntry struct {
	Source         string // openbot_usage, openbot_turn or hook_tool
	ExternalID     string // unique per source; the idempotency key
	PrincipalID    string // set by hook events; when empty the principal is found by OpenBotAgentID
	OpenBotAgentID string
	Provider       string
	Model          string
	Usage          Usage
	OccurredAt     time.Time
	TurnID         string // host turn of the record; "" when unknown (spec S2 C.7 COLLAB)
}

// UsageResult reports what RecordUsage changed.
type UsageResult struct {
	Recorded     bool   // a ledger row was written by this call
	Duplicate    bool   // (source, external_id) was already in the ledger; nothing changed (AC-H-032)
	UnknownAgent bool   // no principal for this OpenBot agent; nothing stored (AC-H-061)
	PrincipalID  string // principal the usage belongs to
	ProjectID    string // empty when the usage is outside every lease
	TaskID       string
	Phase        string // DISCOVERY, BUILD, REWORK or COLLAB; empty when unattributed (spec S2 C.7)
	TaskBudget   string // budget states after the entry; empty when unattributed
	ProjectState string
	SystemState  string
	Exhausted    bool // T-H-06 fired: the task went RUNNING -> WAITING_DECISION
}

// phaseAt is the cost phase lease l was in at time $2 (spec S2 C.7, REV-005
// #10), rebuilt from the journal and the attempts rather than read from the
// lease now, since the tailer reads a record later than it happened:
// DISCOVERY from a grant in DISCOVERY until its understanding was taken;
// then REWORK when the newest attempt of the task by then failed, else BUILD.
// A lease granted before the grant event named its phase starts in its
// current phase.
const phaseAt = `CASE
	WHEN COALESCE((SELECT g.payload->>'intel_phase' FROM journal g WHERE g.task_id = l.task_id AND g.type = 'LEASE_GRANTED'
			AND g.payload->>'lease_id' = l.id::text ORDER BY g.seq LIMIT 1), l.intel_phase) = 'DISCOVERY'
		AND NOT EXISTS (SELECT 1 FROM journal u WHERE u.task_id = l.task_id AND u.type IN ('UNDERSTANDING_ACCEPTED','UNDERSTANDING_FORCED')
			AND u.payload->>'lease_id' = l.id::text AND u.created_at <= $2)
		THEN 'DISCOVERY'
	ELSE COALESCE((SELECT CASE WHEN a.all_pass THEN 'BUILD' ELSE 'REWORK' END FROM attempt a
			WHERE a.task_id = l.task_id AND a.created_at <= $2 ORDER BY a.id DESC LIMIT 1), 'BUILD') END`

// RecordUsage writes one ledger row and adds it to the budgets (§2.4). Caller
// must use store.Runtime scope.
//
// The entry is attributed to the lease its principal held at OccurredAt. With
// a lease, the amounts are added to the TASK, its PROJECT and SYSTEM in this
// transaction, each in one UPDATE that also recomputes the state, so parallel
// entries neither lose nor double an amount (AC-H-033); T-H-06 is applied when
// the TASK budget is EXHAUSTED while the task is RUNNING. Without a lease the
// row is stored with empty project and task and adds to no budget (AC-H-032,
// AC-H-082). A repeated (source, external_id) changes nothing. Usage of the
// host turn that handled a consult message goes to the asker's task with phase
// COLLAB (spec S2 C.7); AttributeCollab moves rows recorded before the turn was known.
func RecordUsage(ctx context.Context, tx pgx.Tx, e LedgerEntry) (UsageResult, error) {
	var res UsageResult
	u := e.Usage
	if e.ExternalID == "" || e.OccurredAt.IsZero() ||
		(e.Source != "openbot_usage" && e.Source != "openbot_turn" && e.Source != "hook_tool") {
		return res, kerr.New(kerr.Invalid, "ledger entry needs a known source, an external id and a time")
	}
	if u.Cost < 0 || u.InputTokens < 0 || u.OutputTokens < 0 || u.ToolCalls < 0 || u.Turns < 0 {
		return res, kerr.New(kerr.Invalid, "usage must not be negative")
	}
	res.PrincipalID = e.PrincipalID
	if res.PrincipalID == "" {
		err := tx.QueryRow(ctx, `SELECT id FROM principal WHERE openbot_agent_id = $1`, e.OpenBotAgentID).Scan(&res.PrincipalID)
		if errors.Is(err, pgx.ErrNoRows) || e.OpenBotAgentID == "" {
			res.UnknownAgent = true
			return res, nil
		}
		if err != nil {
			return res, err
		}
	}
	// The turn that answered a consult belongs to the asker's task, not to the
	// answerer's own lease (spec S2 C.7): counted there once, and only there.
	err := tx.QueryRow(ctx, `SELECT project_id, task_id, 'COLLAB' FROM consult
		WHERE answerer = $1 AND turn_id = $2 AND $2 <> '' ORDER BY created_at LIMIT 1`, res.PrincipalID, e.TurnID).
		Scan(&res.ProjectID, &res.TaskID, &res.Phase)
	if errors.Is(err, pgx.ErrNoRows) {
		// The lease that was active at that time; a replaced lease ends exactly when its successor starts.
		err = tx.QueryRow(ctx, `SELECT project_id, task_id, `+phaseAt+` FROM lease l
			WHERE principal_id = $1 AND granted_at <= $2 AND (ended_at IS NULL OR ended_at > $2)
			ORDER BY granted_at DESC, epoch DESC LIMIT 1`, res.PrincipalID, e.OccurredAt).Scan(&res.ProjectID, &res.TaskID, &res.Phase)
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}
	var id int64
	err = tx.QueryRow(ctx, `INSERT INTO budget_ledger (source, external_id, project_id, task_id, principal_id,
			openbot_agent_id, provider, model, cost_microusd, input_tokens, output_tokens, tool_calls, turns, occurred_at, phase, turn_id)
		VALUES ($1, $2, nullif($3,'')::uuid, nullif($4,'')::uuid, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, nullif($15,''), $16)
		ON CONFLICT (source, external_id) DO NOTHING RETURNING id`,
		e.Source, e.ExternalID, res.ProjectID, res.TaskID, res.PrincipalID, e.OpenBotAgentID, e.Provider, e.Model,
		u.Cost, u.InputTokens, u.OutputTokens, u.ToolCalls, u.Turns, e.OccurredAt, res.Phase, e.TurnID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		res.Duplicate = true
		return res, nil
	}
	if err != nil {
		return res, err
	}
	res.Recorded = true
	if res.TaskID == "" {
		return res, nil
	}
	// Always TASK, then PROJECT, then SYSTEM: one lock order for every writer.
	if err := tx.QueryRow(ctx, addTaskUsage, append(u.args(), res.TaskID)...).Scan(&res.TaskBudget); err != nil {
		return res, err
	}
	if err := tx.QueryRow(ctx, addProjectUsage, append(u.args(), res.ProjectID)...).Scan(&res.ProjectState); err != nil {
		return res, err
	}
	if err := tx.QueryRow(ctx, addSystemUsage, u.args()...).Scan(&res.SystemState); err != nil {
		return res, err
	}
	if res.TaskBudget == "EXHAUSTED" {
		res.Exhausted, err = exhaustTask(ctx, tx, Task{ID: res.TaskID, ProjectID: res.ProjectID}, "")
	}
	return res, err
}
