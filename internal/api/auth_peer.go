package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/attest"
	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// The two sockets of harnessd (spec S2 A.1).
const (
	SocketAgent = "agent" // only `kch hook` uses it; never grants Owner
	SocketOwner = "owner" // the Owner API, in a directory hidden from every sandbox
)

// PeerAuth authenticates by process attestation: who the caller is follows
// from its ancestors as the kernel reports them, per socket (AC-S2-001..003):
//
//   - agent socket: the peer must attest as an agent that has a principal and
//     must not sit in a sandbox; everything else is 401. It never yields the owner.
//   - owner socket: a peer that attests as an enrolled agent, or that has
//     harnessd among its ancestors (a verification check), is 403. Every other
//     process of this OS user is the Owner (limitation L1′).
//
// The Authorization header is never read (AC-S2-006).
type PeerAuth struct {
	Pool   *pgxpool.Pool
	Attest attest.Config
	Socket string // SocketAgent or SocketOwner

	reported rateLimit // pid+reason -> last ATTEST_REJECTED
}

// rateLimit lets an event through once a minute per key. It lives in process
// memory and is bounded.
type rateLimit struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether key was not seen in the last minute, and records it.
func (l *rateLimit) allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[string]time.Time{}
	}
	if at, seen := l.last[key]; seen && now.Sub(at) < time.Minute {
		return false
	}
	if len(l.last) > 4096 { // forget what is old enough to be reported again
		for k, at := range l.last {
			if now.Sub(at) >= time.Minute {
				delete(l.last, k)
			}
		}
	}
	l.last[key] = now
	return true
}

// peer is the outcome of one attestation.
type peer struct {
	pid    int
	id     attest.Identity
	reason string // why the peer is refused; "" = allowed
	status int    // 401 or 403 when refused
	who    authn.Principal
}

// resolve attests the peer of a request and applies the rule of the socket.
func (a *PeerAuth) resolve(r *http.Request) (peer, error) {
	p := peer{status: http.StatusUnauthorized}
	pid, uid, err := attest.PeerCred(peerConn(r))
	if err != nil {
		p.reason = "no_peer"
		return p, nil
	}
	p.pid = pid
	if uid != os.Getuid() {
		p.reason = "foreign_uid"
		return p, nil
	}
	if p.id, err = a.Attest.Resolve(pid); err != nil {
		p.reason = "unreadable"
		return p, nil
	}
	var enrolled authn.Principal
	hasPrincipal := false
	if p.id.AgentID != "" {
		enrolled, err = authn.ByAgent(r.Context(), a.Pool, p.id.AgentID)
		if err != nil && !errors.Is(err, authn.ErrUnauthenticated) {
			return p, err
		}
		hasPrincipal = err == nil
	}
	if a.Socket == SocketAgent {
		switch {
		case p.id.HostPID == 0:
			p.reason = "no_host"
		case p.id.AgentID == "":
			p.reason = "no_agent"
		case !hasPrincipal:
			p.reason = "not_enrolled"
		case p.id.Sandboxed:
			p.reason = "sandboxed" // hooks run outside the sandbox; a sandboxed peer is not the hook client (REV-004 #17)
		default:
			p.who = enrolled
		}
		return p, nil
	}
	p.status = http.StatusForbidden
	switch {
	case p.id.HasHarnessAncestor:
		p.reason = "runner"
	case hasPrincipal:
		p.reason = "enrolled_agent"
	default:
		if p.who, err = authn.Owner(r.Context(), a.Pool); err != nil {
			return p, fmt.Errorf("owner principal: %w", err)
		}
	}
	return p, nil
}

// Authenticate implements Authenticator.
func (a *PeerAuth) Authenticate(r *http.Request) (authn.Principal, error) {
	p, err := a.resolve(r)
	if err != nil {
		return authn.Principal{}, err
	}
	if p.reason == "" {
		return p.who, nil
	}
	a.journalRejected(r.Context(), p)
	if p.status == http.StatusForbidden {
		return authn.Principal{}, kerr.New(kerr.Forbidden, "tiến trình này không được dùng API Owner (%s)", p.reason)
	}
	return authn.Principal{}, authn.ErrUnauthenticated
}

// Whoami implements Authenticator: the attestation of the peer itself (spec S2 A.4).
func (a *PeerAuth) Whoami(r *http.Request) map[string]any {
	p, err := a.resolve(r)
	out := map[string]any{"socket": a.Socket, "pid": p.pid, "agent_id": p.id.AgentID, "host_pid": p.id.HostPID,
		"provider_pid": p.id.ProviderPID, "sandboxed": p.id.Sandboxed, "harness_ancestor": p.id.HasHarnessAncestor,
		"allowed": err == nil && p.reason == "", "role": "none"}
	switch {
	case err != nil:
		out["refused"] = "internal"
	case p.reason != "":
		out["refused"] = p.reason
	default:
		out["role"] = p.who.Kind
		out["principal"] = map[string]any{"id": p.who.ID, "name": p.who.Name, "enforcement": p.who.Enforcement}
	}
	return out
}

// journalRejected records a refused peer as ATTEST_REJECTED, at most once a
// minute per pid and reason. The payload holds a pid, a reason code and the
// attested agent directory name — nothing the peer sent.
func (a *PeerAuth) journalRejected(ctx context.Context, p peer) {
	if !a.reported.allow(fmt.Sprintf("%d/%s", p.pid, p.reason)) {
		return
	}
	payload := map[string]any{"socket": a.Socket, "pid": p.pid, "reason": p.reason}
	if id := p.id.AgentID; id != "" && len(id) <= 128 && !strings.ContainsFunc(id, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		payload["agent_id"] = id
	}
	_ = store.InScope(context.WithoutCancel(ctx), a.Pool, store.Runtime, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{Type: "ATTEST_REJECTED", Payload: payload})
		return err
	})
}
