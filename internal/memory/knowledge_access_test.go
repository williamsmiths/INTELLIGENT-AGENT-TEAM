package memory_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func agentNote(key, title string) memory.CreateInput {
	return memory.CreateInput{Level: memory.LevelAgent, Key: key, Kind: "decision",
		Content: memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred", Title: title}}
}

func TestKnowledgeAccess(t *testing.T) {
	e := newEnv(t)

	t.Run("AC-H-004 outsider and another agent's private memory", func(t *testing.T) {
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: auth("Xóa nhóm phải xác nhận")})
		note := agentNote("NOTE-1", "bí mật riêng của W về parser")
		note.ProjectID = e.A
		private := e.mustCreate(e.W, note)

		_, err := e.search(e.X, memory.SearchQuery{ProjectID: e.A, Q: "REQ-1"})
		wantKind(t, err, kerr.NotFound, "outsider search")
		_, err = e.create(e.X, memory.CreateInput{ProjectID: e.A, Key: "REQ-X", Kind: "requirement", Content: cand("lọt")})
		wantKind(t, err, kerr.NotFound, "outsider create")

		for _, q := range []string{"", "NOTE-1", "parser", "bi mat rieng"} {
			hits, err := e.search(e.V, memory.SearchQuery{ProjectID: e.A, Q: q})
			if err != nil {
				t.Fatalf("V search %q: %v", q, err)
			}
			for _, h := range hits {
				if h.Item.ID == private.ID || h.Item.Level == memory.LevelAgent {
					t.Fatalf("V sees W's AGENT item with q=%q", q)
				}
			}
		}
		for _, who := range []struct {
			name string
			hits func() ([]memory.Hit, error)
		}{
			{"owner", func() ([]memory.Hit, error) {
				return e.search(e.Owner, memory.SearchQuery{ProjectID: e.A, Q: "NOTE-1"})
			}},
			{"author", func() ([]memory.Hit, error) { return e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "parser"}) }},
		} {
			hits, err := who.hits()
			if err != nil || !slices.ContainsFunc(hits, func(h memory.Hit) bool { return h.Item.ID == private.ID }) {
				t.Fatalf("%s does not see the AGENT item: %v %v", who.name, hitKeys(hits), err)
			}
		}

		// The project journal carries ids and codes, never the content or the private key.
		e.must(e.V, func(tx pgx.Tx) error {
			rows, err := tx.Query(e.ctx, `SELECT payload::text FROM journal WHERE project_id = $1`, e.A)
			if err != nil {
				return err
			}
			payloads, err := pgx.CollectRows(rows, pgx.RowTo[string])
			joined := strings.Join(payloads, "\n")
			if len(payloads) < 2 || strings.Contains(joined, "parser") || strings.Contains(joined, "NOTE-1") || strings.Contains(joined, "Xóa nhóm") {
				t.Fatalf("journal payloads leak content or are missing: %s", joined)
			}
			return err
		})
	})

	t.Run("AC-H-005 worker supersedes others' or AUTHORITATIVE items, then its own", func(t *testing.T) {
		req := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "REQ-5", Kind: "requirement", Content: cand("của W, sẽ được xác nhận")})
		if _, err := e.confirm(e.Owner, req.ID, 1); err != nil {
			t.Fatal(err)
		}
		leads := e.mustCreate(e.L, memory.CreateInput{ProjectID: e.A, Key: "REQ-2", Kind: "requirement", Content: cand("của Lead")})
		mine := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "NOTE-5", Kind: "decision", Content: cand("ghi chú v1")})

		next := memory.SupersedeInput{ExpectedVersion: 1, Content: cand("bản mới")}
		_, err := e.supersede(e.W, req.ID, next)
		wantKind(t, err, kerr.Forbidden, "supersede own AUTHORITATIVE item")
		_, err = e.supersede(e.W, leads.ID, next)
		wantKind(t, err, kerr.Forbidden, "supersede the lead's item")

		res, err := e.supersede(e.W, mine.ID, memory.SupersedeInput{ExpectedVersion: 1, Content: cand("ghi chú v2")})
		if err != nil || res.Item.Version != 2 || res.Item.Trust != memory.TrustCandidate || !res.Item.IsCurrent || res.Item.Key != "NOTE-5" {
			t.Fatalf("supersede own: %+v, %v", res.Item, err)
		}
		if old := e.row(mine.ID); old.Trust != memory.TrustHistorical || old.Current || old.EndedReason != "superseded" || old.ReplacedBy != res.Item.ID {
			t.Fatalf("old version = %+v", old)
		}
		_, err = e.supersede(e.X, mine.ID, next)
		wantKind(t, err, kerr.NotFound, "outsider supersede")

		// Spec §0: AGENT memory is written only by its agent; the owner may still invalidate it (PM-H-07).
		note := agentNote("NOTE-OWN", "ghi nhớ riêng của W")
		note.ProjectID = e.A
		private := e.mustCreate(e.W, note)
		_, err = e.supersede(e.Owner, private.ID, memory.SupersedeInput{ExpectedVersion: 1, Content: cand("owner viết hộ")})
		wantKind(t, err, kerr.Forbidden, "owner supersedes an agent's AGENT memory")
		if _, err := e.invalidate(e.Owner, private.ID, memory.InvalidateInput{ExpectedVersion: 1, Reason: "sai"}); err != nil {
			t.Fatalf("owner invalidates AGENT memory: %v", err)
		}
	})

	t.Run("AC-H-107 the same key in private and shared namespaces", func(t *testing.T) {
		w := agentNote("NOTE-7", "ghi nhớ của W")
		w.ProjectID = e.A
		e.mustCreate(e.W, w)
		v := agentNote("NOTE-7", "ghi nhớ của V")
		v.ProjectID = e.A
		got := e.mustCreate(e.V, v)
		if got.NS != e.V.ID || got.Level != memory.LevelAgent {
			t.Fatalf("V's AGENT item = %+v", got)
		}
		shared := e.mustCreate(e.V, memory.CreateInput{ProjectID: e.A, Key: "NOTE-7", Kind: "decision", Content: cand("mục chung cùng key")})
		if shared.NS != "" || shared.Level != memory.LevelProject {
			t.Fatalf("PROJECT item = %+v", shared)
		}
		// A second PROJECT item with a taken key is a real conflict, in the shared namespace only.
		_, err := e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: "NOTE-7", Kind: "decision", Content: cand("nội dung khác")})
		wantKind(t, err, kerr.Conflict, "taken PROJECT key")
	})

	t.Run("AC-H-101 selective remembering", func(t *testing.T) {
		body := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred", Scope: "feature:editor",
			Title: "Parser bỏ qua dòng trống", Body: "Hàm parse() trong src/parser.go\nbỏ qua dòng trống."}
		first := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "NOTE-10", Kind: "decision", Content: body})
		before := len(e.events(e.A))

		retyped := body
		retyped.Body = "  Hàm parse() trong src/parser.go bỏ qua   dòng trống. "
		for _, key := range []string{"NOTE-10", "NOTE-11"} {
			res, err := e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: key, Kind: "decision", Content: retyped})
			if err != nil || res.Created || res.Item.ID != first.ID || res.Item.Version != 1 {
				t.Fatalf("rewrite with key %s: created=%v id=%s err=%v", key, res.Created, res.Item.ID, err)
			}
		}
		if after := len(e.events(e.A)); after != before {
			t.Fatalf("journal grew from %d to %d", before, after)
		}

		otherAuthor, err := e.create(e.V, memory.CreateInput{ProjectID: e.A, Key: "NOTE-12", Kind: "decision", Content: body})
		if err != nil || !otherAuthor.Created {
			t.Fatalf("other author: %+v %v", otherAuthor, err)
		}
		variants := map[string]memory.CreateInput{
			"kind":   {ProjectID: e.A, Key: "NOTE-13", Kind: "rule", Content: body},
			"aspect": {ProjectID: e.A, Key: "NOTE-14", Kind: "decision", Content: body},
			"scope":  {ProjectID: e.A, Key: "NOTE-15", Kind: "decision", Content: body},
		}
		a, s := variants["aspect"], variants["scope"]
		a.Aspect, s.Scope = memory.AspectToBe, "feature:import"
		variants["aspect"], variants["scope"] = a, s
		for name, in := range variants {
			res, err := e.create(e.W, in)
			if err != nil || !res.Created || res.Item.ID == first.ID {
				t.Fatalf("different %s: %+v %v", name, res, err)
			}
		}
	})
}
