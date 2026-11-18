package api

import (
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// intelAPI registers the routes of spec S2 C.9 on the owner socket.
// PM-S2-15: the views are open to the Owner (all), a Lead (its projects) and
// the lease holder (its task) through work.GetTask; answering and resuming
// are Owner only (PM-S2-12, PM-S2-13).
func (s *Server) intelAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/tasks/{id}/questions", s.taskQuestions)
	api.HandleFunc("GET /v1/tasks/{id}/understanding", s.taskUnderstanding)
	api.HandleFunc("GET /v1/tasks/{id}/cost", s.taskCost)
	api.HandleFunc("GET /v1/questions", s.listQuestions)
	api.Handle("POST /v1/questions/{id}/answer", ownerOnly(http.HandlerFunc(s.answerQuestion)))
	api.Handle("POST /v1/tasks/{id}/resume", ownerOnly(http.HandlerFunc(s.resumeTask)))
}

// viewable runs fn after the caller passed the task view rule (404 outside its scope).
func (s *Server) viewable(r *http.Request, fn func(tx pgx.Tx, tv work.TaskView) error) error {
	return s.scoped(r, func(tx pgx.Tx) error {
		tv, err := work.GetTask(r.Context(), tx, principalOf(r), r.PathValue("id"))
		if err != nil {
			return err
		}
		return fn(tx, tv)
	})
}

func (s *Server) taskQuestions(w http.ResponseWriter, r *http.Request) {
	var out []intel.QuestionRow
	err := s.viewable(r, func(tx pgx.Tx, tv work.TaskView) (err error) {
		out, err = intel.ListQuestions(r.Context(), tx, tv.Task.ID, nil, r.URL.Query().Get("status"))
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"questions": orEmpty(out)}, err)
}

func (s *Server) taskUnderstanding(w http.ResponseWriter, r *http.Request) {
	var out []intel.UnderstandingRow
	err := s.viewable(r, func(tx pgx.Tx, tv work.TaskView) (err error) {
		out, err = intel.ListUnderstandings(r.Context(), tx, tv.Task.ID)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"understanding": orEmpty(out)}, err)
}

func (s *Server) taskCost(w http.ResponseWriter, r *http.Request) {
	var out intel.CostReport
	err := s.viewable(r, func(tx pgx.Tx, tv work.TaskView) (err error) {
		out, err = intel.Cost(r.Context(), tx, tv.Task.ID)
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// listQuestions serves GET /v1/questions?status=OPEN: the Owner sees every
// project, a Lead its projects, a Worker the task it holds (PM-S2-15).
func (s *Server) listQuestions(w http.ResponseWriter, r *http.Request) {
	actor, status := principalOf(r), strings.ToUpper(r.URL.Query().Get("status"))
	if status != "" && status != "OPEN" && status != "ANSWERED" {
		writeErr(w, r, kerr.New(kerr.Invalid, "status must be OPEN or ANSWERED"))
		return
	}
	var out []intel.QuestionRow
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		if actor.IsOwner() {
			out, err = intel.ListQuestions(r.Context(), tx, "", nil, status)
			return err
		}
		lead := []string{}
		for p, role := range actor.Roles {
			if role == "lead" {
				lead = append(lead, p)
			}
		}
		if out, err = intel.ListQuestions(r.Context(), tx, "", lead, status); err != nil {
			return err
		}
		held, err := work.Me(r.Context(), tx, actor)
		if err != nil || held.Lease == nil || actor.RoleIn(held.Lease.ProjectID) == "lead" {
			return err
		}
		mine, err := intel.ListQuestions(r.Context(), tx, held.Lease.TaskID, nil, status)
		out = append(out, mine...)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"questions": orEmpty(out)}, err)
}

// resumeTask serves POST /v1/tasks/{id}/resume {note} (T-S2-03): the reason
// nonprogress leaves the set and the counter starts again.
func (s *Server) resumeTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Note string `json:"note"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var state string
	err := s.scoped(r, func(tx pgx.Tx) error {
		tv, err := work.GetTask(r.Context(), tx, principalOf(r), r.PathValue("id"))
		if err != nil {
			return err
		}
		if !hasReason(tv.Task, work.ReasonNonprogress) {
			return kerr.New(kerr.Conflict, "task is not waiting for nonprogress")
		}
		var had bool
		if state, had, err = work.RemoveWaitingReason(r.Context(), tx, tv.Task.ID, work.ReasonNonprogress); err != nil || !had {
			return err
		}
		if _, err := journal.Append(r.Context(), tx, journal.Event{ProjectID: tv.Task.ProjectID, TaskID: tv.Task.ID,
			Type: "NONPROGRESS_RESUMED", Actor: principalOf(r).ID, Payload: map[string]any{"state": state, "note": in.Note != ""}}); err != nil {
			return err
		}
		if tv.Lease != nil && strings.TrimSpace(in.Note) != "" {
			return work.AppendNotice(r.Context(), tx, tv.Lease.ID, "Chủ sản phẩm cho tiếp tục sau khi dừng vì không tiến triển: "+in.Note)
		}
		return nil
	})
	s.reply(w, r, http.StatusOK, map[string]any{"task_state": state}, err)
}
