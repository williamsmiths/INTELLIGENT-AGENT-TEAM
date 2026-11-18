package api

import (
	"net/http"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var in struct{ Slug, Name string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Project
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.CreateProject(r.Context(), tx, principalOf(r), in.Slug, in.Name)
		return err
	})
	s.reply(w, r, http.StatusCreated, out, err)
}

func (s *Server) setProjectLimits(w http.ResponseWriter, r *http.Request) {
	var in work.ProjectLimits
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Project
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.SetProjectLimits(r.Context(), tx, principalOf(r), r.PathValue("p"), in)
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) registerRepo(w http.ResponseWriter, r *http.Request) {
	var in struct{ Path string }
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var root string
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		root, err = work.RegisterRepo(r.Context(), tx, principalOf(r), r.PathValue("p"), in.Path)
		return err
	})
	s.reply(w, r, http.StatusCreated, map[string]string{"root": root}, err)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	var out work.Project
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.GetProject(r.Context(), tx, principalOf(r), r.PathValue("p"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// createPrincipal returns the token once; only its hash is stored (PM-H-02).
func (s *Server) createPrincipal(w http.ResponseWriter, r *http.Request) {
	var in work.NewPrincipal
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var id work.Identity
	var token string
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		id, token, err = work.CreatePrincipal(r.Context(), tx, principalOf(r), in)
		return err
	})
	s.reply(w, r, http.StatusCreated, map[string]any{"principal": id, "token": token}, err)
}

func (s *Server) addMembership(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ProjectID string `json:"project_id"`
		Role      string `json:"role"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Identity
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.AddMembership(r.Context(), tx, principalOf(r), r.PathValue("id"), in.ProjectID, in.Role)
		return err
	})
	s.reply(w, r, http.StatusCreated, out, err)
}

// me is where the CLI learns the project and task of the caller: from the
// lease, never from an argument the model typed (spec §0.1).
func (s *Server) me(w http.ResponseWriter, r *http.Request) {
	var out work.Identity
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.Me(r.Context(), tx, principalOf(r))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) setSystemBudget(w http.ResponseWriter, r *http.Request) {
	var in work.Limits
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.Budget
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.SetSystemBudget(r.Context(), tx, principalOf(r), in)
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) setTaskBudget(w http.ResponseWriter, r *http.Request) {
	var in work.Limits
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.BudgetView
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.SetTaskBudget(r.Context(), tx, principalOf(r), r.PathValue("id"), in)
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) getBudget(w http.ResponseWriter, r *http.Request) {
	var out work.BudgetView
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.GetBudget(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	after, limit := cursor(r)
	var out []work.Event
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.ListEvents(r.Context(), tx, principalOf(r), r.PathValue("p"), after, limit)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"events": orEmpty(out)}, err)
}

func (s *Server) listSystemEvents(w http.ResponseWriter, r *http.Request) {
	after, limit := cursor(r)
	var out []work.Event
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.ListSystemEvents(r.Context(), tx, principalOf(r), after, limit)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"events": orEmpty(out)}, err)
}

// registerHookInstall records the hash of the hook file the owner just
// installed for an agent, so a later change is detected (AC-H-070, L2).
func (s *Server) registerHookInstall(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SettingsPath string `json:"settings_path"`
		SHA256       string `json:"sha256"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	if in.SettingsPath == "" || len(in.SHA256) != 64 {
		writeErr(w, r, kerr.New(kerr.Invalid, "cần settings_path và sha256"))
		return
	}
	err := s.runtime(r.Context(), func(tx pgx.Tx) error {
		if _, err := work.LookupPrincipal(r.Context(), tx, r.PathValue("id")); err != nil {
			return err
		}
		return hookinstall.Register(r.Context(), tx, r.PathValue("id"), in.SettingsPath, in.SHA256)
	})
	s.reply(w, r, http.StatusOK, map[string]string{"registered": r.PathValue("id")}, err)
}

func cursor(r *http.Request) (after int64, limit int) {
	after, _ = strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ = strconv.Atoi(r.URL.Query().Get("limit"))
	return after, limit
}
