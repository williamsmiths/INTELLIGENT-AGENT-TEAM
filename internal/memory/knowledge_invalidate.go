package memory

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// InvalidateInput ends a key without a new version (T-H-34).
type InvalidateInput struct {
	ExpectedVersion int
	Reason          string // required; see the note on InvalidateKnowledge
	ReplacedByKey   string // optional: the key this item is merged into
}

// InvalidateKnowledge makes the current version HISTORICAL with no successor
// (T-H-34; PM-H-07, PM-H-08) and closes every live relation touching the key.
// A key pinned by an open task needs ReplacedByKey pointing at a current
// AUTHORITATIVE + TO_BE item of the same project, else 409 (AC-H-106).
//
// The schema keeps a reason code (invalidated / merged) and replaced_by; it has
// no column for the free-text reason, which is therefore required but not stored.
func InvalidateKnowledge(ctx context.Context, tx pgx.Tx, actor authn.Principal, id string, in InvalidateInput) (WriteResult, error) {
	old, err := loadForChange(ctx, tx, actor, id, in.ExpectedVersion)
	if err != nil {
		return WriteResult{}, err
	}
	if strings.TrimSpace(in.Reason) == "" {
		return WriteResult{}, kerr.New(kerr.Invalid, "reason is required")
	}
	mergeKey := strings.TrimSpace(in.ReplacedByKey)
	if mergeKey == old.Key {
		return WriteResult{}, kerr.New(kerr.Invalid, "replaced_by_key must be another key")
	}
	var target Item
	var haveTarget bool
	if mergeKey != "" {
		// The replacement is looked up in the key namespace of the invalidated item.
		target, haveTarget, err = currentInNamespace(ctx, tx, old.ProjectID, old.Level, old.NS, mergeKey)
		if err != nil {
			return WriteResult{}, err
		}
	}
	tasks, err := pinnedTasks(ctx, tx, old)
	if err != nil {
		return WriteResult{}, err
	}
	if len(tasks) > 0 && !actor.IsOwner() {
		// A task's requirement is the owner's baseline; an agent cannot re-point it (REV-003 #10).
		return WriteResult{}, kerr.New(kerr.Conflict, "%d task(s) pin %q: only the owner may change it", len(tasks), old.Key)
	}
	switch {
	case len(tasks) > 0 && (!haveTarget || target.Trust != TrustAuthoritative || target.Aspect != AspectToBe):
		return WriteResult{}, kerr.New(kerr.Conflict,
			"%d open task(s) pin %q: replaced_by_key must be a current AUTHORITATIVE TO_BE item", len(tasks), old.Key)
	case mergeKey != "" && !haveTarget:
		return WriteResult{}, kerr.New(kerr.Invalid, "replaced_by_key %q is not a current key", mergeKey)
	}

	reason := "invalidated"
	if haveTarget {
		reason = "merged"
	}
	if err := endVersion(ctx, tx, old, reason, target.ID, strings.TrimSpace(in.Reason)); err != nil {
		return WriteResult{}, err
	}
	closed, err := closeRelationsOf(ctx, tx, old)
	if err != nil {
		return WriteResult{}, err
	}
	out, err := getItem(ctx, tx, old.ID)
	if err != nil {
		return WriteResult{}, err
	}
	res := WriteResult{Item: out, ClosedRelations: closed}
	extra := map[string]any{"reason": reason, "affected_tasks": tasks, "closed_relations": closed}
	if haveTarget {
		extra["replaced_by"] = target.ID
		if len(tasks) > 0 {
			res.Repin = &Repin{TaskIDs: tasks, Key: target.Key, Version: target.Version}
		}
	}
	ev := journal.Event{ProjectID: out.ProjectID, Type: EventInvalidated, Payload: eventPayload(out, extra)}
	return res, emit(ctx, tx, actor, ev, &res.Deferred)
}

// closeRelationsOf sets valid_to on every live relation touching the key (T-H-34).
// Row-level security lets an agent update only relations it created; if others'
// relations touch the key the invalidation cannot be completed as specified, so
// it is refused instead of leaving live relations on a dead key.
func closeRelationsOf(ctx context.Context, tx pgx.Tx, it Item) (int, error) {
	if it.Level != LevelProject {
		return 0, nil // relations join PROJECT-level keys only
	}
	const touching = `project_id = $1 AND valid_to IS NULL AND (from_key = $2 OR to_key = $2)`
	var live int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM relation WHERE `+touching, it.ProjectID, it.Key).Scan(&live); err != nil {
		return 0, wrap("count relations", err)
	}
	if live == 0 {
		return 0, nil
	}
	tag, err := tx.Exec(ctx, `UPDATE relation SET valid_to = now() WHERE `+touching, it.ProjectID, it.Key)
	if err != nil {
		return 0, wrap("close relations", err)
	}
	if int(tag.RowsAffected()) != live {
		return 0, kerr.New(kerr.Conflict,
			"%d live relation(s) of other authors touch %q; their authors or the owner must close them first",
			live-int(tag.RowsAffected()), it.Key)
	}
	return live, nil
}
