package memory_test

import (
	"context"
	"errors"
	"math"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/embed/embedtest"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func TestSemanticSidecarDown(t *testing.T) {
	e := newEnv(t)
	closed := httptest.NewServer(nil)
	closed.Close()
	down := &embed.Client{BaseURL: closed.URL, Model: embed.DefaultModel, Dim: embed.DefaultDim}
	w := memory.EnableSemantic(e.pool, down, embed.DefaultMinSim)
	t.Cleanup(func() { memory.UseSemantic(nil) })

	t.Run("AC-S2-023 writes succeed, search answers with the structured stages", func(t *testing.T) {
		req := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: auth("Xóa nhóm câu hỏi phải xác nhận")})
		if n, err := w.Step(e.ctx); n != 0 || !errors.Is(err, embed.ErrUnavailable) {
			t.Fatalf("worker with the sidecar down: %d rows, %v", n, err)
		}
		start := time.Now()
		hits, info, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "xoa nhom cau hoi"})
		if err != nil || len(hits) != 1 || hits[0].Item.ID != req.ID || hits[0].Stage != memory.StageFTS || info.Semantic != memory.SemanticUnavailable {
			t.Fatalf("%v %+v %v", hitKeys(hits), info, err)
		}
		if took := time.Since(start); took > time.Second {
			t.Fatalf("search took %s with the sidecar down", took)
		}
	})

	t.Run("AC-S2-023 slow sidecar: deadline, then the queue catches up", func(t *testing.T) {
		fake := embedtest.New(t)
		fake.Concept("xóa nhóm", "gỡ bỏ cụm")
		w := memory.EnableSemantic(e.pool, fake.Client(), embed.DefaultMinSim)
		fake.SetDelay(10 * time.Second)
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "UI-9", Kind: "ui", Content: auth("Gỡ bỏ cụm bài tập")}) // the write does not wait
		start := time.Now()
		var after int
		e.must(e.W, func(tx pgx.Tx) error {
			hits, info, err := memory.SearchWithInfo(e.ctx, tx, e.W, memory.SearchQuery{ProjectID: e.A, Q: "xóa nhóm"})
			if err != nil || len(hits) != 1 || hits[0].Item.Key != "REQ-1" || info.Semantic != memory.SemanticUnavailable {
				t.Errorf("slow sidecar: %v %+v %v", hitKeys(hits), info, err)
			}
			return tx.QueryRow(e.ctx, `SELECT count(*) FROM knowledge`).Scan(&after) // the transaction is still usable
		})
		if took := time.Since(start); took < 700*time.Millisecond || took > 2*time.Second || after == 0 {
			t.Fatalf("search took %s (deadline 800 ms), %d rows read afterwards", took, after)
		}
		fake.SetDelay(0)
		if n := e.drain(w); n != 2 { // both rows were pending: the queue retries later
			t.Fatalf("worker embedded %d rows after the sidecar recovered, want 2", n)
		}
		hits, info, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "xóa nhóm"})
		if err != nil || info.Semantic != memory.SemanticOK || !slices.Equal(hitKeys(hits), []string{"REQ-1", "UI-9"}) || hits[1].Stage != memory.StageSemantic {
			t.Fatalf("after recovery: %v %+v %v", hitKeys(hits), info, err)
		}
	})

	t.Run("AC-S2-023 a failing vector statement does not break the caller's transaction", func(t *testing.T) {
		memory.UseSemantic(brokenSearcher{})
		var n int
		e.must(e.W, func(tx pgx.Tx) error {
			hits, info, err := memory.SearchWithInfo(e.ctx, tx, e.W, memory.SearchQuery{ProjectID: e.A, Q: "xóa nhóm"})
			if err != nil || len(hits) != 1 || info.Semantic != memory.SemanticUnavailable {
				t.Errorf("%v %+v %v", hitKeys(hits), info, err)
			}
			return tx.QueryRow(e.ctx, `SELECT count(*) FROM knowledge`).Scan(&n)
		})
		if n == 0 {
			t.Fatal("the transaction was not usable after the semantic stage failed")
		}
	})
}

// brokenSearcher answers with a vector PostgreSQL refuses (NaN).
type brokenSearcher struct{}

func (brokenSearcher) Query(context.Context, string) (memory.SemanticQuery, error) {
	v := make([]float32, embed.DefaultDim)
	v[0] = float32(math.NaN())
	return memory.SemanticQuery{Model: embed.DefaultModel, Dim: embed.DefaultDim, Vector: v, MinSim: 0.5}, nil
}
