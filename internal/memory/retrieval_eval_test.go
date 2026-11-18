package memory_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

const (
	evalRounds = 5  // each query runs this many times for the latency percentiles
	evalDepth  = 10 // results fetched per query; MRR looks at these, recall at the first 5
)

type evalProject struct {
	id, task string
	member   authn.Principal
	ids      map[string]bool // knowledge ids that may appear in this project's results
}

func seedEval(e *env) map[string]*evalProject {
	e.t.Helper()
	global := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred",
		Title: "Chạy lại test tích hợp sau khi đổi schema", Body: "Sau khi sửa migration, chạy lại toàn bộ test tích hợp trước khi báo xong.",
		AppliesWhen: "task có thay đổi schema", Limits: "không áp dụng cho thay đổi chỉ có giao diện"}
	exp := e.mustCreate(e.W, memory.CreateInput{Level: memory.LevelGlobal, Key: "EXP-1", Kind: "experience", Content: global})
	if _, err := e.confirm(e.Owner, exp.ID, 1); err != nil {
		e.t.Fatal(err)
	}
	projects := map[string]*evalProject{
		"ielts":     {id: e.A, member: e.W, ids: map[string]bool{exp.ID: true}},
		"warehouse": {id: e.B, member: e.X, ids: map[string]bool{exp.ID: true}},
	}
	for name, items := range map[string][]evalItem{"ielts": evalItemsIELTS, "warehouse": evalItemsWarehouse} {
		p := projects[name]
		for _, it := range items {
			c := memory.Content{Aspect: it.Aspect, Trust: memory.TrustAuthoritative, Title: it.Title, Body: it.Body}
			p.ids[e.mustCreate(e.Owner, memory.CreateInput{ProjectID: p.id, Key: it.Key, Kind: it.Kind, Content: c}).ID] = true
		}
		if _, err := e.relate(e.Owner, p.id, "DEC-1", "impacts", "REQ-1"); err != nil {
			e.t.Fatal(err)
		}
		p.task = e.task(p.id, "T-EVAL", "RUNNING", "REQ-1")
	}
	return projects
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[min(len(sorted)-1, int(p*float64(len(sorted))))]
}

