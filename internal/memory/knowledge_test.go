package memory_test

import (
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func (e *env) confirm(actor authn.Principal, id string, version int) (memory.WriteResult, error) {
	var res memory.WriteResult
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		res, err = memory.ConfirmKnowledge(e.ctx, tx, actor, id, version)
		return err
	})
	return res, err
}

func (e *env) supersede(actor authn.Principal, id string, in memory.SupersedeInput) (memory.WriteResult, error) {
	var res memory.WriteResult
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		res, err = memory.SupersedeKnowledge(e.ctx, tx, actor, id, in)
		return err
	})
	return res, err
}

func (e *env) invalidate(actor authn.Principal, id string, in memory.InvalidateInput) (memory.WriteResult, error) {
	var res memory.WriteResult
	err := e.as(actor, func(tx pgx.Tx) (err error) {
		res, err = memory.InvalidateKnowledge(e.ctx, tx, actor, id, in)
		return err
	})
	return res, err
}

func (e *env) source(actor authn.Principal, projectID, content string) memory.Source {
	e.t.Helper()
	var s memory.Source
	e.must(actor, func(tx pgx.Tx) (err error) {
		s, err = memory.CreateSource(e.ctx, tx, actor, memory.SourceInput{ProjectID: projectID, Kind: "observation", Content: content})
		return err
	})
	return s
}

func TestKnowledgeLifecycle(t *testing.T) {
	e := newEnv(t)

	t.Run("AC-H-001 worker proposes, owner confirms", func(t *testing.T) {
		res, err := e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: "REQ-1", Kind: "requirement", Content: cand("Xóa nhóm phải xác nhận")})
		if err != nil || !res.Created || res.Item.Version != 1 || res.Item.Trust != memory.TrustCandidate {
			t.Fatalf("create: %+v, %v", res, err)
		}
		done, err := e.confirm(e.Owner, res.Item.ID, 1)
		if err != nil || done.Item.Trust != memory.TrustAuthoritative || done.Item.Version != 1 {
			t.Fatalf("confirm: %+v, %v", done.Item, err)
		}
		if got := e.events(e.A); !slices.Equal(got, []string{memory.EventAdded, memory.EventConfirmed}) {
			t.Fatalf("journal = %v", got)
		}
	})

	t.Run("AC-H-002 worker confirms its own item", func(t *testing.T) {
		it := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "REQ-2", Kind: "requirement", Content: cand("tự xác nhận")})
		_, err := e.confirm(e.W, it.ID, 1)
		wantKind(t, err, kerr.Forbidden, "worker confirm")
		if r := e.row(it.ID); r.Trust != memory.TrustCandidate {
			t.Fatalf("trust = %s, want CANDIDATE", r.Trust)
		}
	})

	t.Run("AC-H-003 worker writes AUTHORITATIVE or a bad OBSERVED", func(t *testing.T) {
		_, err := e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: "REQ-3", Kind: "requirement", Content: auth("tự phong")})
		wantKind(t, err, kerr.Forbidden, "worker AUTHORITATIVE")

		noSource := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustObserved, Title: "quan sát không nguồn"}
		_, err = e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: "OBS-1", Kind: "ui", Content: noSource})
		wantKind(t, err, kerr.Invalid, "OBSERVED without source")

		src := e.source(e.W, e.A, "ảnh chụp màn hình editor")
		toBe := memory.Content{Aspect: memory.AspectToBe, Trust: memory.TrustObserved, Title: "quan sát sai aspect", SourceIDs: []string{src.ID}}
		_, err = e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: "OBS-2", Kind: "ui", Content: toBe})
		wantKind(t, err, kerr.Invalid, "OBSERVED with TO_BE")

		foreign := e.source(e.X, e.B, "nguồn của project khác")
		other := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustObserved, Title: "nguồn ngoài project", SourceIDs: []string{foreign.ID}}
		_, err = e.create(e.W, memory.CreateInput{ProjectID: e.A, Key: "OBS-3", Kind: "ui", Content: other})
		wantKind(t, err, kerr.Invalid, "OBSERVED with a source of another project")

		ok := memory.Content{Aspect: memory.AspectAsIs, Trust: memory.TrustObserved, Title: "quan sát hợp lệ", SourceIDs: []string{src.ID}}
		it := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "OBS-4", Kind: "ui", Content: ok})
		if it.Trust != memory.TrustObserved || len(it.SourceIDs) != 1 {
			t.Fatalf("observed item = %+v", it)
		}
		if src.Origin != "AGENT" || e.source(e.Owner, e.A, "lời chủ sản phẩm").Origin != "OWNER" {
			t.Fatalf("PM-H-04: origin must come from the actor")
		}
	})

	t.Run("AC-H-100 owner writes an official requirement directly", func(t *testing.T) {
		it := e.mustCreate(e.Owner, memory.CreateInput{ProjectID: e.A, Key: "REQ-9", Kind: "requirement", Content: auth("Nút thêm nằm trong Part")})
		if it.Version != 1 || it.Trust != memory.TrustAuthoritative || it.NS != "" {
			t.Fatalf("item = %+v", it)
		}
	})

	t.Run("AC-H-006 confirm with a wrong version or twice", func(t *testing.T) {
		it := e.mustCreate(e.W, memory.CreateInput{ProjectID: e.A, Key: "REQ-6", Kind: "requirement", Content: cand("xác nhận sai phiên bản")})
		_, err := e.confirm(e.Owner, it.ID, 2)
		wantKind(t, err, kerr.Conflict, "confirm expected_version 2")
		if _, err := e.confirm(e.Owner, it.ID, 1); err != nil {
			t.Fatalf("confirm v1: %v", err)
		}
		before := len(e.events(e.A))
		// A replay with the same Idempotency-Key is answered by the HTTP layer; a
		// second real confirm is a wrong state and adds no event.
		_, err = e.confirm(e.Owner, it.ID, 1)
		wantKind(t, err, kerr.Conflict, "confirm an AUTHORITATIVE item")
		if after := len(e.events(e.A)); after != before {
			t.Fatalf("journal grew from %d to %d", before, after)
		}
	})
}
