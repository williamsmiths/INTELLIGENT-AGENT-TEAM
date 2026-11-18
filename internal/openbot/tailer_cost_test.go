package openbot

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Spec S2 L13: a usage record the host left without a cost is priced from its
// tokens; a model without a price is journaled once and costs 0.
func TestL13_CostFallbackFromTokens(t *testing.T) {
	tk := tokens{UncachedInput: 1000, CachedInput: 10000, CacheCreation: 500, Output: 200}
	for model, want := range map[string]int64{"claude-haiku-5-5": 400, "claude-sonnet-5-5": 7000, "claude-opus-5-5-20261001": 14000} {
		p, ok := priceOf(model)
		if got := p.microUSD(tk); !ok || got != want {
			t.Errorf("%s: %d micro-USD (priced=%v), want %d", model, got, ok, want)
		}
	}
	for _, model := range []string{"claude-fable-5-2", "gpt-x", ""} {
		if _, ok := priceOf(model); ok {
			t.Errorf("%q has a fallback price", model)
		}
	}

	e := newHarness(t)
	host := newHostDB(t)
	t0 := time.Now()
	const usage = `{"uncachedInput":1000,"cachedInput":10000,"cacheCreation":500,"output":200}`
	add := func(record, model string, cost any) {
		t.Helper()
		host.seq++
		if _, err := host.db.Exec(`INSERT INTO agent_usage_records VALUES (?,?,?,?,?,?,?,?,?,?,?)`, e.agentID, record, "sess-1", "turn-1",
			"claude", model, iso(t0), usage, cost, "api", iso(t0.Add(time.Duration(host.seq)*time.Millisecond))); err != nil {
			t.Fatal(err)
		}
	}
	add("r-haiku", "claude-haiku-5-5", nil)
	add("r-sonnet-priced", "claude-sonnet-5-5", 0.5) // the host's own cost wins over the table
	add("r-fable-1", "claude-fable-5-2", nil)
	add("r-fable-2", "claude-fable-5-2", nil)
	tailer := NewTailer(e.pool, host.path)
	defer tailer.Close()
	if st, err := tailer.Step(e.ctx); err != nil || st.Usage != 4 {
		t.Fatalf("step: %+v %v", st, err)
	}
	cost := func(record string) (v int64) {
		t.Helper()
		e.must(e.rt(func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, `SELECT cost_microusd FROM budget_ledger WHERE source = 'openbot_usage' AND external_id = $1`,
				e.agentID+"/"+record).Scan(&v)
		}))
		return v
	}
	for record, want := range map[string]int64{"r-haiku": 400, "r-sonnet-priced": 500000, "r-fable-1": 0, "r-fable-2": 0} {
		if got := cost(record); got != want {
			t.Errorf("%s: cost %d micro-USD, want %d", record, got, want)
		}
	}
	var events int
	var model string
	e.must(e.rt(func(tx pgx.Tx) error {
		return tx.QueryRow(e.ctx, `SELECT count(*), coalesce(min(payload->>'model'), '') FROM journal WHERE type = 'USAGE_UNPRICED'`).Scan(&events, &model)
	}))
	if events != 1 || model != "claude-fable-5-2" {
		t.Fatalf("USAGE_UNPRICED: %d events for %q, want one for claude-fable-5-2", events, model)
	}
}
