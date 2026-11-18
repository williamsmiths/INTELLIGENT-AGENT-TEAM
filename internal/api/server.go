// Package api is the HTTP surface of harnessd (spec §3). It authenticates the
// caller, opens the database scope that matches the caller, calls the
// services, and maps their errors to status codes. It holds no business rules
// of its own except the orchestration between services.
package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// Notifier delivers a message to an agent through the host app (OpenBot owns
// the agent loop and its mailbox). A nil Notifier disables delivery. Send
// returns the host's delivery id; Delivery reads its status and the host turn
// that handled it, which is how a consult's cost is attributed (spec S2 C.7).
type Notifier interface {
	Send(ctx context.Context, agentID, text, clientMessageID string) (deliveryID string, err error)
	Delivery(ctx context.Context, agentID, deliveryID string) (status, turnID string, err error)
	ClearContext(ctx context.Context, agentID string) error
}

// Server carries the dependencies of the handlers.
type Server struct {
	Pool        *pgxpool.Pool
	Runner      *verify.Runner
	Notifier    Notifier
	LogDir      string   // evidence logs, outside every repo
	TempRoots   []string // extra roots every HOOK agent may read and write; empty in production (tests use a scratch dir)
	AgentsRoot  string   // "<AgentsRoot>/<openbot agent id>" is the agent's own workspace
	PacketChars int      // character budget of a context packet

	ReadRoots    []string // machine sandbox_read_roots: toolchain directories every enrolled agent may read (AC-S2-007)
	PilotTmpRoot string   // "<PilotTmpRoot>/<openbot agent id>" is the private temp root of an agent

	Bases     verify.Store     // trusted base store (S2 D.1); zero Dir = "bases" next to LogDir (see bases)
	Isolation verify.Isolation // extra sandbox settings of the checks (AC-S2-010; see verifyRequest)

	Conversations ConversationFinder // spec S2 E.9: the host conversation of a session; nil = no binding ever
}

// ConversationFinder tells which OpenBot conversation a Claude session of an
// agent belongs to: kind "channel" or "thread" and its id, or "" when none
// (spec S2 E.9-3; openbot.Conversations reads the host database read-only).
type ConversationFinder interface {
	Find(ctx context.Context, session, openBotAgentID string) (kind, id string, err error)
}

// ownerAPI registers the routes of spec §3 that the owner socket serves
// (S2 A.4). The hooks and the agent verbs live on the agent socket only
// (routes.go: AgentRoutes, OwnerRoutes).
func (s *Server) ownerAPI(api *http.ServeMux) {
	owner := func(h http.HandlerFunc) http.Handler { return ownerOnly(h) }
	// Identity and limits (owner only).
	api.Handle("POST /v1/projects", owner(s.createProject))
	api.Handle("PUT /v1/projects/{p}/limits", owner(s.setProjectLimits))
	api.Handle("POST /v1/projects/{p}/repos", owner(s.registerRepo))
	api.Handle("PUT /v1/projects/{p}/repos/ignore", owner(s.setRepoIgnore))
	api.Handle("POST /v1/principals", owner(s.createPrincipal))
	api.Handle("POST /v1/principals/{id}/memberships", owner(s.addMembership))
	api.Handle("POST /v1/principals/{id}/hook-install", owner(s.registerHookInstall))
	api.Handle("PUT /v1/system/budget", owner(s.setSystemBudget))
	api.Handle("GET /v1/system/events", owner(s.listSystemEvents))
	api.HandleFunc("GET /v1/projects/{p}", s.getProject)
	api.HandleFunc("GET /v1/me", s.me)
	// Memory & Context.
	api.HandleFunc("POST /v1/projects/{p}/sources", s.createSource)
	api.HandleFunc("GET /v1/sources/{id}", s.getSource)
	api.HandleFunc("POST /v1/projects/{p}/knowledge", s.createKnowledge)
	api.HandleFunc("GET /v1/projects/{p}/knowledge", s.searchKnowledge)
	api.HandleFunc("POST /v1/global/knowledge", s.createKnowledge)
	api.HandleFunc("GET /v1/global/knowledge", s.searchKnowledge)
	api.Handle("POST /v1/knowledge/{id}/confirm", owner(s.confirmKnowledge))
	api.HandleFunc("POST /v1/knowledge/{id}/supersede", s.supersedeKnowledge)
	api.HandleFunc("POST /v1/knowledge/{id}/invalidate", s.invalidateKnowledge)
	api.HandleFunc("POST /v1/projects/{p}/relations", s.createRelation)
	api.HandleFunc("GET /v1/projects/{p}/relations", s.listRelations)
	api.HandleFunc("POST /v1/relations/{id}/close", s.closeRelation)
	api.HandleFunc("POST /v1/projects/{p}/blackboard", s.postBlackboard)
	api.HandleFunc("GET /v1/projects/{p}/blackboard", s.listBlackboard)
	// Tasks, leases, budgets.
	api.HandleFunc("POST /v1/projects/{p}/tasks", s.createTask)
	api.HandleFunc("GET /v1/tasks/{id}", s.getTask)
	api.HandleFunc("POST /v1/tasks/{id}/ready", s.readyTask)
	api.HandleFunc("POST /v1/tasks/{id}/assign", s.assignTask)
	api.HandleFunc("POST /v1/tasks/{id}/release", s.releaseTask)
	api.HandleFunc("POST /v1/tasks/{id}/supersede", s.supersedeTask)
	api.Handle("POST /v1/tasks/{id}/revoke", owner(s.revokeLease))
	api.Handle("POST /v1/tasks/{id}/feedback", owner(s.feedback))
	api.Handle("PUT /v1/tasks/{id}/budget", owner(s.setTaskBudget))
	api.HandleFunc("GET /v1/tasks/{id}/budget", s.getBudget)
	// Context, verification, journal, hooks.
	api.HandleFunc("GET /v1/tasks/{id}/context", s.getContext)
	api.HandleFunc("POST /v1/tasks/{id}/verify", s.verifyTask)
	api.HandleFunc("GET /v1/tasks/{id}/evidence", s.listEvidence)
	api.HandleFunc("GET /v1/projects/{p}/events", s.listEvents)
}

