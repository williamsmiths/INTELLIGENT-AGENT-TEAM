package memory_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

// twin holds what one of two look-alike projects contains: the same keys and
// the same words, plus one marker that exists only there.
type twin struct {
	project, task, marker, source string
	in                            memory.PacketInput
}

// seedTwins gives projects A and B the same keys (REQ-1, API-1), the same words
// and the same shape, written by a member of each.
func seedTwins(e *env) (a, b twin) {
	e.t.Helper()
	seed := func(project, marker string, member authn.Principal) twin {
		req := auth("Xóa nhóm phải xác nhận")
		req.Scope, req.Body = "feature:editor", "Xóa nhóm câu hỏi phải xác nhận. "+marker
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: project, Key: "REQ-1", Kind: "requirement", Content: req})
		api := cand("API xóa nhóm câu hỏi")
		api.Scope, api.Body = "feature:editor", "DELETE /groups/{id} "+marker
		e.mustCreate(member, memory.CreateInput{ProjectID: project, Key: "API-1", Kind: "api", Content: api})
		if _, err := e.relate(member, project, "API-1", "implements", "REQ-1"); err != nil {
			e.t.Fatal(err)
		}
		t := twin{project: project, marker: marker, task: e.task(project, "T-1", "RUNNING", "REQ-1")}
		t.source = e.source(member, project, "nguồn "+marker).ID
		e.must(member, func(tx pgx.Tx) error {
			_, err := memory.PostBlackboard(e.ctx, tx, member, memory.BlackboardInput{ProjectID: project, TaskID: t.task, Kind: "finding", Body: "xóa nhóm: " + marker})
			return err
		})
		t.in = memory.PacketInput{ProjectID: project, TaskID: t.task, TaskKey: "T-1", TaskTitle: "Xác nhận khi xóa nhóm",
			DesiredOutcome: "xóa nhóm câu hỏi phải xác nhận", Scope: "feature:editor", Revision: 1, DependencyVersions: map[string]int{"REQ-1": 1}}
		return t
	}
	return seed(e.A, "DẤU-RIÊNG-ALPHA", e.W), seed(e.B, "DẤU-RIÊNG-BETA", e.X)
}

func TestProjectIsolation(t *testing.T) {
	e := newEnv(t)
	a, b := seedTwins(e)

	t.Run("AC-H-080 no cross-project retrieval", func(t *testing.T) {
		for _, q := range []string{"REQ-1", "xoa nhom cau hoi", "API-1", "nhom cau hoii", ""} {
			hits, err := e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: q})
			if err != nil || len(hits) == 0 {
				t.Fatalf("search %q: %v %v", q, hitKeys(hits), err)
			}
			for _, h := range hits {
				if h.Item.ProjectID != e.A || strings.Contains(h.Item.Body, b.marker) {
					t.Fatalf("search %q returned a row of another project: %+v", q, h.Item)
				}
			}
		}
		if p := e.packet(e.W, a.in); !strings.Contains(p.Text, a.marker) || strings.Contains(p.Text, b.marker) {
			t.Fatalf("packet of A is wrong:\n%s", p.Text)
		}
		e.must(e.W, func(tx pgx.Tx) error {
			posts, err := memory.ListBlackboard(e.ctx, tx, e.W, e.A, "", 0, 0)
			if err == nil && (len(posts) != 1 || posts[0].ProjectID != e.A) {
				t.Fatalf("blackboard of A = %+v", posts)
			}
			return err
		})

		// Calling project B directly: it does not exist for W.
		calls := map[string]func(tx pgx.Tx) error{
			"search": func(tx pgx.Tx) error {
				_, err := memory.Search(e.ctx, tx, e.W, memory.SearchQuery{ProjectID: e.B, Q: "REQ-1"})
				return err
			},
			"packet": func(tx pgx.Tx) error { _, err := memory.AssemblePacket(e.ctx, tx, e.W, b.in); return err },
			"blackboard": func(tx pgx.Tx) error {
				_, err := memory.ListBlackboard(e.ctx, tx, e.W, e.B, "", 0, 0)
				return err
			},
			"relations": func(tx pgx.Tx) error { _, err := memory.ListRelations(e.ctx, tx, e.W, e.B, "REQ-1"); return err },
			"source":    func(tx pgx.Tx) error { _, err := memory.GetSource(e.ctx, tx, e.W, b.source); return err },
			"new source": func(tx pgx.Tx) error {
				_, err := memory.CreateSource(e.ctx, tx, e.W, memory.SourceInput{ProjectID: e.B, Kind: "note", Content: "x"})
				return err
			},
		}
		for name, call := range calls {
			wantKind(t, e.as(e.W, call), kerr.NotFound, name+" on project B")
		}
		e.must(e.W, func(tx pgx.Tx) error { // PM-H-04: a member reads a source of its project
			s, err := memory.GetSource(e.ctx, tx, e.W, a.source)
			if err == nil && (s.ProjectID != e.A || s.Origin != "AGENT") {
				t.Fatalf("source of A = %+v", s)
			}
			return err
		})

		// SQL with no project condition at all, under W's scope: row-level security still returns A only.
		e.must(e.W, func(tx pgx.Tx) error {
			for _, table := range []string{"knowledge", "relation", "blackboard", "source", "journal", "task"} {
				var foreign, own int
				err := tx.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE project_id IS DISTINCT FROM $1), count(*) FILTER (WHERE project_id = $1) FROM `+table, e.A).Scan(&foreign, &own)
				if err != nil {
					return err
				}
				if foreign != 0 || own == 0 {
					t.Fatalf("%s under W's scope: %d foreign rows, %d own rows", table, foreign, own)
				}
			}
			return nil
		})
	})

	t.Run("AC-H-084 an agent of two projects carries no context across", func(t *testing.T) {
		for _, n := range []struct{ project, key, title string }{
			{e.A, "NOTE-1", "Ghi nhớ của Z ở alpha: parser dùng regex"},
			{e.B, "NOTE-1", "Ghi nhớ của Z ở beta: kho dùng FIFO"},
		} {
			note := agentNote(n.key, n.title)
			note.ProjectID = n.project
			e.mustCreate(e.Z, note)
		}
		inA, inB := e.packet(e.Z, a.in), e.packet(e.Z, b.in)
		if !strings.Contains(inA.Text, "parser dùng regex") || !strings.Contains(inA.Text, a.marker) {
			t.Fatalf("packet in A lacks A's content:\n%s", inA.Text)
		}
		for _, leaked := range []string{a.marker, "parser dùng regex", "Ghi nhớ của Z ở alpha"} {
			if strings.Contains(inB.Text, leaked) {
				t.Fatalf("packet in B contains %q of project A:\n%s", leaked, inB.Text)
			}
		}
		if !strings.Contains(inB.Text, "kho dùng FIFO") || !strings.Contains(inB.Text, b.marker) || inA.Hash == inB.Hash {
			t.Fatalf("packet in B must use B's memory and another hash:\n%s", inB.Text)
		}
		// PM-H-13: another member's packet, and the owner's, never contain Z's AGENT memory.
		for _, viewer := range []authn.Principal{e.W, e.Owner} {
			if p := e.packet(viewer, a.in); strings.Contains(p.Text, "parser dùng regex") {
				t.Fatalf("%s's packet contains Z's AGENT memory", viewer.Name)
			}
		}
	})
}
