package memory_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func TestSearch(t *testing.T) {
	e := newEnv(t)
	body := auth("Quy tắc xóa")
	body.Body = "Xóa nhóm câu hỏi phải xác nhận"
	req := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: body})
	other := auth("Nhắc tới REQ-1 trong nội dung")
	other.Body = "Màn hình báo cáo tham chiếu REQ-1 nhưng là mục khác"
	e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "UI-7", Kind: "ui", Content: other})
	old := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "NOTE-1", Kind: "decision", Content: cand("Quy ước đặt tên nhóm câu hỏi cũ")})
	if _, err := e.supersede(e.W, old.ID, memory.SupersedeInput{ExpectedVersion: 1, Content: cand("Quy ước đặt tên mới cho bộ đề")}); err != nil {
		t.Fatal(err)
	}

	t.Run("AC-H-008 Vietnamese without diacritics, key first, no HISTORICAL by default", func(t *testing.T) {
		hits, err := e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "xoa nhom cau hoi"})
		if err != nil || len(hits) == 0 || hits[0].Item.ID != req.ID || hits[0].Stage != memory.StageFTS {
			t.Fatalf("unaccented search: %v %v", hitKeys(hits), err)
		}
		hits, err = e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "REQ-1"})
		if err != nil || len(hits) < 2 || hits[0].Item.Key != "REQ-1" || hits[0].Stage != memory.StageKey || hits[1].Item.Key != "UI-7" {
			t.Fatalf("key search: %v %v", hitKeys(hits), err)
		}
		// A misspelled query has no full-text match; the trigram stage still finds the item.
		hits, err = e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "nhom cau hoii"})
		if err != nil || len(hits) == 0 || hits[0].Item.ID != req.ID || hits[0].Stage != memory.StageTrigram {
			t.Fatalf("trigram search: %+v %v", hits, err)
		}

		for _, q := range []string{"", "NOTE-1", "quy uoc dat ten", "nhom cau hoi cu"} {
			hits, err := e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: q})
			if err != nil || slices.ContainsFunc(hits, func(h memory.Hit) bool { return h.Item.Trust == memory.TrustHistorical }) {
				t.Fatalf("default search q=%q returns HISTORICAL rows: %v", q, err)
			}
		}
		hits, err = e.search(e.W, memory.SearchQuery{ProjectID: e.A, Q: "nhom cau hoi cu", Trust: memory.TrustHistorical})
		if err != nil || len(hits) != 1 || hits[0].Item.ID != old.ID {
			t.Fatalf("trust=HISTORICAL: %v %v", hitKeys(hits), err)
		}
	})

	t.Run("filters", func(t *testing.T) {
		for name, tc := range map[string]struct {
			q    memory.SearchQuery
			want []string
		}{
			"kind":   {memory.SearchQuery{Kind: "ui"}, []string{"UI-7"}},
			"trust":  {memory.SearchQuery{Trust: memory.TrustCandidate}, []string{"NOTE-1"}},
			"aspect": {memory.SearchQuery{Aspect: memory.AspectAsIs}, []string{}},
			"level":  {memory.SearchQuery{Level: memory.LevelAgent}, []string{}},
			"all":    {memory.SearchQuery{}, []string{"NOTE-1", "REQ-1", "UI-7"}},
		} {
			tc.q.ProjectID = e.A
			hits, err := e.search(e.W, tc.q)
			if err != nil || !slices.Equal(hitKeys(hits), tc.want) {
				t.Errorf("%s filter: %v %v, want %v", name, hitKeys(hits), err, tc.want)
			}
		}
		_, err := e.search(e.W, memory.SearchQuery{ProjectID: e.A, Kind: "poem"})
		wantKind(t, err, kerr.Invalid, "unknown kind filter")
	})
}

func TestBlackboard(t *testing.T) {
	e := newEnv(t)
	t.Run("PM-H-10 append and read by cursor", func(t *testing.T) {
		taskID := e.task(e.A, "T-1", "RUNNING")
		foreign := e.task(e.B, "T-B", "RUNNING")
		post := func(actor authn.Principal, in memory.BlackboardInput) (memory.BlackboardEntry, error) {
			var out memory.BlackboardEntry
			err := e.as(actor, func(tx pgx.Tx) (err error) {
				out, err = memory.PostBlackboard(e.ctx, tx, actor, in)
				return err
			})
			return out, err
		}
		list := func(actor authn.Principal, projectID, task string, after int64) ([]memory.BlackboardEntry, error) {
			var out []memory.BlackboardEntry
			err := e.as(actor, func(tx pgx.Tx) (err error) {
				out, err = memory.ListBlackboard(e.ctx, tx, actor, projectID, task, after, 0)
				return err
			})
			return out, err
		}
		first, err := post(e.W, memory.BlackboardInput{ProjectID: e.A, TaskID: taskID, Kind: "finding", Body: "parser bỏ qua dòng trống", Refs: []string{"file:src/parser.go"}})
		if err != nil || first.Author != e.W.ID || len(first.Refs) != 1 {
			t.Fatalf("post: %+v %v", first, err)
		}
		if _, err := post(e.V, memory.BlackboardInput{ProjectID: e.A, Kind: "decision", Body: "dùng bảng tạm"}); err != nil {
			t.Fatal(err)
		}
		_, err = post(e.W, memory.BlackboardInput{ProjectID: e.A, Kind: "rumor", Body: "x"})
		wantKind(t, err, kerr.Invalid, "unknown kind")
		_, err = post(e.W, memory.BlackboardInput{ProjectID: e.A, TaskID: foreign, Kind: "finding", Body: "x"})
		wantKind(t, err, kerr.Invalid, "task of another project")
		_, err = post(e.X, memory.BlackboardInput{ProjectID: e.A, Kind: "finding", Body: "x"})
		wantKind(t, err, kerr.NotFound, "post by a non-member")

		all, err := list(e.V, e.A, "", 0)
		if err != nil || len(all) != 2 || all[0].ID != first.ID {
			t.Fatalf("list all: %+v %v", all, err)
		}
		ofTask, _ := list(e.V, e.A, taskID, 0)
		delta, _ := list(e.V, e.A, "", first.ID)
		if len(ofTask) != 1 || len(delta) != 1 || delta[0].Kind != "decision" {
			t.Fatalf("task filter %d, delta %+v", len(ofTask), delta)
		}
		_, err = list(e.X, e.A, "", 0)
		wantKind(t, err, kerr.NotFound, "read by a non-member")
	})
}
