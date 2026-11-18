package memory_test

import (
	"math"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/embed/embedtest"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

// useFake turns the semantic stage on against a fake sidecar (deterministic
// vectors; these tests are about harness logic, not about the model).
func useFake(e *env) (*embedtest.Sidecar, *embed.Worker) {
	e.t.Helper()
	fake := embedtest.New(e.t)
	w := memory.EnableSemantic(e.pool, fake.Client(), embed.DefaultMinSim)
	e.t.Cleanup(func() { memory.UseSemantic(nil) })
	return fake, w
}

// drain runs the embed worker until nothing is pending and returns the rows embedded.
func (e *env) drain(w *embed.Worker) int {
	e.t.Helper()
	total := 0
	for range 1000 {
		n, err := w.Step(e.ctx)
		if err != nil {
			e.t.Fatalf("embed worker: %v", err)
		}
		if n == 0 {
			return total
		}
		total += n
	}
	e.t.Fatalf("embed worker still finds work after %d rows: it embeds rows again", total)
	return total
}

func (e *env) searchInfo(actor authn.Principal, q memory.SearchQuery) ([]memory.Hit, memory.Info, error) {
	var hits []memory.Hit
	var info memory.Info
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		hits, info, err = memory.SearchWithInfo(e.ctx, tx, actor, q)
		return err
	})
	return hits, info, err
}

func titled(c memory.Content, body string) memory.Content { c.Body = body; return c }

func TestSemanticEmbedOnlyChangedContent(t *testing.T) {
	t.Run("AC-S2-020 create, identical re-create, confirm, supersede: exactly 2 embed requests", func(t *testing.T) {
		e := newEnv(t)
		fake, w := useFake(e)
		in := memory.CreateInput{ProjectID: e.A, Key: "NOTE-1", Kind: "rule", Content: titled(cand("Xóa nhóm câu hỏi phải xác nhận"), "Hiện hộp thoại trước khi xóa.")}
		first := e.mustCreate(e.W, in)
		if n := e.drain(w); n != 1 {
			t.Fatalf("after create: %d rows embedded, want 1", n)
		}
		in.Key = "NOTE-2" // AC-H-101: the same content again returns the old row
		again, err := e.create(e.W, in)
		if err != nil || again.Created || again.Item.ID != first.ID || e.drain(w) != 0 {
			t.Fatalf("identical re-create: created=%v err=%v", again.Created, err)
		}
		if _, err := e.confirm(e.Owner, first.ID, 1); err != nil || e.drain(w) != 0 {
			t.Fatalf("confirm (trust change) caused an embed: %v", err)
		}
		next, err := e.supersede(e.Owner, first.ID, memory.SupersedeInput{ExpectedVersion: 1,
			Content: titled(auth("Xóa nhóm câu hỏi cần nhập lại tên nhóm"), "Nội dung mới.")})
		if err != nil || e.drain(w) != 1 || e.drain(w) != 0 {
			t.Fatalf("supersede with new content must embed exactly one row: %v", err)
		}
		want := []string{"Xóa nhóm câu hỏi phải xác nhận\nHiện hộp thoại trước khi xóa.", "Xóa nhóm câu hỏi cần nhập lại tên nhóm\nNội dung mới."}
		if fake.Requests() != 2 || !slices.Equal(fake.Texts(), want) {
			t.Fatalf("sidecar received %d requests with texts %q, want 2 with %q", fake.Requests(), fake.Texts(), want)
		}
		var stored []string
		e.must(e.Owner, func(tx pgx.Tx) error {
			rows, err := tx.Query(e.ctx, `SELECT e.knowledge_id::text FROM knowledge_embedding e JOIN knowledge k ON k.id = e.knowledge_id
				WHERE e.model = $1 AND e.content_hash = k.content_hash ORDER BY k.version`, embed.DefaultModel)
			if err != nil {
				return err
			}
			stored, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		})
		if !slices.Equal(stored, []string{first.ID, next.Item.ID}) {
			t.Fatalf("stored embeddings %v, want one per version", stored)
		}
	})
}

