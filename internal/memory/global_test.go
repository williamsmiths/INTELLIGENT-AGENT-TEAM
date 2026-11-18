package memory_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

func (e *env) global(actor authn.Principal, q memory.GlobalQuery) []memory.Hit {
	e.t.Helper()
	var hits []memory.Hit
	e.must(actor, func(tx pgx.Tx) (err error) {
		hits, err = memory.ListGlobal(e.ctx, tx, actor, q)
		return err
	})
	return hits
}

func TestGlobalExperience(t *testing.T) {
	e := newEnv(t)
	t.Run("AC-H-085 propose, confirm, reuse", func(t *testing.T) {
		exp := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustCandidate, Basis: "inferred",
			Title: "Chạy lại test sau khi sửa migration", Body: "Sau khi đổi schema, chạy lại toàn bộ test tích hợp trước khi báo xong.",
			AppliesWhen: "task có thay đổi schema hoặc migration", Limits: "không áp dụng cho thay đổi chỉ có giao diện"}
		in := memory.CreateInput{Level: memory.LevelGlobal, Key: "EXP-1", Kind: "experience", Content: exp}

		wrongKind := in
		wrongKind.Kind = "requirement"
		_, err := e.create(e.W, wrongKind)
		wantKind(t, err, kerr.Invalid, "GLOBAL kind requirement")
		for name, change := range map[string]func(*memory.CreateInput){
			"no applies_when": func(c *memory.CreateInput) { c.AppliesWhen = "" },
			"no limits":       func(c *memory.CreateInput) { c.Limits = " " },
			"a project":       func(c *memory.CreateInput) { c.ProjectID = e.A },
		} {
			bad := in
			change(&bad)
			_, err := e.create(e.W, bad)
			wantKind(t, err, kerr.Invalid, "GLOBAL with "+name)
		}
		withSource := in
		withSource.SourceIDs = []string{e.source(e.W, e.A, "nguồn của project").ID}
		_, err = e.create(e.W, withSource)
		wantKind(t, err, kerr.Invalid, "GLOBAL with source_ids")
		official := in
		official.Trust, official.Basis = memory.TrustAuthoritative, ""
		_, err = e.create(e.W, official)
		wantKind(t, err, kerr.Forbidden, "agent writes AUTHORITATIVE GLOBAL")

		res, err := e.create(e.W, in)
		if err != nil || !res.Created || res.Item.ProjectID != "" || len(res.Item.SourceIDs) != 0 || res.Item.NS != e.W.ID {
			t.Fatalf("propose: %+v %v", res.Item, err)
		}
		// Row-level security refuses a project-less journal row from an agent scope, so the
		// event comes back for the caller to append as Runtime.
		if len(res.Deferred) != 1 || res.Deferred[0].Type != memory.EventAdded || res.Deferred[0].ProjectID != "" {
			t.Fatalf("deferred events = %+v", res.Deferred)
		}
		if err := e.as(e.W, func(tx pgx.Tx) error {
			_, err := journal.Append(e.ctx, tx, res.Deferred[0])
			return err
		}); err == nil {
			t.Fatal("an agent scope wrote a project-less journal row; the Deferred hand-off is no longer needed")
		}
		if err := store.InScope(e.ctx, e.pool, store.Runtime, func(tx pgx.Tx) error {
			_, err := journal.Append(e.ctx, tx, res.Deferred[0])
			return err
		}); err != nil {
			t.Fatalf("append deferred event as runtime: %v", err)
		}

		// Until the owner confirms, only the author and the owner see the proposal.
		taskA, taskB := e.task(e.A, "T-A", "RUNNING"), e.task(e.B, "T-B", "RUNNING")
		packetIn := func(projectID, taskID string) memory.PacketInput {
			return memory.PacketInput{ProjectID: projectID, TaskID: taskID, TaskKey: "T", TaskTitle: "Thêm cột vào schema bằng migration",
				DesiredOutcome: "migration chạy được và test tích hợp qua", Revision: 1}
		}
		if hits := e.global(e.V, memory.GlobalQuery{}); len(hits) != 0 {
			t.Fatalf("another agent lists the unconfirmed proposal: %v", hitKeys(hits))
		}
		if hits, err := e.search(e.V, memory.SearchQuery{ProjectID: e.A, Q: "migration"}); err != nil || len(hits) != 0 {
			t.Fatalf("another agent finds the unconfirmed proposal: %v %v", hitKeys(hits), err)
		}
		if p := e.packet(e.V, packetIn(e.A, taskA)); strings.Contains(p.Text, "EXP-1") || slices.Contains(p.Blocks, memory.BlockGlobal) {
			t.Fatalf("unconfirmed GLOBAL item in a packet:\n%s", p.Text)
		}
		if mine := e.global(e.W, memory.GlobalQuery{}); len(mine) != 1 || mine[0].Item.ID != res.Item.ID {
			t.Fatalf("author does not list its own proposal: %v", hitKeys(mine))
		}
		if all := e.global(e.Owner, memory.GlobalQuery{Trust: memory.TrustCandidate}); len(all) != 1 || all[0].Item.ID != res.Item.ID {
			t.Fatalf("owner trust=CANDIDATE: %v", hitKeys(all))
		}
		// The same key proposed by another agent does not collide (private namespace, AC-H-107).
		other := in
		other.Title = "Kinh nghiệm khác cùng key"
		if _, err := e.create(e.V, other); err != nil {
			t.Fatalf("same GLOBAL key by another agent: %v", err)
		}

		done, err := e.confirm(e.Owner, res.Item.ID, 1)
		if err != nil || done.Item.Trust != memory.TrustAuthoritative || done.Item.NS != "" || done.Item.ProjectID != "" {
			t.Fatalf("confirm: %+v %v", done.Item, err)
		}
		if got := e.events(""); !slices.Equal(got, []string{memory.EventAdded, memory.EventConfirmed}) {
			t.Fatalf("system journal = %v", got)
		}
		for _, who := range []struct {
			actor   authn.Principal
			project string
			task    string
		}{{e.V, e.A, taskA}, {e.X, e.B, taskB}} {
			p := e.packet(who.actor, packetIn(who.project, who.task))
			begin := strings.Index(p.Text, memory.UnverifiedBegin)
			for _, want := range []string{"[GLOBAL · AUTHORITATIVE] EXP-1 v1", "Áp dụng khi: task có thay đổi schema", "Giới hạn: không áp dụng cho thay đổi chỉ có giao diện"} {
				if at := strings.Index(p.Text, want); at < 0 || (begin >= 0 && at > begin) {
					t.Fatalf("%s: packet lacks %q as confirmed content:\n%s", who.actor.Name, want, p.Text)
				}
			}
			hits, err := e.search(who.actor, memory.SearchQuery{ProjectID: who.project, Q: "migration"})
			if err != nil || len(hits) != 1 || hits[0].Item.Level != memory.LevelGlobal {
				t.Fatalf("%s: project search does not include the confirmed GLOBAL item: %v %v", who.actor.Name, hitKeys(hits), err)
			}
		}
	})
}
