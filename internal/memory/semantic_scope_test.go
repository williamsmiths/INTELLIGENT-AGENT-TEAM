package memory_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/embed"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func TestSemanticProjectIsolation(t *testing.T) {
	e := newEnv(t)
	fake, w := useFake(e)
	fake.Concept("xác nhận trước khi xóa", "hỏi lại rồi mới gỡ")
	same := memory.CreateInput{Key: "REQ-9", Kind: "requirement", Content: titled(auth("Xác nhận trước khi xóa"), "Áp dụng cho mọi nhóm.")}
	mk := func(actor authn.Principal, project string, in memory.CreateInput) memory.Item {
		in.ProjectID = project
		return e.mustCreate(actor, in)
	}
	inA, inB := mk(e.Owner, e.A, same), mk(e.Owner, e.B, same) // two projects, identical content
	note := mk(e.V, e.A, memory.CreateInput{Level: memory.LevelAgent, Key: "N-1", Kind: "decision", Content: cand("Ghi nhớ riêng: xác nhận trước khi xóa")})
	draft := e.mustCreate(e.V, memory.CreateInput{Level: memory.LevelGlobal, Key: "EXP-9", Kind: "experience", Content: memory.Content{
		Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred", Title: "Luôn xác nhận trước khi xóa",
		AppliesWhen: "task có thao tác xóa", Limits: "không áp dụng cho dữ liệu tạm"}})
	if n := e.drain(w); n != 4 {
		t.Fatalf("embedded %d rows, want 4", n)
	}
	const paraphrase = "hỏi lại rồi mới gỡ" // shares no word with the items

	ids := func(hits []memory.Hit) []string {
		out := make([]string, len(hits))
		for i, h := range hits {
			out[i] = h.Item.ID
		}
		slices.Sort(out)
		return out
	}
	sorted := func(s ...string) []string { slices.Sort(s); return s }

	t.Run("AC-S2-022 a member of A gets no hit of B", func(t *testing.T) {
		for name, tc := range map[string]struct {
			actor authn.Principal
			want  []string
		}{
			"worker of A only":           {e.W, []string{inA.ID}},
			"worker of both, asking A":   {e.Z, []string{inA.ID}},        // row-level security shows Z both rows: the query's own project predicate decides
			"author of the AGENT note":   {e.V, sorted(inA.ID, note.ID)}, // its unconfirmed GLOBAL draft is not part of a project search (PM-H-09)
			"owner asking A":             {e.Owner, sorted(inA.ID, note.ID)},
			"worker of B asking its own": {e.X, nil},
		} {
			project := e.A
			if tc.actor.ID == e.X.ID {
				project = e.B
				tc.want = []string{inB.ID}
			}
			hits, info, err := e.searchInfo(tc.actor, memory.SearchQuery{ProjectID: project, Q: paraphrase})
			if err != nil || info.Semantic != memory.SemanticOK || !slices.Equal(ids(hits), tc.want) {
				t.Errorf("%s: hits %v info %+v err %v, want ids %v", name, hitKeys(hits), info, err, tc.want)
			}
			for _, h := range hits {
				if h.Stage != memory.StageSemantic || (h.Item.ProjectID != project && h.Item.Level != memory.LevelGlobal) {
					t.Errorf("%s: hit %s stage %s project %s", name, h.Item.Key, h.Stage, h.Item.ProjectID)
				}
			}
		}
		_, _, err := e.searchInfo(e.W, memory.SearchQuery{ProjectID: e.B, Q: paraphrase})
		wantKind(t, err, kerr.NotFound, "W searching project B")
	})

	t.Run("AC-S2-022 bare vector SQL without a project predicate under an agent scope", func(t *testing.T) {
		vec := embed.Literal(fake.Vector(paraphrase))
		bare := func(actor authn.Principal) []string {
			var got []string
			e.must(actor, func(tx pgx.Tx) error {
				rows, err := tx.Query(e.ctx, `SELECT knowledge_id::text FROM knowledge_embedding ORDER BY embedding <=> $1::text::vector`, vec)
				if err != nil {
					return err
				}
				got, err = pgx.CollectRows(rows, pgx.RowTo[string])
				return err
			})
			slices.Sort(got)
			return got
		}
		for name, tc := range map[string]struct {
			actor authn.Principal
			want  []string
		}{
			"worker of A": {e.W, []string{inA.ID}},
			"worker of B": {e.X, []string{inB.ID}},
			"author in A": {e.V, sorted(inA.ID, note.ID, draft.ID)},
			"owner":       {e.Owner, sorted(inA.ID, inB.ID, note.ID, draft.ID)},
		} {
			if got := bare(tc.actor); !slices.Equal(got, tc.want) {
				t.Errorf("%s sees embeddings %v, want %v", name, got, tc.want)
			}
		}
		// The same holds for a join the caller writes itself, and for the nearest-neighbour row.
		e.must(e.W, func(tx pgx.Tx) error {
			var n int
			err := tx.QueryRow(e.ctx, `SELECT count(*) FROM knowledge_embedding e LEFT JOIN knowledge k ON k.id = e.knowledge_id
				WHERE e.knowledge_id = ANY($1::uuid[])`, []string{inB.ID, note.ID, draft.ID}).Scan(&n)
			if err == nil && n != 0 {
				t.Errorf("W reads %d embeddings of rows it may not see", n)
			}
			return err
		})
	})
}

