package memory_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

const realSidecar = "http://127.0.0.1:8870"

type realFixture struct {
	Projects map[string]struct{ Items []dumpItem }
	Global   []dumpItem
	Queries  []struct {
		ID, Project, Group, Q string
		Want                  []string
		Holdout               bool
	}
}

type ratio struct {
	Num   float64 `json:"num"`
	Den   int     `json:"den"`
	Value float64 `json:"value"`
}

func (r *ratio) add(v float64) { r.Num += v; r.Den++; r.Value = r.Num / float64(r.Den) }

type realGroup struct {
	Lexical  ratio `json:"lexical_recall_at_5"`
	Semantic ratio `json:"semantic_recall_at_5"`
}

type countingTransport struct{ posts atomic.Int64 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost {
		c.posts.Add(1)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func pcts(ms []float64) map[string]float64 {
	sort.Float64s(ms)
	return map[string]float64{"p50": percentile(ms, 0.50), "p95": percentile(ms, 0.95), "max": percentile(ms, 1)}
}

// realEnv skips unless the local sidecar answers, then loads the frozen fixture
// of evals/retrieval into a fresh database through the package API.
type realEnv struct {
	*env
	fx       realFixture
	sha      string
	projects map[string]realProject
	owner    map[string]string // knowledge id → fixture project or "global"
}

type realProject struct {
	id     string
	member authn.Principal
}

func newRealEnv(t *testing.T) *realEnv {
	t.Helper()
	hc := &http.Client{Timeout: 2 * time.Second}
	resp, err := hc.Get(realSidecar + "/health")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Skipf("local embedding sidecar not reachable at %s", realSidecar)
	}
	resp.Body.Close()
	raw, err := os.ReadFile("../../evals/retrieval/fixture.json")
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	r := &realEnv{sha: hex.EncodeToString(sum[:]), owner: map[string]string{}}
	if err := json.Unmarshal(raw, &r.fx); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t)
	r.env, r.projects = e, map[string]realProject{"ielts": {e.A, e.W}, "warehouse": {e.B, e.X}}
	for _, it := range r.fx.Global {
		g := e.mustCreate(e.W, memory.CreateInput{Level: memory.LevelGlobal, Key: it.Key, Kind: it.Kind, Content: memory.Content{Aspect: it.Aspect,
			Trust: memory.TrustCandidate, Basis: "inferred", Title: it.Title, Body: it.Body, AppliesWhen: it.AppliesWhen, Limits: it.Limits}})
		if _, err := e.confirm(e.Owner, g.ID, 1); err != nil {
			t.Fatal(err)
		}
		r.owner[g.ID] = "global"
	}
	for name, p := range r.fx.Projects {
		for _, it := range p.Items {
			c := memory.Content{Aspect: it.Aspect, Trust: memory.TrustAuthoritative, Title: it.Title, Body: it.Body}
			r.owner[e.mustCreate(e.Owner, memory.CreateInput{ProjectID: r.projects[name].id, Key: it.Key, Kind: it.Kind, Content: c}).ID] = name
		}
	}
	return r
}
