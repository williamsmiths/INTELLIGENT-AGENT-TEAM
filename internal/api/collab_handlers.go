package api

import (
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

func (s *Server) createRelation(w http.ResponseWriter, r *http.Request) {
	var in memory.RelationInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	in.ProjectID = r.PathValue("p")
	var out memory.Relation
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.CreateRelation(r.Context(), tx, principalOf(r), in)
		return err
	})
	s.reply(w, r, http.StatusCreated, out, err)
}

func (s *Server) closeRelation(w http.ResponseWriter, r *http.Request) {
	var out memory.Relation
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.CloseRelation(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) listRelations(w http.ResponseWriter, r *http.Request) {
	var out []memory.Relation
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.ListRelations(r.Context(), tx, principalOf(r), r.PathValue("p"), r.URL.Query().Get("key"))
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"relations": orEmpty(out)}, err)
}

func (s *Server) postBlackboard(w http.ResponseWriter, r *http.Request) {
	var in memory.BlackboardInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	in.ProjectID = r.PathValue("p")
	var out memory.BlackboardEntry
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.PostBlackboard(r.Context(), tx, principalOf(r), in)
		return err
	})
	s.reply(w, r, http.StatusCreated, out, err)
}

func (s *Server) listBlackboard(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	var out []memory.BlackboardEntry
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = memory.ListBlackboard(r.Context(), tx, principalOf(r), r.PathValue("p"), q.Get("task"), after, limit)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"entries": orEmpty(out)}, err)
}

// orEmpty makes a nil slice encode as [] instead of null.
func orEmpty[T any](v []T) []T {
	if v == nil {
		return []T{}
	}
	return v
}
