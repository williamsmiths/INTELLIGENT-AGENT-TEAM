package api

import (
	"context"
	"errors"
	"net"
	"net/http"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

type ctxKey int

const (
	principalKey ctxKey = 1
	connKey      ctxKey = 2
)

// Authenticator names the caller of a request. Identity, roles and project
// membership come only from here (spec §0.1); nothing a request carries in
// its headers or body can change them.
//
// PeerAuth is the only implementation in a production binary: the caller is
// the process on the other end of a unix socket (spec S2 A.1). The bearer
// token authenticator of S1 exists in test builds only (export_test.go).
type Authenticator interface {
	// Authenticate returns the principal, authn.ErrUnauthenticated (401) or a
	// kerr.Forbidden error (403).
	Authenticate(r *http.Request) (authn.Principal, error)
	// Whoami describes what the caller attests as, for GET /v1/whoami. It
	// answers every peer, also one Authenticate refuses.
	Whoami(r *http.Request) map[string]any
}

// ConnContext is the http.Server.ConnContext of both sockets: it keeps the
// connection of a request, so the peer can be asked from the kernel.
func ConnContext(ctx context.Context, c net.Conn) context.Context {
	return context.WithValue(ctx, connKey, c)
}

// peerConn returns the unix connection a request arrived on, or nil.
func peerConn(r *http.Request) *net.UnixConn {
	c, _ := r.Context().Value(connKey).(*net.UnixConn)
	return c
}

// authenticate resolves the caller to a principal before any handler runs.
func authenticate(auth Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, err := auth.Authenticate(r)
			switch {
			case errors.Is(err, authn.ErrUnauthenticated):
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "không xác thực được tiến trình gọi (attest thất bại hoặc agent chưa ghi danh)"})
				return
			case err != nil:
				writeErr(w, r, err)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey, p)))
		})
	}
}

// principalOf returns the authenticated caller.
func principalOf(r *http.Request) authn.Principal {
	p, _ := r.Context().Value(principalKey).(authn.Principal)
	return p
}

// ownerOnly is the "middleware owner-only" of spec §3.
func ownerOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !principalOf(r).IsOwner() {
			writeErr(w, r, kerr.New(kerr.Forbidden, "chỉ Owner được thực hiện"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