func TestSemanticAppendsOnly(t *testing.T) {
	e := newEnv(t)
	fake, w := useFake(e)
	fake.Concept("REQ-1", "xóa nhóm", "gỡ bỏ cụm") // the query, the key-matched item and the paraphrased item mean the same
	req := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement",
		Content: titled(auth("Xóa nhóm câu hỏi phải xác nhận"), "Chỉ xóa sau khi người dùng đồng ý.")})
	near := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "UI-9", Kind: "ui",
		Content: titled(auth("Hộp thoại hỏi lại"), "Hiện ra trước khi gỡ bỏ cụm bài tập.")})
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "API-3", Kind: "api", Content: auth("Xuất điểm ra tệp CSV")})
	e.drain(w)

	t.Run("AC-S2-021 key hit first, semantic hit tagged, nothing twice", func(t *testing.T) {
		hits, info, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "REQ-1"})
		if err != nil || info.Semantic != memory.SemanticOK || len(hits) != 2 {
			t.Fatalf("hits %v info %+v err %v", hitKeys(hits), info, err)
		}
		if hits[0].Item.ID != req.ID || hits[0].Stage != memory.StageKey {
			t.Errorf("first hit is %s (%s), want REQ-1 by key", hits[0].Item.Key, hits[0].Stage)
		}
		if hits[1].Item.ID != near.ID || hits[1].Stage != memory.StageSemantic || hits[1].Score < embed.DefaultMinSim || hits[1].Score > 1.0001 {
			t.Errorf("second hit is %s (%s, score %.3f), want UI-9 by semantic with its similarity", hits[1].Item.Key, hits[1].Stage, hits[1].Score)
		}
		// REQ-1 is as close to the query as UI-9 is, so only the de-duplication keeps it from appearing twice.
		var sim float64
		for i, x := range fake.Vector("REQ-1") {
			sim += float64(x) * float64(fake.Vector(req.Title + "\n" + req.Body)[i])
		}
		if sim < embed.DefaultMinSim || math.Abs(sim-hits[1].Score) > 0.2 {
			t.Fatalf("fixture is wrong: REQ-1 has similarity %.3f to the query", sim)
		}
		plain, err := e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "REQ-1"})
		if err != nil || !slices.Equal(hitKeys(plain), hitKeys(hits)) {
			t.Errorf("Search returns %v, SearchWithInfo %v", hitKeys(plain), hitKeys(hits))
		}
	})

	t.Run("AC-S2-021 semantic fills empty slots only and embeds a query once", func(t *testing.T) {
		before := fake.Requests()
		hits, info, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "REQ-1", Limit: 1})
		if err != nil || len(hits) != 1 || hits[0].Stage != memory.StageKey || info.Semantic != memory.SemanticOK || fake.Requests() != before {
			t.Fatalf("limit 1: %v %+v %v, %d new sidecar requests", hitKeys(hits), info, err, fake.Requests()-before)
		}
		for range 3 { // same query text: cached after the first embedding (spec S2 B.1)
			if _, _, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "gỡ bỏ cụm"}); err != nil {
				t.Fatal(err)
			}
		}
		if got := fake.Requests() - before; got != 1 {
			t.Errorf("3 searches of one new query reached the sidecar %d times, want 1", got)
		}
		// Filters apply to the semantic stage like to the others.
		hits, _, err = e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "gỡ bỏ cụm", Kind: "requirement"})
		if err != nil || len(hits) != 1 || hits[0].Item.ID != req.ID || hits[0].Stage != memory.StageSemantic {
			t.Errorf("kind filter: %v %v", hitKeys(hits), err)
		}
		if hits, info, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A}); err != nil || info.Semantic != memory.SemanticOff || len(hits) != 3 {
			t.Errorf("list without query text: %v %+v %v", hitKeys(hits), info, err)
		}
	})

	t.Run("off without a searcher", func(t *testing.T) {
		memory.UseSemantic(nil)
		defer memory.EnableSemantic(e.pool, fake.Client(), embed.DefaultMinSim)
		hits, info, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.A, Q: "REQ-1"})
		if err != nil || info.Semantic != memory.SemanticOff || len(hits) != 1 {
			t.Fatalf("%v %+v %v", hitKeys(hits), info, err)
		}
	})
}
