package memory_test

import (
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func (e *env) relate(actor authn.Principal, projectID, from, typ, to string) (memory.Relation, error) {
	var r memory.Relation
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		r, err = memory.CreateRelation(e.ctx, tx, actor, memory.RelationInput{ProjectID: projectID, FromKey: from, ToKey: to, Type: typ})
		return err
	})
	return r, err
}

func (e *env) relations(actor authn.Principal, projectID, key string) []memory.Relation {
	e.t.Helper()
	var out []memory.Relation
	e.must(actor, func(tx pgx.Tx) (err error) {
		out, err = memory.ListRelations(e.ctx, tx, actor, projectID, key)
		return err
	})
	return out
}

func TestInvalidateAndMerge(t *testing.T) {
	e := newEnv(t)
	t.Run("AC-H-102 invalidate and merge", func(t *testing.T) {
		rule1 := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "RULE-1", Kind: "rule", Content: cand("Điểm làm tròn 0.5")})
		rule2 := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "RULE-2", Kind: "rule", Content: auth("Điểm làm tròn đến 0.5 gần nhất")})
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "API-1", Kind: "api", Content: auth("POST /scores")})
		if _, err := e.relate(e.V, e.A, "RULE-1", "depends_on", "API-1"); err != nil {
			t.Fatal(err)
		}

		_, err := e.invalidate(e.V, rule1.ID, memory.InvalidateInput{ExpectedVersion: 1, Reason: "trùng"})
		wantKind(t, err, kerr.Forbidden, "invalidate by a worker who is not the author")
		_, err = e.invalidate(e.Owner, rule1.ID, memory.InvalidateInput{ExpectedVersion: 1})
		wantKind(t, err, kerr.Invalid, "invalidate without a reason")
		// The author may not leave V's relation live on a dead key (row-level security hides it from UPDATE).
		_, err = e.invalidate(e.W, rule1.ID, memory.InvalidateInput{ExpectedVersion: 1, Reason: "trùng"})
		wantKind(t, err, kerr.Conflict, "author invalidates a key with another agent's relation")
		if r := e.row(rule1.ID); !r.Current || len(e.relations(e.W, e.A, "RULE-1")) != 1 {
			t.Fatalf("refused invalidation changed state: %+v", r)
		}

		res, err := e.invalidate(e.Owner, rule1.ID, memory.InvalidateInput{ExpectedVersion: 1, Reason: "trùng", ReplacedByKey: "RULE-2"})
		if err != nil || res.ClosedRelations != 1 || res.Repin != nil {
			t.Fatalf("invalidate: %+v, %v", res, err)
		}
		if r := e.row(rule1.ID); r.Trust != memory.TrustHistorical || r.Current || r.EndedReason != "merged" || r.ReplacedBy != rule2.ID {
			t.Fatalf("RULE-1 = %+v", r)
		}
		if live := e.relations(e.W, e.A, "RULE-1"); len(live) != 0 {
			t.Fatalf("relations of RULE-1 still live: %+v", live)
		}
		hits, err := e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "RULE-1"})
		if err != nil || slices.Contains(hitKeys(hits), "RULE-1") {
			t.Fatalf("default search returns RULE-1: %v %v", hitKeys(hits), err)
		}
		hits, err = e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "RULE-1", Trust: memory.TrustHistorical})
		if err != nil || len(hits) != 1 || hits[0].Item.ID != rule1.ID {
			t.Fatalf("trust=HISTORICAL search: %v %v", hitKeys(hits), err)
		}
		if got := e.events(e.A); got[len(got)-1] != memory.EventInvalidated {
			t.Fatalf("journal = %v", got)
		}
	})
}

