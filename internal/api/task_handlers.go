package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func (s *Server) createTask(w http.ResponseWriter, r *http.Request) {
	var in work.NewTask
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Task
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.CreateTask(r.Context(), tx, principalOf(r), r.PathValue("p"), in)
		return err
	})
	s.reply(w, r, http.StatusCreated, out, err)
}

func (s *Server) getTask(w http.ResponseWriter, r *http.Request) {
	var out work.TaskView
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.GetTask(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) readyTask(w http.ResponseWriter, r *http.Request) {
	var out work.Task
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.ReadyTask(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// assignTask grants the lease (T-H-02, T-H-20). work.AssignTask authorizes the
// actor itself and needs the runtime scope, because a Lead cannot read the
// assignee's membership row.
func (s *Server) assignTask(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Principal     string    `json:"principal"`
		Mode          string    `json:"mode"`
		Notify        *bool     `json:"notify"`         // false: the caller delivers the message itself (evaluation driver)
		IntelMode     string    `json:"intel_mode"`     // spec S2 C.1: the owner forces DIRECT, ASSISTED or DEEP
		IntelFeatures *[]string `json:"intel_features"` // spec S2 C.1: mechanisms switched on (default: all in DEEP, all but the gate otherwise)
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Change
	err := s.runtime(r.Context(), func(tx pgx.Tx) (err error) {
		opt := work.IntelOptions{Mode: in.IntelMode}
		if in.IntelFeatures != nil {
			opt.Features = append([]string{}, *in.IntelFeatures...)
		}
		out, err = work.AssignTaskWith(r.Context(), tx, s.bases(), principalOf(r), r.PathValue("id"), in.Principal, in.Mode, opt)
		return err
	})
	// An OBSERVE lease is invisible to the agent by definition (configuration A).
	if err == nil && wantNotify(in.Notify) && out.Granted != nil && out.Granted.Mode != "OBSERVE" {
		s.notifyChanges(r.Context(), "bạn được giao task này", out)
	}
	s.reply(w, r, http.StatusOK, out, err)
}

// releaseTask ends the caller's own lease (T-H-21). The task state change is
// a runtime write, so the service runs under the runtime scope after checking
// that the caller is the holder.
func (s *Server) releaseTask(w http.ResponseWriter, r *http.Request) {
	var out work.Change
	err := s.runtime(r.Context(), func(tx pgx.Tx) (err error) {
		out, err = work.ReleaseLease(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) supersedeTask(w http.ResponseWriter, r *http.Request) {
	var out work.Change
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.SupersedeTask(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) revokeLease(w http.ResponseWriter, r *http.Request) {
	var out work.Change
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.RevokeLease(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// feedback stores the owner's words verbatim, bumps the revision and replaces
// the lease of this task only (PM-H-14, T-H-22, AC-H-050, AC-H-052).
func (s *Server) feedback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text   string `json:"text"`
		Notify *bool  `json:"notify"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Change
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		if out, err = work.Feedback(r.Context(), tx, principalOf(r), r.PathValue("id"), in.Text); err != nil {
			return err
		}
		return s.feedbackImpact(r.Context(), tx, principalOf(r), &out, in.Text) // spec S2 C.5
	})
	if err == nil && wantNotify(in.Notify) {
		s.notifyChanges(r.Context(), "chủ sản phẩm vừa gửi feedback", out)
	}
	s.reply(w, r, http.StatusCreated, out, err)
}

// wantNotify: delivery through the host app is on unless the request turns it off.
func wantNotify(v *bool) bool { return v == nil || *v }
