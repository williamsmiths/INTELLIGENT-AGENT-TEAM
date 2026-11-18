package api

import (
	"context"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
)

// This file is compiled into test binaries only. It keeps the bearer-token
// authenticator of S1 for the httptest-based e2e tests; a production binary
// (cmd/harnessd) cannot reach it, so tokens cannot be switched on (AC-S2-006).

// TokenAuth resolves "Authorization: Bearer <token>" to a principal.
type TokenAuth struct{ Pool *pgxpool.Pool }

// Authenticate implements Authenticator.
func (a TokenAuth) Authenticate(r *http.Request) (authn.Principal, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return authn.Principal{}, authn.ErrUnauthenticated
	}
	return authn.Authenticate(r.Context(), a.Pool, strings.TrimSpace(token))
}

// Whoami implements Authenticator.
func (a TokenAuth) Whoami(r *http.Request) map[string]any {
	p, err := a.Authenticate(r)
	if err != nil {
		return map[string]any{"socket": "token", "allowed": false, "role": "none"}
	}
	return map[string]any{"socket": "token", "allowed": true, "role": p.Kind, "agent_id": p.OpenBotAgentID,
		"principal": map[string]any{"id": p.ID, "name": p.Name, "enforcement": p.Enforcement}}
}

// Routes serves the union of both sockets behind token authentication.
func (s *Server) Routes() http.Handler {
	return s.router(TokenAuth{Pool: s.Pool}, s.ownerAPI, s.isolationAPI, s.agentAPI)
}

// A verb that exists in test binaries only: it shows that a verb is added by
// registering it in init() and that a heredoc body reaches the handler byte
// for byte (AC-S2-004).
func init() {
	RegisterAgentVerb("echo-body", func(_ context.Context, _ *Server, c *AgentCall) (AgentReply, error) {
		return AgentReply{Text: "body=" + string(c.Body) + "|args=" + strings.Join(c.Args, ",") + "|stdin=" + c.Stdin,
			Data: map[string]any{"raw": string(c.Body)}}, nil
	})
}
