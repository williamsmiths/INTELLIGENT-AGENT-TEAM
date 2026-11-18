package openbot

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// usagePos is the keyset position in agent_usage_records. recorded_at orders
// the rows as the host wrote them; record_id is unique per agent, so agent_id
// completes the key.
type usagePos struct {
	RecordedAt string `json:"recorded_at"`
	RecordID   string `json:"record_id"`
	AgentID    string `json:"agent_id"`
}

type usageRow struct {
	pos                                     usagePos
	turnID, provider, model, occurredAt, tk string
	costUSD                                 sql.NullFloat64 // NULL: the host did not price this record (limitation L13)
}

// price is USD per million tokens, which is micro-USD per token.
type price struct{ input, cachedRead, cacheWrite, output float64 }

// prices is the fallback table for records the host left without a cost
// (spec S2 L13). A model that is not listed stays unpriced: its tokens count,
// its cost is 0 and USAGE_UNPRICED is journaled once per model.
var prices = []struct {
	model string // matched as a part of the model id
	price
}{
	{"haiku-5-5", price{input: 0.1, cachedRead: 0.01, cacheWrite: 0.2, output: 0.5}},
	{"sonnet-5-5", price{input: 2, cachedRead: 0.1, cacheWrite: 4, output: 10}},
	{"opus-5-5", price{input: 4, cachedRead: 0.2, cacheWrite: 8, output: 20}},
}

// priceOf finds the fallback price of a model id.
func priceOf(model string) (price, bool) {
	for _, p := range prices {
		if strings.Contains(model, p.model) {
			return p.price, true
		}
	}
	return price{}, false
}

// microUSD is the cost of the tokens at a price.
func (p price) microUSD(tk tokens) int64 {
	return int64(math.Round(tk.UncachedInput*p.input + tk.CachedInput*p.cachedRead + tk.CacheCreation*p.cacheWrite + tk.Output*p.output))
}

// tokens is the host's tokens_json. Input tokens = uncached + cached + cache creation.
type tokens struct {
	UncachedInput float64 `json:"uncachedInput"`
	CachedInput   float64 `json:"cachedInput"`
	CacheCreation float64 `json:"cacheCreation"`
	Output        float64 `json:"output"`
}

// entry converts one host row into a ledger entry; ok is false for a row that
// cannot be read. unpriced reports a record without a cost whose model has no
// fallback price.
func (r usageRow) entry() (e work.LedgerEntry, unpriced, ok bool) {
	at, err := time.Parse(time.RFC3339Nano, r.occurredAt)
	var tk tokens
	if err != nil || json.Unmarshal([]byte(r.tk), &tk) != nil {
		return work.LedgerEntry{}, false, false
	}
	in := tk.UncachedInput + tk.CachedInput + tk.CacheCreation
	if tk.UncachedInput < 0 || tk.CachedInput < 0 || tk.CacheCreation < 0 || tk.Output < 0 || r.costUSD.Float64 < 0 || math.IsNaN(r.costUSD.Float64) {
		return work.LedgerEntry{}, false, false
	}
	cost := int64(math.Round(r.costUSD.Float64 * 1e6))
	if !r.costUSD.Valid {
		p, priced := priceOf(r.model)
		cost, unpriced = p.microUSD(tk), !priced
	}
	turn := r.turnID
	if turn == "baseline" { // usage that belongs to no turn
		turn = ""
	}
	return work.LedgerEntry{Source: "openbot_usage", ExternalID: r.pos.AgentID + "/" + r.pos.RecordID,
		OpenBotAgentID: r.pos.AgentID, Provider: r.provider, Model: r.model, OccurredAt: at, TurnID: turn, // spec S2 C.7 COLLAB
		Usage: work.Usage{Cost: cost, InputTokens: int64(in), OutputTokens: int64(tk.Output)},
	}, unpriced, true
}

// unpricedSeen remembers, per tailer, the models already journaled, so the
// journal is searched once per model and not once per record.
var unpricedSeen sync.Map // unpricedKey -> true

type unpricedKey struct {
	tailer *Tailer
	model  string
}