// Spec S2 B.1: in a context packet the semantic stage only fills the related
// and experience blocks below their limit; it never replaces or reorders.
func TestSemanticPacketFill(t *testing.T) {
	e := newEnv(t)
	fake, w := useFake(e)
	fake.Concept("chuyển nút thêm vào part", "vị trí điều khiển tạo cụm bài", "bố cục thanh công cụ soạn đề")
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "ui", Content: titled(auth("Nút thêm nằm trong Part"), "Chuyển nút thêm vào Part.")})
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "DEC-1", Kind: "decision", Content: auth("Giữ nguyên thứ tự Part")})
	if _, err := e.relate(e.Owner, e.A, "DEC-1", "impacts", "REQ-1"); err != nil {
		t.Fatal(err)
	}
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "UI-5", Kind: "ui", Content: auth("Vị trí điều khiển tạo cụm bài")})
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.B, Key: "UI-5", Kind: "ui", Content: auth("Vị trí điều khiển tạo cụm bài (project khác)")})
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "API-8", Kind: "api", Content: auth("Xuất điểm ra tệp CSV")})
	global := func(key, title string) {
		g := e.mustCreate(e.W, memory.CreateInput{Level: memory.LevelGlobal, Key: key, Kind: "experience", Content: memory.Content{
			Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred", Title: title, AppliesWhen: "task đổi giao diện", Limits: "không áp dụng cho API"}})
		if _, err := e.confirm(e.Owner, g.ID, 1); err != nil {
			t.Fatal(err)
		}
	}
	global("EXP-1", "Chụp ảnh màn hình sau khi đổi Part")   // matched by full-text ("Part")
	global("EXP-2", "Xem lại bố cục thanh công cụ soạn đề") // matched only by meaning
	global("EXP-3", "Đặt tên nhánh theo mã task")           // matched by neither
	e.drain(w)

	in := memory.PacketInput{ProjectID: e.A, TaskID: e.task(e.A, "T-1", "RUNNING", "REQ-1"), TaskKey: "T-1", TaskTitle: "Chuyển nút thêm vào Part",
		DesiredOutcome: "Mỗi Part có nút riêng", Revision: 1, DependencyVersions: map[string]int{"REQ-1": 1}}
	withSem := e.packet(e.W, in)
	memory.UseSemantic(nil)
	lexical := e.packet(e.W, in)
	memory.EnableSemantic(e.pool, fake.Client(), embed.DefaultMinSim)

	keysOf := func(p memory.Packet, block string) []string {
		var keys []string
		for _, r := range p.Refs {
			if r.Block == block {
				keys = append(keys, r.Key)
			}
		}
		return keys
	}
	t.Run("AC-S2-021 packet: relation-derived and full-text items first, semantic appended", func(t *testing.T) {
		if got := keysOf(lexical, memory.BlockRelated); !slices.Equal(got, []string{"DEC-1"}) {
			t.Fatalf("lexical related = %v", got)
		}
		if got := keysOf(withSem, memory.BlockRelated); !slices.Equal(got, []string{"DEC-1", "UI-5"}) {
			t.Errorf("related with semantic = %v, want DEC-1 (relation) then UI-5 (semantic); never the pinned REQ-1", got)
		}
		if got := keysOf(lexical, memory.BlockGlobal); !slices.Equal(got, []string{"EXP-1"}) {
			t.Fatalf("lexical global = %v", got)
		}
		if got := keysOf(withSem, memory.BlockGlobal); !slices.Equal(got, []string{"EXP-1", "EXP-2"}) {
			t.Errorf("global with semantic = %v, want EXP-1 (full-text) then EXP-2 (semantic)", got)
		}
		if !strings.Contains(withSem.Text, "gần nghĩa với task") || strings.Contains(withSem.Text, "project khác") || strings.Contains(withSem.Text, "API-8") {
			t.Errorf("packet text is wrong:\n%s", withSem.Text)
		}
		if keysOf(withSem, memory.BlockPinned)[0] != "REQ-1" || !slices.Equal(withSem.Blocks[:3], lexical.Blocks[:3]) {
			t.Errorf("blocks changed: %v vs %v", withSem.Blocks, lexical.Blocks)
		}
	})

	t.Run("AC-S2-021 packet over budget: semantic lines are dropped before any structured block", func(t *testing.T) {
		in.MaxChars = lexical.Chars // exactly room for everything structured
		tight := e.packet(e.W, in)
		if !slices.Equal(tight.Dropped, []string{"semantic"}) || tight.Text != lexical.Text || tight.Hash != lexical.Hash {
			t.Fatalf("dropped %v blocks %v (lexical %v)\n%s", tight.Dropped, tight.Blocks, lexical.Blocks, tight.Text)
		}
		in.MaxChars = withSem.Chars - 1 // one character short of everything
		if p := e.packet(e.W, in); p.Text != lexical.Text {
			t.Fatalf("one character over budget did not fall back to the structured packet:\n%s", p.Text)
		}
		in.MaxChars = 0
	})

	t.Run("AC-S2-023 packet with the sidecar failing equals the packet without semantic", func(t *testing.T) {
		fake.SetStatus(503)
		in.TaskTitle += " " // a text the query cache has not seen
		lexicalIn := in
		got := e.packet(e.W, in)
		memory.UseSemantic(nil)
		want := e.packet(e.W, lexicalIn)
		if got.Hash != want.Hash || got.Text != want.Text {
			t.Fatalf("packet differs when the sidecar fails:\n%s", got.Text)
		}
	})
}