// scoped runs fn in a transaction under the caller's own scope, so row-level
// security filters everything the request reads or writes.
func (s *Server) scoped(r *http.Request, fn func(tx pgx.Tx) error) error {
	return store.InScope(r.Context(), s.Pool, principalOf(r).Scope(), fn)
}

// runtime runs fn under the runtime scope. Handlers use it only after they
// authorized the caller, for the writes the spec reserves to the Runtime role.
func (s *Server) runtime(ctx context.Context, fn func(tx pgx.Tx) error) error {
	return store.InScope(ctx, s.Pool, store.Runtime, fn)
}

// reply writes the value or the mapped error.
func (s *Server) reply(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	if err != nil {
		writeErr(w, r, err)
		return
	}
	writeJSON(w, status, v)
}

// leaseLock keeps an agent inside the project of its active lease: while it
// holds a lease, a request whose path names another project is refused with
// 409 (spec §0.1, AC-H-083). The project is never taken from a body.
func (s *Server) leaseLock(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalOf(r)
		project := projectInPath(r.URL.Path)
		if project == "" && !p.IsOwner() {
			var err error
			if project, err = s.resourceProject(r.Context(), r.URL.Path); err != nil {
				writeErr(w, r, err) // fail closed: an unreadable owner of the resource is not "no project" (REV-003 recheck #5)
				return
			}
		}
		// A project the caller is not a member of is answered 404 by the services;
		// answering 409 here would reveal that it exists.
		if p.IsOwner() || project == "" || p.RoleIn(project) == "" {
			next.ServeHTTP(w, r)
			return
		}
		var held *work.Held
		err := s.runtime(r.Context(), func(tx pgx.Tx) (err error) {
			held, err = work.ActiveLease(r.Context(), tx, p.ID)
			return err
		})
		if err != nil {
			writeErr(w, r, err)
			return
		}
		if held != nil && held.Lease.ProjectID != project {
			writeErr(w, r, kerr.New(kerr.Conflict, "đang giữ lease ở project khác; không tự chuyển project giữa chừng"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

// projectInPath returns the {p} of "/v1/projects/{p}/...", or "".
func projectInPath(path string) string {
	const prefix = "/v1/projects/"
	if len(path) <= len(prefix) || path[:len(prefix)] != prefix {
		return ""
	}
	rest := path[len(prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '/' {
			return rest[:i]
		}
	}
	return rest
}

// resourceTables maps "/v1/<name>/{id}" routes to the table that owns the id.
var resourceTables = map[string]string{"tasks": "task", "sources": "source", "knowledge": "knowledge", "relations": "relation"}

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// resourceProject finds the project of a task, source, knowledge item or
// relation named in the path, so the lease lock also covers routes that do
// not carry a project (REV-003 #6). GLOBAL knowledge and a resource that does
// not exist have no project (""). A database error is returned, never read as
// "no project": the caller answers 500 instead of letting the request skip the
// lease lock (spec S2 D.1, fail closed).
func (s *Server) resourceProject(ctx context.Context, path string) (string, error) {
	parts := strings.Split(strings.TrimPrefix(path, "/v1/"), "/")
	if len(parts) < 2 || !uuidRe.MatchString(parts[1]) {
		return "", nil
	}
	table, ok := resourceTables[parts[0]]
	if !ok {
		return "", nil
	}
	var project string
	err := s.runtime(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, "SELECT coalesce(project_id::text, '') FROM "+table+" WHERE id = $1", parts[1]).Scan(&project)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lease lock: project of %s %s: %w", parts[0], parts[1], err)
	}
	return project, nil
}
