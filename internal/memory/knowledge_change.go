package memory

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// ConfirmKnowledge turns the current CANDIDATE or OBSERVED version into
// AUTHORITATIVE (T-H-31, PM-H-06). Owner only; a confirmed GLOBAL item moves
// to the shared key namespace.
func ConfirmKnowledge(ctx context.Context, tx pgx.Tx, actor authn.Principal, id string, expectedVersion int) (WriteResult, error) {
	if !actor.IsOwner() {
		return WriteResult{}, kerr.New(kerr.Forbidden, "only the owner confirms knowledge") // AC-H-002
	}
	it, err := getItem(ctx, tx, id)
	if err != nil {
		return WriteResult{}, err
	}
	if !it.IsCurrent || it.Version != expectedVersion {
		return WriteResult{}, kerr.New(kerr.Conflict, "expected_version %d is not the current version", expectedVersion) // AC-H-006
	}
	if it.Trust != TrustCandidate && it.Trust != TrustObserved {
		return WriteResult{}, kerr.New(kerr.Conflict, "only CANDIDATE or OBSERVED knowledge can be confirmed (is %s)", it.Trust)
	}
	if it.Level == LevelGlobal {
		if _, taken, err := currentInNamespace(ctx, tx, "", LevelGlobal, "", it.Key); err != nil {
			return WriteResult{}, err
		} else if taken {
			return WriteResult{}, kerr.New(kerr.Conflict, "a confirmed GLOBAL item already uses key %q", it.Key)
		}
	}
	out, err := scanItem(tx.QueryRow(ctx, `UPDATE knowledge k
		SET trust = 'AUTHORITATIVE', ns = CASE WHEN k.level = 'GLOBAL' THEN NULL ELSE k.ns END
		WHERE k.id = $1 AND k.is_current AND k.version = $2 AND k.trust IN ('CANDIDATE','OBSERVED')
		RETURNING `+itemCols, id, expectedVersion))
	if errors.Is(err, pgx.ErrNoRows) {
		return WriteResult{}, kerr.New(kerr.Conflict, "knowledge changed concurrently")
	}
	if c := uniqueViolation(err); c != "" {
		return WriteResult{}, kerr.New(kerr.Conflict, "a confirmed item already uses key %q (%s)", it.Key, c)
	}
	if err != nil {
		return WriteResult{}, wrap("confirm knowledge", err)
	}
	res := WriteResult{Item: out}
	ev := journal.Event{ProjectID: out.ProjectID, Type: EventConfirmed, Payload: eventPayload(out, nil)}
	return res, emit(ctx, tx, actor, ev, &res.Deferred)
}

// loadForChange loads the version a supersede or invalidate targets and applies
// PM-H-07 / PM-H-08: the owner may change anything; an agent only its own item
// that is not AUTHORITATIVE. A wrong role is 403 before a wrong version is 409.
func loadForChange(ctx context.Context, tx pgx.Tx, actor authn.Principal, id string, expectedVersion int) (Item, error) {
	it, err := getItem(ctx, tx, id)
	if err != nil {
		return Item{}, err
	}
	if !actor.IsOwner() {
		if it.Level != LevelGlobal && actor.RoleIn(it.ProjectID) == "" {
			return Item{}, kerr.New(kerr.NotFound, "knowledge not found")
		}
		if it.CreatedBy != actor.ID || it.Trust == TrustAuthoritative {
			return Item{}, kerr.New(kerr.Forbidden, "agents change only their own, not yet AUTHORITATIVE knowledge") // AC-H-005
		}
	}
	if !it.IsCurrent || it.Version != expectedVersion {
		return Item{}, kerr.New(kerr.Conflict, "expected_version %d is not the current version", expectedVersion)
	}
	return it, nil
}

// pinnedTasks lists open tasks of the project that pin the key (AC-H-106). A key
// is pinned when a task with state <> SUPERSEDED has it in requirement_keys.
// Only PROJECT-level keys can be pinned (T-H-01). The task table is read, never written.
func pinnedTasks(ctx context.Context, tx pgx.Tx, it Item) ([]string, error) {
	if it.Level != LevelProject {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id::text FROM task
		WHERE project_id = $1 AND state <> 'SUPERSEDED' AND $2 = ANY (requirement_keys)
		ORDER BY created_at, id`, it.ProjectID, it.Key)
	if err != nil {
		return nil, wrap("load pinning tasks", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	return ids, wrap("load pinning tasks", err)
}

// endVersion closes the current version: HISTORICAL, not current, with a reason
// code and an optional replacement. Zero rows means another request changed the
// key first (AC-H-007).
func endVersion(ctx context.Context, tx pgx.Tx, it Item, reason, replacedBy, note string) error {
	tag, err := tx.Exec(ctx, `UPDATE knowledge
		SET is_current = false, trust = 'HISTORICAL', ended_reason = $3, replaced_by = nullif($4,'')::uuid, ended_note = $5
		WHERE id = $1 AND is_current AND version = $2`, it.ID, it.Version, reason, replacedBy, note)
	if err != nil {
		return wrap("end knowledge version", err)
	}
	if tag.RowsAffected() == 0 {
		return kerr.New(kerr.Conflict, "version %d of %q is no longer current", it.Version, it.Key)
	}
	return nil
}
