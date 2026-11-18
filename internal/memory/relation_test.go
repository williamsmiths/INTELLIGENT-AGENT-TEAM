package memory_test

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func (e *env) packet(actor authn.Principal, in memory.PacketInput) memory.Packet {
	e.t.Helper()
	var p memory.Packet
	e.must(actor, func(tx pgx.Tx) (err error) {
		p, err = memory.AssemblePacket(e.ctx, tx, actor, in)
		return err
	})
	return p
}

func TestRelations(t *testing.T) {
	e := newEnv(t)
	t.Run("AC-H-103 retrieval through relations", func(t *testing.T) {
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: auth("Xóa nhóm phải xác nhận")})
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "API-1", Kind: "api", Content: auth("DELETE /groups/{id}")})
		e.mustCreate(e.V, memory.CreateInput{ProjectID: e.A, Key: "TEST-1", Kind: "flow", Content: cand("Kịch bản kiểm thử xóa nhóm")})
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "API-2", Kind: "api", Content: auth("GET /reports không liên quan")})
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.B, Key: "B-ONLY", Kind: "api", Content: auth("API của project B")})
		note := agentNote("NOTE-1", "ghi nhớ riêng")
		note.ProjectID = e.A
		e.mustCreate(e.W, note)
		global := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred", Title: "Luôn chạy test trước khi báo xong",
			AppliesWhen: "mọi task có test", Limits: "không áp dụng cho tài liệu"}
		e.mustCreate(e.W, memory.CreateInput{Level: memory.LevelGlobal, Key: "EXP-1", Kind: "experience", Content: global})

		implements, err := e.relate(e.W, e.A, "API-1", "implements", "REQ-1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := e.relate(e.V, e.A, "TEST-1", "verifies", "REQ-1"); err != nil {
			t.Fatal(err)
		}
		taskID := e.task(e.A, "T-1", "RUNNING", "REQ-1")
		in := memory.PacketInput{ProjectID: e.A, TaskID: taskID, TaskKey: "T-1", TaskTitle: "Thêm xác nhận khi xóa nhóm",
			DesiredOutcome: "Người dùng phải xác nhận", Revision: 1, DependencyVersions: map[string]int{"REQ-1": 1}}

		p := e.packet(e.W, in)
		for _, want := range []string{"API-1", "TEST-1", "`implements` tới REQ-1", "`verifies` tới REQ-1"} {
			if !strings.Contains(p.Text, want) {
				t.Fatalf("packet lacks %q:\n%s", want, p.Text)
			}
		}
		if strings.Contains(p.Text, "API-2") || strings.Contains(p.Text, "B-ONLY") {
			t.Fatalf("packet contains an unrelated item:\n%s", p.Text)
		}

		for name, ends := range map[string][2]string{
			"key of another project": {"B-ONLY", "REQ-1"},
			"missing key":            {"REQ-1", "NOPE-1"},
			"AGENT-level key":        {"NOTE-1", "REQ-1"},
			"GLOBAL key":             {"EXP-1", "REQ-1"},
			"same key twice":         {"REQ-1", "REQ-1"},
		} {
			_, err := e.relate(e.W, e.A, ends[0], "depends_on", ends[1])
			wantKind(t, err, kerr.Invalid, "relation with "+name)
		}
		_, err = e.relate(e.W, e.A, "API-1", "likes", "REQ-1")
		wantKind(t, err, kerr.Invalid, "unknown relation type")
		_, err = e.relate(e.V, e.A, "API-1", "implements", "REQ-1")
		wantKind(t, err, kerr.Conflict, "relation that is already live")
		_, err = e.relate(e.X, e.A, "API-1", "impacts", "REQ-1")
		wantKind(t, err, kerr.NotFound, "relation by a non-member")

		closeAs := func(actor authn.Principal) error {
			return e.as(actor, func(tx pgx.Tx) error {
				_, err := memory.CloseRelation(e.ctx, tx, actor, implements.ID)
				return err
			})
		}
		wantKind(t, closeAs(e.V), kerr.Forbidden, "close by another worker")
		wantKind(t, closeAs(e.X), kerr.NotFound, "close by a non-member")
		if err := closeAs(e.W); err != nil {
			t.Fatalf("close by the creator: %v", err)
		}
		wantKind(t, closeAs(e.W), kerr.Conflict, "close twice")

		live := e.relations(e.V, e.A, "REQ-1")
		if len(live) != 1 || live[0].Type != "verifies" || live[0].FromKey != "TEST-1" {
			t.Fatalf("live relations of REQ-1 = %+v", live)
		}
		after := e.packet(e.W, in)
		if strings.Contains(after.Text, "API-1") || !strings.Contains(after.Text, "TEST-1") {
			t.Fatalf("closed relation still feeds the packet:\n%s", after.Text)
		}
		if after.Hash == p.Hash {
			t.Fatal("context hash did not change with the content")
		}
	})
}