// journalUnpriced records USAGE_UNPRICED once per model: the cost of such
// records is unknown, so budgets under-count until the price table knows it.
func (t *Tailer) journalUnpriced(ctx context.Context, tx pgx.Tx, r usageRow) error {
	model := r.model
	if len(model) > 100 {
		model = model[:100]
	}
	key := unpricedKey{t, model}
	if _, seen := unpricedSeen.Load(key); seen {
		return nil
	}
	var known bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal WHERE type = 'USAGE_UNPRICED' AND payload->>'model' = $1)`,
		model).Scan(&known); err != nil {
		return err
	}
	if !known {
		if _, err := journal.Append(ctx, tx, journal.Event{Type: "USAGE_UNPRICED",
			Payload: map[string]any{"model": model, "provider": r.provider}}); err != nil {
			return err
		}
	}
	unpricedSeen.Store(key, true)
	return nil
}

func (t *Tailer) stepUsage(ctx context.Context, db *sql.DB, batch int, st *Stats) (bool, error) {
	var pos usagePos
	raw, err := t.cursor(ctx, usageCursor)
	if err != nil {
		return false, err
	}
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &pos); err != nil {
			return false, err
		}
	}
	rows, err := db.QueryContext(ctx, `SELECT recorded_at, record_id, agent_id, turn_id, provider, model, occurred_at,
			tokens_json, estimated_cost_usd
		FROM agent_usage_records WHERE (recorded_at, record_id, agent_id) > (?, ?, ?)
		ORDER BY recorded_at, record_id, agent_id LIMIT ?`, pos.RecordedAt, pos.RecordID, pos.AgentID, batch)
	if err != nil {
		return false, err
	}
	var page []usageRow
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.pos.RecordedAt, &r.pos.RecordID, &r.pos.AgentID, &r.turnID, &r.provider, &r.model,
			&r.occurredAt, &r.tk, &r.costUSD); err != nil {
			rows.Close()
			return false, err
		}
		page = append(page, r)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	// One transaction per record: the record and its cursor commit together, and
	// no transaction ever holds the budget rows of two tasks (lock order).
	for _, r := range page {
		var res work.UsageResult
		entry, unpriced, ok := r.entry()
		err := t.runtime(ctx, func(tx pgx.Tx) error {
			if unpriced {
				if err := t.journalUnpriced(ctx, tx, r); err != nil {
					return err
				}
			}
			if ok {
				var err error
				if res, err = work.RecordUsage(ctx, tx, entry); err != nil {
					return err
				}
				if err := recordTurn(ctx, tx, r, entry, res); err != nil {
					return err
				}
			}
			next, err := json.Marshal(r.pos)
			if err != nil {
				return err
			}
			return setCursor(ctx, tx, usageCursor, string(next))
		})
		if err != nil {
			return false, err
		}
		switch {
		case !ok:
			st.Invalid++
		case res.UnknownAgent:
			st.UnknownAgent++
		case res.Duplicate:
			st.Duplicates++
		default:
			st.Usage++
			if res.TaskID == "" {
				st.Unattributed++
			}
		}
	}
	return len(page) == batch, nil
}

// recordTurn counts the turn of a usage record once (ledger source
// openbot_turn, keyed by the turn) and notes which agent, project and task the
// observed turn belongs to. The host writes "baseline" for usage that belongs
// to no turn: its tokens count, a turn does not.
func recordTurn(ctx context.Context, tx pgx.Tx, r usageRow, entry work.LedgerEntry, res work.UsageResult) error {
	if res.UnknownAgent || r.turnID == "" || r.turnID == "baseline" {
		return nil
	}
	entry.Source, entry.ExternalID = "openbot_turn", r.pos.AgentID+"/"+r.turnID
	entry.Usage = work.Usage{Turns: 1}
	if _, err := work.RecordUsage(ctx, tx, entry); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `INSERT INTO agent_turn (turn_id, openbot_agent_id, status, project_id, task_id)
		VALUES ($1, $2, 'running', nullif($3, '')::uuid, nullif($4, '')::uuid)
		ON CONFLICT (turn_id) DO UPDATE SET openbot_agent_id = EXCLUDED.openbot_agent_id,
			project_id = COALESCE(agent_turn.project_id, EXCLUDED.project_id),
			task_id = COALESCE(agent_turn.task_id, EXCLUDED.task_id)`,
		r.turnID, r.pos.AgentID, res.ProjectID, res.TaskID)
	return err
}