// TestRetrievalEvaluation measures the staged retrieval (AC-H-090). It asserts
// isolation and that the report exists; the quality numbers are reported as
// measured, to decide later whether vector search is needed.
func TestRetrievalEvaluation(t *testing.T) {
	e := newEnv(t)
	projects := seedEval(e)
	rep := evalReport{Spec: "AC-H-090", GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ItemsPerProject: map[string]int{"ielts": len(evalItemsIELTS), "warehouse": len(evalItemsWarehouse)},
		Overall:         &evalGroupReport{Group: "overall", FirstRelevantStage: map[string]int{}},
		Method: map[string]string{
			"search":  "memory.Search under the scope of a worker of the queried project, limit 10; stages key -> fts -> trigram",
			"recall":  "share of the labelled keys found in the first 5 results, averaged per group",
			"mrr":     "1/rank of the first labelled key within 10 results, 0 when absent",
			"latency": "wall time of memory.Search inside an open transaction, 5 runs per query",
			"packet":  "memory.AssemblePacket for a task titled with the query that pins REQ-1 (one relation hop: DEC-1 impacts REQ-1); characters of the text",
			"isolation": "every search hit and packet reference must belong to the queried project or be a confirmed GLOBAL item; " +
				"each query also runs as an agent that is a member of both projects",
		}}
	groups := map[string]*evalGroupReport{}
	for _, q := range evalQueries {
		p := projects[q.Project]
		g := groups[q.Group]
		if g == nil {
			g = &evalGroupReport{Group: q.Group, FirstRelevantStage: map[string]int{}}
			groups[q.Group] = g
			rep.Groups = append(rep.Groups, g)
		}
		var hits []memory.Hit
		for _, actor := range []authn.Principal{p.member, e.Z} {
			for range evalRounds {
				e.must(actor, func(tx pgx.Tx) (err error) {
					start := time.Now()
					hits, err = memory.Search(e.ctx, tx, actor, memory.SearchQuery{ProjectID: p.id, Q: q.Q, Limit: evalDepth})
					if actor.ID == p.member.ID {
						g.latencies = append(g.latencies, float64(time.Since(start).Microseconds())/1000)
					}
					return err
				})
				for _, h := range hits {
					if !p.ids[h.Item.ID] {
						rep.CrossProjectHits++
						t.Errorf("query %q in %s as %s returned %s of project %q", q.Q, q.Project, actor.Name, h.Item.Key, h.Item.ProjectID)
					}
				}
			}
		}
		packet := e.packet(p.member, memory.PacketInput{ProjectID: p.id, TaskID: p.task, TaskKey: "T-EVAL", TaskTitle: q.Q,
			Revision: 1, DependencyVersions: map[string]int{"REQ-1": 1}})
		for _, r := range packet.Refs {
			if r.KnowledgeID != "" && !p.ids[r.KnowledgeID] {
				rep.CrossProjectHits++
				t.Errorf("packet for %q in %s references %s outside the project", q.Q, q.Project, r.Key)
			}
		}

		qr := evalQueryReport{Project: q.Project, Group: q.Group, Q: q.Q, Want: q.Want, Stage: "none", Top5: []string{}}
		found := 0
		for rank, h := range hits {
			relevant := h.Item.Level == memory.LevelProject && slices.Contains(q.Want, h.Item.Key)
			if rank < 5 {
				qr.Top5 = append(qr.Top5, h.Item.Key)
				if relevant {
					found++
				}
			}
			if relevant && qr.RR == 0 {
				qr.RR, qr.Stage = 1/float64(rank+1), h.Stage
			}
		}
		qr.RecallAt5 = float64(found) / float64(len(q.Want))
		rep.Queries = append(rep.Queries, qr)
		for _, agg := range []*evalGroupReport{g, rep.Overall} {
			agg.Queries++
			agg.RecallAt5 += qr.RecallAt5
			agg.MRR += qr.RR
			agg.PacketCharsAvg += float64(packet.Chars)
			agg.FirstRelevantStage[qr.Stage]++
		}
	}
	for _, g := range rep.Groups {
		rep.Overall.latencies = append(rep.Overall.latencies, g.latencies...)
	}
	for _, g := range append(slices.Clone(rep.Groups), rep.Overall) {
		n := float64(g.Queries)
		sort.Float64s(g.latencies)
		g.RecallAt5, g.MRR, g.PacketCharsAvg = g.RecallAt5/n, g.MRR/n, g.PacketCharsAvg/n
		g.P50Ms, g.P95Ms = percentile(g.latencies, 0.50), percentile(g.latencies, 0.95)
		t.Logf("%-17s queries=%2d recall@5=%.3f mrr=%.3f p50=%.2fms p95=%.2fms packet_chars=%.0f first_relevant_stage=%v",
			g.Group, g.Queries, g.RecallAt5, g.MRR, g.P50Ms, g.P95Ms, g.PacketCharsAvg, g.FirstRelevantStage)
	}
	for _, q := range rep.Queries {
		if q.RecallAt5 < 1 {
			t.Logf("miss  [%s/%s] %q want=%v top5=%v", q.Project, q.Group, q.Q, q.Want, q.Top5)
		}
	}

	raw, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join("..", "..", ".work", "retrieval-eval.json") // <repo>/.work, ignored by git
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(out, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(rep.Queries) < 30 || len(rep.Groups) != 4 || len(projects) < 2 || rep.CrossProjectHits != 0 {
		t.Fatalf("report incomplete or isolation broken: queries=%d groups=%d cross_project_hits=%d", len(rep.Queries), len(rep.Groups), rep.CrossProjectHits)
	}
	t.Logf("report written to %s", out)
}
