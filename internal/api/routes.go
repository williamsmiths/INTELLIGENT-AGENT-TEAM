package api

import (
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AgentRoutes is everything the agent socket serves (spec S2 A.4, PM-S2-02):
// whoami, the Claude hooks and the agent verbs. No Owner route exists here, so
// no identity on this socket can reach one (AC-S2-003).
func (s *Server) AgentRoutes(auth Authenticator) http.Handler {
	return s.router(auth, s.agentAPI)
}

// OwnerRoutes is everything the owner socket serves: whoami and every S1
// route except the hooks, plus the Owner routes of S2 (PM-S2-01, PM-S2-04).
func (s *Server) OwnerRoutes(auth Authenticator) http.Handler {
	return s.router(auth, s.ownerAPI, s.isolationAPI)
}

// router puts the route sets behind one authenticator.
func (s *Server) router(auth Authenticator, register ...func(*http.ServeMux)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/whoami", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, auth.Whoami(r))
	})
	api := http.NewServeMux()
	for _, reg := range register {
		reg(api)
	}
	mux.Handle("/v1/", authenticate(auth)(s.leaseLock(idempotent(s.Pool)(api))))
	return mux
}

// agentAPI registers the routes of PM-S2-02.
func (s *Server) agentAPI(api *http.ServeMux) {
	api.HandleFunc("POST /v1/hooks/claude/{event}", s.hook)
	api.HandleFunc("POST /v1/agent/{verb}", s.withInbox(s.agentVerb)) // spec S2 C.3: consult outcomes reach the asker in any verb
}

// isolationAPI registers the Owner routes of spec S2 A.4 (PM-S2-04).
func (s *Server) isolationAPI(api *http.ServeMux) {
	api.Handle("GET /v1/principals/{id}", ownerOnly(http.HandlerFunc(s.getPrincipal)))
	api.Handle("PUT /v1/principals/{id}", ownerOnly(http.HandlerFunc(s.updatePrincipal)))
	api.Handle("PUT /v1/projects/{p}/reference-roots", ownerOnly(http.HandlerFunc(s.setReferenceRoots)))
	// Spec S2 part E (PM-S2-21, PM-S2-24): Leads read the policy with `kch delegate-policy` on the agent socket.
	api.Handle("PUT /v1/projects/{p}/delegate-policy", ownerOnly(http.HandlerFunc(s.setDelegatePolicy)))
	api.Handle("GET /v1/projects/{p}/delegate-policy", ownerOnly(http.HandlerFunc(s.getDelegatePolicy)))
	// Spec S2 E.9 (PM-S2-23): conversation → project binding.
	api.Handle("PUT /v1/projects/{p}/conversations", ownerOnly(http.HandlerFunc(s.bindConversation)))
	api.Handle("GET /v1/projects/{p}/conversations", ownerOnly(http.HandlerFunc(s.listConversations)))
	api.Handle("DELETE /v1/projects/{p}/conversations/{kind}/{id...}", ownerOnly(http.HandlerFunc(s.unbindConversation)))
	s.intelAPI(api) // spec S2 C.9 (intel_handlers.go)
}

// getPrincipal returns a principal with its specialty and the roots its
// sandbox is generated from; `kch enroll` reads it.
func (s *Server) getPrincipal(w http.ResponseWriter, r *http.Request) {
	var out work.PrincipalView
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.GetPrincipal(r.Context(), tx, principalOf(r), r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// updatePrincipal serves PUT /v1/principals/{id}: {"specialty": "product"|...|""}.
func (s *Server) updatePrincipal(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Specialty string `json:"specialty"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.PrincipalView
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.SetSpecialty(r.Context(), tx, principalOf(r), r.PathValue("id"), in.Specialty)
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// setReferenceRoots serves PUT /v1/projects/{p}/reference-roots: {"paths": [...]}.
func (s *Server) setReferenceRoots(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var roots []string
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		roots, err = work.SetReferenceRoots(r.Context(), tx, principalOf(r), r.PathValue("p"), in.Paths)
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"reference_roots": orEmpty(roots)}, err)
}
