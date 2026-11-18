// Package memory is the Native Memory & Context layer of the harness (spec §0):
// sources, versioned knowledge with trust levels, SQL relations, the blackboard,
// staged retrieval (key → FTS → trigram) and the context packet.
//
// Every exported function runs inside the caller's transaction, which the caller
// opened with store.InScope(ctx, pool, actor.Scope(), ...). The functions do the
// Permission Matrix checks themselves; row-level security is the second line.
package memory

import (
	"context"
	"errors"
	"regexp"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Knowledge levels (spec §0, "Memory một hệ thống, ba phạm vi").
const (
	LevelGlobal  = "GLOBAL"
	LevelProject = "PROJECT"
	LevelAgent   = "AGENT"
)

// Trust levels (spec §2.3).
const (
	TrustAuthoritative = "AUTHORITATIVE"
	TrustObserved      = "OBSERVED"
	TrustCandidate     = "CANDIDATE"
	TrustHistorical    = "HISTORICAL"
)

// Aspects of the Live Product Model.
const (
	AspectAsIs = "AS_IS"
	AspectToBe = "TO_BE"
)

// Journal event types written by this package. Payloads carry ids, keys,
// versions and reason codes only (PM-H-20).
const (
	EventAdded       = "KNOWLEDGE_ADDED"
	EventConfirmed   = "KNOWLEDGE_CONFIRMED"
	EventSuperseded  = "KNOWLEDGE_SUPERSEDED" // name not in the spec: T-H-32 names no event
	EventInvalidated = "KNOWLEDGE_INVALIDATED"
)

var (
	levels  = []string{LevelGlobal, LevelProject, LevelAgent}
	kinds   = []string{"goal", "requirement", "rule", "ui", "flow", "api", "data", "architecture", "dependency", "decision", "experience"}
	aspects = []string{AspectAsIs, AspectToBe}
	trusts  = []string{TrustAuthoritative, TrustObserved, TrustCandidate, TrustHistorical}
	bases   = []string{"proposed", "inferred", "unknown"}

	relationTypes   = []string{"implements", "depends_on", "verifies", "impacts", "consumes", "owns"}
	blackboardKinds = []string{"finding", "evidence", "proposal", "conflict", "decision", "impact", "next_action"}

	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

func isUUID(s string) bool { return uuidRe.MatchString(s) }

func oneOf(v string, allowed []string) bool { return slices.Contains(allowed, v) }

// requireMember returns the actor's role in the project ("owner", "lead" or
// "worker"). Anyone else gets NotFound: the spec hides existence (§3, AC-H-004).
func requireMember(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string) (string, error) {
	if !isUUID(projectID) {
		return "", kerr.New(kerr.NotFound, "project not found")
	}
	role := actor.RoleIn(projectID)
	if role == "" {
		return "", kerr.New(kerr.NotFound, "project not found")
	}
	if actor.IsOwner() {
		var one int
		err := tx.QueryRow(ctx, `SELECT 1 FROM project WHERE id = $1`, projectID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", kerr.New(kerr.NotFound, "project not found")
		}
		if err != nil {
			return "", wrap("load project", err)
		}
	}
	return role, nil
}

// emit appends a journal event. Row-level security refuses project-less journal
// rows written under an agent scope (journal_all checks kch_sees(project_id)),
// so those events are handed back to the caller in *deferred, to be appended
// under store.Runtime in the same request.
func emit(ctx context.Context, tx pgx.Tx, actor authn.Principal, ev journal.Event, deferred *[]journal.Event) error {
	ev.Actor = actor.ID
	if ev.ProjectID == "" && !actor.IsOwner() {
		*deferred = append(*deferred, ev)
		return nil
	}
	if _, err := journal.Append(ctx, tx, ev); err != nil {
		return wrap("append journal "+ev.Type, err)
	}
	return nil
}

// uniqueViolation returns the violated constraint name, or "" when err is not a unique violation.
func uniqueViolation(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		if pgErr.ConstraintName == "" {
			return "unique"
		}
		return pgErr.ConstraintName
	}
	return ""
}

type wrapped struct {
	op  string
	err error
}

func (w *wrapped) Error() string { return "memory: " + w.op + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// wrap adds context to an internal error; typed kerr errors pass through unchanged
// so the HTTP layer keeps their safe message.
func wrap(op string, err error) error {
	if err == nil || kerr.KindOf(err) != 0 {
		return err
	}
	return &wrapped{op: op, err: err}
}
