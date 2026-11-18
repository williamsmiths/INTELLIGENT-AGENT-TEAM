package memory_test

import (
	"encoding/json"
	"net/http"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

// TestSemanticRealSidecar measures the production path (spec S2 part B) on the
// frozen fixture of evals/retrieval with the real local sidecar: package API →
// embed worker → pgvector → SearchWithInfo. It asserts only isolation and that
// the semantic stage helps the paraphrase + cross-language holdout queries;
// the numbers are logged and written to .work/retrieval-production.json.
func TestSemanticRealSidecar(t *testing.T) {
	r := newRealEnv(t)
	e, fx, projects, owner := r.env, r.fx, r.projects, r.owner

	counter := &countingTransport{}
	client := &embed.Client{BaseURL: realSidecar, Model: embed.DefaultModel, Dim: embed.DefaultDim, HTTP: &http.Client{Transport: counter}}
	worker := memory.EnableSemantic(e.pool, client, embed.DefaultMinSim)
	t.Cleanup(func() { memory.UseSemantic(nil) })
	start := time.Now()
	rows := e.drain(worker)
	embedSeconds, embedRequests := time.Since(start).Seconds(), counter.posts.Load()
	if rows != len(owner) || e.drain(worker) != 0 || counter.posts.Load() != embedRequests {
		t.Fatalf("worker embedded %d of %d rows, or embedded again on a second run", rows, len(owner))
	}

	sets := map[string]map[string]*realGroup{"tuning": {}, "holdout": {}}
	junk := map[string]*ratio{"tuning": {}, "holdout": {}}
	status := map[string]int{}
	lat := map[string][]float64{}
	cross := 0
	run := func(mode string, q memory.SearchQuery, member authn.Principal) (hits []memory.Hit, info memory.Info) {
		e.must(member, func(tx pgx.Tx) (err error) {
			at := time.Now()
			hits, info, err = memory.SearchWithInfo(e.ctx, tx, member, q)
			lat[mode] = append(lat[mode], float64(time.Since(at).Microseconds())/1000)
			return err
		})
		return hits, info
	}
	recall := func(hits []memory.Hit, want []string) float64 {
		found := 0
		for _, h := range hits[:min(5, len(hits))] {
			if slices.Contains(want, h.Item.Key) {
				found++
			}
		}
		return float64(found) / float64(len(want))
	}
	const combined = "paraphrase + cross-language"
	for _, q := range fx.Queries {
		pr, set := projects[q.Project], "tuning"
		if q.Holdout {
			set = "holdout"
		}
		query := memory.SearchQuery{ProjectID: pr.id, Q: q.Q, Limit: evalDepth}
		sem, info := run("semantic_first_call", query, pr.member)
		run("semantic_cached_query", query, pr.member)
		status[info.Semantic]++
		memory.UseSemantic(nil)
		lex, _ := run("lexical", query, pr.member)
		memory.EnableSemantic(e.pool, client, embed.DefaultMinSim)
		if len(lex) > len(sem) || !slices.Equal(hitKeys(lex), hitKeys(sem[:len(lex)])) {
			t.Errorf("%s: semantic changed the structured hits: %v vs %v", q.ID, hitKeys(lex), hitKeys(sem))
		}
		for _, h := range sem {
			if from := owner[h.Item.ID]; from != q.Project && from != "global" {
				cross++
				t.Errorf("AC-S2-022: query %s of %s returned %s of %q (stage %s)", q.ID, q.Project, h.Item.Key, from, h.Stage)
			}
		}
		if len(q.Want) == 0 { // negative queries: anything the semantic stage adds is junk
			added := 0.0
			if len(sem) > len(lex) {
				added = 1
			}
			junk[set].add(added)
			continue
		}
		groups := []string{q.Group}
		if q.Group != "vi_diacritics" && q.Group != "vi_no_diacritics" && q.Group != "english" && q.Group != "code" {
			groups = append(groups, combined)
		}
		for _, g := range groups {
			if sets[set][g] == nil {
				sets[set][g] = &realGroup{}
			}
			sets[set][g].Lexical.add(recall(lex, q.Want))
			sets[set][g].Semantic.add(recall(sem, q.Want))
		}
	}

	latency := map[string]map[string]float64{}
	for mode, ms := range lat {
		latency[mode] = pcts(ms)
	}
	report := map[string]any{"generated_at": time.Now().UTC().Format(time.RFC3339), "fixture_sha256": r.sha,
		"model": client.Model, "dim": client.Dim, "min_sim": embed.DefaultMinSim, "limit": evalDepth, "sidecar": realSidecar,
		"method": "memory.SearchWithInfo as a worker of the query's project in a fresh migrated database; " +
			"semantic hits are appended after the structured stages (pgvector, exact or HNSW as the planner chooses); latency is wall time inside an open transaction",
		"embed":              map[string]any{"rows": rows, "requests": embedRequests, "seconds": embedSeconds, "query_requests": counter.posts.Load() - embedRequests},
		"cross_project_hits": cross, "semantic_status": status, "recall_at_5": sets, "negative_queries_with_semantic_hits": junk, "latency_ms": latency}
	out, err := json.MarshalIndent(report, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("../../.work", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("../../.work/retrieval-production.json", append(out, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, set := range []string{"tuning", "holdout"} {
		for g, v := range sets[set] {
			t.Logf("%-7s %-28s recall@5 lexical %4.1f/%-2d  +semantic %4.1f/%-2d", set, g, v.Lexical.Num, v.Lexical.Den, v.Semantic.Num, v.Semantic.Den)
		}
		t.Logf("%-7s negative queries with semantic hits: %.0f/%d", set, junk[set].Num, junk[set].Den)
	}
	t.Logf("embed: %d rows in %d requests, %.1f s; %d query embeddings; status %v; cross-project hits %d; latency ms %v",
		rows, embedRequests, embedSeconds, counter.posts.Load()-embedRequests, status, cross, latency)

	if cross != 0 {
		t.Fatalf("AC-S2-022: %d cross-project hits", cross)
	}
	if g := sets["holdout"][combined]; g == nil || g.Semantic.Value <= g.Lexical.Value {
		t.Fatalf("holdout %s recall@5: semantic %+v is not above lexical %+v", combined, g.Semantic, g.Lexical)
	}
}