func TestPinnedKeys(t *testing.T) {
	e := newEnv(t)
	t.Run("AC-H-106 supersede or invalidate a pinned key", func(t *testing.T) {
		req1 := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: auth("Xóa nhóm phải xác nhận")})
		req7 := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-7", Kind: "requirement", Content: auth("Xóa nhóm cần xác nhận hai bước")})
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-8", Kind: "requirement", Content: cand("đề xuất chưa xác nhận")})
		// The same key in project B, pinned there, must not matter (only tasks of the same project).
		e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.B, Key: "REQ-1", Kind: "requirement", Content: auth("Yêu cầu của B")})
		running := e.task(e.A, "T-1", "RUNNING", "REQ-1")
		e.task(e.A, "T-OLD", "SUPERSEDED", "REQ-1")
		e.task(e.B, "T-B", "RUNNING", "REQ-1")

		_, err := e.supersede(e.Owner, req1.ID, memory.SupersedeInput{ExpectedVersion: 1, Content: cand("bản nháp")})
		wantKind(t, err, kerr.Conflict, "supersede a pinned key with a CANDIDATE")
		asIs := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustAuthoritative, Title: "hiện trạng"}
		_, err = e.supersede(e.Owner, req1.ID, memory.SupersedeInput{ExpectedVersion: 1, Content: asIs})
		wantKind(t, err, kerr.Conflict, "supersede a pinned key with AS_IS")
		for name, in := range map[string]memory.InvalidateInput{
			"no replaced_by_key": {ExpectedVersion: 1, Reason: "bỏ"},
			"missing key":        {ExpectedVersion: 1, Reason: "bỏ", ReplacedByKey: "REQ-404"},
			"CANDIDATE target":   {ExpectedVersion: 1, Reason: "bỏ", ReplacedByKey: "REQ-8"},
		} {
			_, err := e.invalidate(e.Owner, req1.ID, in)
			wantKind(t, err, kerr.Conflict, "invalidate a pinned key: "+name)
		}
		if r := e.row(req1.ID); !r.Current || r.Trust != memory.TrustAuthoritative || r.Version != 1 {
			t.Fatalf("REQ-1 v1 must stay current: %+v", r)
		}

		for name, in := range map[string]memory.SupersedeInput{
			"level": {ExpectedVersion: 1, Level: memory.LevelAgent, Content: auth("đổi level")},
			"kind":  {ExpectedVersion: 1, Kind: "rule", Content: auth("đổi kind")},
			"key":   {ExpectedVersion: 1, Key: "REQ-100", Content: auth("đổi key")},
		} {
			_, err := e.supersede(e.Owner, req1.ID, in)
			wantKind(t, err, kerr.Invalid, "supersede with another "+name)
		}

		res, err := e.invalidate(e.Owner, req1.ID, memory.InvalidateInput{ExpectedVersion: 1, Reason: "gộp", ReplacedByKey: "REQ-7"})
		if err != nil || res.Repin == nil || !slices.Equal(res.Repin.TaskIDs, []string{running}) || res.Repin.Key != "REQ-7" || res.Repin.Version != req7.Version {
			t.Fatalf("invalidate with a valid replacement: repin=%+v err=%v", res.Repin, err)
		}
		// The next context of T, built from the re-pinned version, contains REQ-7 and no longer REQ-1.
		in := memory.PacketInput{ProjectID: e.A, TaskID: running, TaskKey: "T-1", TaskTitle: "t", Revision: 2,
			DependencyVersions: map[string]int{res.Repin.Key: res.Repin.Version}}
		if p := e.packet(e.W, in); !strings.Contains(p.Text, "REQ-7 v1") || !strings.Contains(p.Text, "xác nhận hai bước") || strings.Contains(p.Text, "REQ-1") {
			t.Fatalf("context after re-pin:\n%s", p.Text)
		}
		// AC-H-053: an authoritative new version re-pins only the tasks of the same project.
		pinning7 := e.task(e.A, "T-7", "RUNNABLE", "REQ-7")
		next, err := e.supersede(e.Owner, req7.ID, memory.SupersedeInput{ExpectedVersion: 1, Content: auth("Xóa nhóm cần xác nhận ba bước")})
		if err != nil || next.Repin == nil || !slices.Equal(next.Repin.TaskIDs, []string{pinning7}) || next.Repin.Key != "REQ-7" || next.Repin.Version != 2 {
			t.Fatalf("supersede a pinned key: repin=%+v err=%v", next.Repin, err)
		}
		// A context still built from the old pin says so instead of showing the replaced text as official.
		if p := e.packet(e.W, in); !strings.Contains(p.Text, "REQ-7 v1: bản đã ghim không còn là bản chính thức (HISTORICAL)") || strings.Contains(p.Text, "hai bước") {
			t.Fatalf("context with a stale pin:\n%s", p.Text)
		}
	})
}

func TestConcurrentSupersede(t *testing.T) {
	e := newEnv(t)
	t.Run("AC-H-007 two supersedes of the same version", func(t *testing.T) {
		req := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: auth("bản gốc")})
		const racers = 8
		errs := make([]error, racers)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range racers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				_, errs[i] = e.supersede(e.Owner, req.ID, memory.SupersedeInput{ExpectedVersion: 1,
					Content: auth("bản mới của request " + string(rune('a'+i)))})
			}()
		}
		close(start)
		wg.Wait()
		won := 0
		for i, err := range errs {
			switch {
			case err == nil:
				won++
			case !kerr.Is(err, kerr.Conflict):
				t.Fatalf("request %d: %v, want 409", i, err)
			}
		}
		var current, versions int
		e.must(e.Owner, func(tx pgx.Tx) error {
			return tx.QueryRow(e.ctx, `SELECT count(*) FILTER (WHERE is_current), count(*) FROM knowledge
				WHERE project_id = $1 AND key = 'REQ-1'`, e.A).Scan(&current, &versions)
		})
		if won != 1 || current != 1 || versions != 2 {
			t.Fatalf("winners=%d current=%d versions=%d, want 1/1/2", won, current, versions)
		}
	})
}
