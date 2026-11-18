package memory_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

// Lexical baseline for evals/retrieval (spec S2 B.1, AC-S2-024). It loads the
// frozen fixture into a fresh database through the package API and writes what
// memory.Search really returns; the scoring and the semantic stage live in
// evals/retrieval. Runs only when both variables are set:
//
//	KCH_RETRIEVAL_FIXTURE=<abs path>/evals/retrieval/fixture.json
//	KCH_RETRIEVAL_OUT=<abs path>/evals/retrieval/runs/lexical.json

type dumpItem struct {
	Key, Kind, Aspect, Title, Body string
	AppliesWhen                    string `json:"applies_when"`
	Limits                         string
}

type dumpFixture struct {
	Projects map[string]struct{ Items []dumpItem }
	Global   []dumpItem
	Queries  []struct {
		ID, Project, Group, Q string
	}
}

type dumpHit struct {
	Key     string  `json:"key"`
	Project string  `json:"project"` // fixture project name, "global", or "?" for a row outside the fixture
	Stage   string  `json:"stage"`
	Score   float64 `json:"score"`
}

type dumpQuery struct {
	ID        string    `json:"id"`
	Hits      []dumpHit `json:"hits"`
	LatencyMs []float64 `json:"latency_ms"`
}

type dumpReport struct {
	FixtureSHA256    string         `json:"fixture_sha256"`
	GeneratedAt      string         `json:"generated_at"`
	Method           string         `json:"method"`
	Limit            int            `json:"limit"`
	Rounds           int            `json:"rounds"`
	Items            map[string]int `json:"items"`
	CrossProjectHits int            `json:"cross_project_hits"`
	Queries          []dumpQuery    `json:"queries"`
}

func TestRetrievalDump(t *testing.T) {
	fixturePath, outPath := os.Getenv("KCH_RETRIEVAL_FIXTURE"), os.Getenv("KCH_RETRIEVAL_OUT")
	if fixturePath == "" || outPath == "" {
		t.Skip("KCH_RETRIEVAL_FIXTURE and KCH_RETRIEVAL_OUT not set")
	}
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fx dumpFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)

	e := newEnv(t)
	type project struct {
		id     string
		member authn.Principal
	}
	projects := map[string]project{"ielts": {e.A, e.W}, "warehouse": {e.B, e.X}}
	owner := map[string]string{} // knowledge id -> fixture project name or "global"
	rep := dumpReport{FixtureSHA256: hex.EncodeToString(sum[:]), GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Method: "memory.Search as a worker of the query's project, stages key -> fts -> trigram; latency is wall time of Search inside an open transaction",
		Limit:  evalDepth, Rounds: evalRounds, Items: map[string]int{}}

	for _, it := range fx.Global { // an agent proposes, the owner confirms (as seedEval does)
		c := memory.Content{Aspect: it.Aspect, Trust: memory.TrustCandidate, Basis: "inferred", Title: it.Title, Body: it.Body,
			AppliesWhen: it.AppliesWhen, Limits: it.Limits}
		g := e.mustCreate(e.W, memory.CreateInput{Level: memory.LevelGlobal, Key: it.Key, Kind: it.Kind, Content: c})
		if _, err := e.confirm(e.Owner, g.ID, 1); err != nil {
			t.Fatal(err)
		}
		owner[g.ID] = "global"
		rep.Items["global"]++
	}
	for name, p := range fx.Projects {
		pr, ok := projects[name]
		if !ok {
			t.Fatalf("fixture project %q has no test project", name)
		}
		for _, it := range p.Items {
			c := memory.Content{Aspect: it.Aspect, Trust: memory.TrustAuthoritative, Title: it.Title, Body: it.Body}
			owner[e.mustCreate(e.Owner, memory.CreateInput{ProjectID: pr.id, Key: it.Key, Kind: it.Kind, Content: c}).ID] = name
			rep.Items[name]++
		}
	}

	for _, q := range fx.Queries {
		pr := projects[q.Project]
		dq := dumpQuery{ID: q.ID, Hits: []dumpHit{}}
		var hits []memory.Hit
		for range evalRounds {
			e.must(pr.member, func(tx pgx.Tx) (err error) {
				start := time.Now()
				hits, err = memory.Search(e.ctx, tx, pr.member, memory.SearchQuery{ProjectID: pr.id, Q: q.Q, Limit: evalDepth})
				dq.LatencyMs = append(dq.LatencyMs, float64(time.Since(start).Microseconds())/1000)
				return err
			})
		}
		for _, h := range hits {
			from, known := owner[h.Item.ID]
			if !known {
				from = "?"
			}
			if from != q.Project && from != "global" {
				rep.CrossProjectHits++
				t.Errorf("query %s in %s returned %s of %q", q.ID, q.Project, h.Item.Key, from)
			}
			dq.Hits = append(dq.Hits, dumpHit{Key: h.Item.Key, Project: from, Stage: h.Stage, Score: h.Score})
		}
		rep.Queries = append(rep.Queries, dq)
	}

	out, err := json.MarshalIndent(rep, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d queries, items %v, cross-project hits %d, written to %s", len(rep.Queries), rep.Items, rep.CrossProjectHits, outPath)
}
