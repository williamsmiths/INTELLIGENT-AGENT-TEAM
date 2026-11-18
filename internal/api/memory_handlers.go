package api

import (
	"context"
	"fmt"
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// The project of a request always comes from the path, after the membership
// check done by the services; a project_id in the body is overwritten (AC-H-083).

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var in memory.SourceInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	in.ProjectID = r.PathValue("p")
	var out memory.Source
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.CreateSource(r.Context(), tx, principalOf(r), in)
		return err
	})
	s.reply(w, r, http.StatusCreated, out, err)
}

func (s *Server) getSource(w http.ResponseWriter, r *http.Request) {
	var out memory.Source
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.GetSource(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// createKnowledge serves POST /v1/projects/{p}/knowledge and POST /v1/global/knowledge.
func (s *Server) createKnowledge(w http.ResponseWriter, r *http.Request) {
	var in memory.CreateInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if p := r.PathValue("p"); p != "" {
		in.ProjectID = p
		if in.Level == "" || in.Level == "GLOBAL" {
			in.Level = "PROJECT"
		}
	} else {
		in.ProjectID, in.Level = "", "GLOBAL"
	}
	s.writeKnowledge(w, r, http.StatusCreated, func(tx pgx.Tx) (memory.WriteResult, error) {
		return memory.CreateKnowledge(r.Context(), tx, principalOf(r), in)
	})
}

func (s *Server) confirmKnowledge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedVersion int `json:"expected_version"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	s.writeKnowledge(w, r, http.StatusOK, func(tx pgx.Tx) (memory.WriteResult, error) {
		return memory.ConfirmKnowledge(r.Context(), tx, principalOf(r), r.PathValue("id"), in.ExpectedVersion)
	})
}

func (s *Server) supersedeKnowledge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedVersion int `json:"expected_version"`
		memory.SupersedeInput
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	in.SupersedeInput.ExpectedVersion = in.ExpectedVersion
	s.writeKnowledge(w, r, http.StatusCreated, func(tx pgx.Tx) (memory.WriteResult, error) {
		return memory.SupersedeKnowledge(r.Context(), tx, principalOf(r), r.PathValue("id"), in.SupersedeInput)
	})
}

func (s *Server) invalidateKnowledge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ExpectedVersion int    `json:"expected_version"`
		Reason          string `json:"reason"`
		ReplacedByKey   string `json:"replaced_by_key"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	s.writeKnowledge(w, r, http.StatusOK, func(tx pgx.Tx) (memory.WriteResult, error) {
		return memory.InvalidateKnowledge(r.Context(), tx, principalOf(r), r.PathValue("id"),
			memory.InvalidateInput{ExpectedVersion: in.ExpectedVersion, Reason: in.Reason, ReplacedByKey: in.ReplacedByKey})
	})
}

// writeKnowledge runs one knowledge write and its consequences in one
// transaction: tasks that pinned the changed key are re-pinned and their
// leases replaced (T-H-32, T-H-34 → T-H-22, T-H-04). Only those tasks change (AC-H-053).
func (s *Server) writeKnowledge(w http.ResponseWriter, r *http.Request, created int, do func(pgx.Tx) (memory.WriteResult, error)) {
	var res memory.WriteResult
	var changes []work.Change
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		if res, err = do(tx); err != nil {
			return err
		}
		// Only the owner can change a pinned key (AC-H-106), and the owner scope is trusted.
		if res.Repin != nil && len(res.Repin.TaskIDs) > 0 {
			notice := fmt.Sprintf("Yêu cầu %s đã đổi; task nay ghim %s v%d. Đọc lại context trước khi sửa tiếp.",
				res.Item.Key, res.Repin.Key, res.Repin.Version)
			changes, err = work.OnDependencyChanged(r.Context(), tx, res.Repin.TaskIDs,
				res.Item.Key, res.Repin.Key, res.Repin.Version, notice)
		}
		return err
	})
	if err == nil {
		s.appendDeferred(r.Context(), res.Deferred)
		s.notifyChanges(r.Context(), "yêu cầu phụ thuộc đã đổi", changes...)
	}
	status := created
	if !res.Created && created == http.StatusCreated {
		status = http.StatusOK // selective remembering returned the existing row (AC-H-101)
	}
	s.reply(w, r, status, res.Item, err)
}

// appendDeferred journals events that have no project (GLOBAL knowledge): only
// the runtime scope may write those rows.
func (s *Server) appendDeferred(ctx context.Context, events []journal.Event) {
	if len(events) == 0 {
		return
	}
	_ = store.InScope(ctx, s.Pool, store.Runtime, func(tx pgx.Tx) error {
		for _, e := range events {
			if _, err := journal.Append(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Server) searchKnowledge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	var hits []memory.Hit
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		if p := r.PathValue("p"); p != "" {
			hits, err = memory.Search(r.Context(), tx, principalOf(r), memory.SearchQuery{ProjectID: p, Q: q.Get("q"),
				Kind: q.Get("kind"), Trust: q.Get("trust"), Aspect: q.Get("aspect"), Level: q.Get("level"), Limit: limit})
			return err
		}
		hits, err = memory.ListGlobal(r.Context(), tx, principalOf(r), memory.GlobalQuery{Q: q.Get("q"), Trust: q.Get("trust"), Limit: limit})
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"hits": orEmpty(hits)}, err)
}
