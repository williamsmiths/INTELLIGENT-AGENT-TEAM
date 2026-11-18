package memory

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// SupersedeInput is the full content of the next version. Level, ProjectID, Key
// and Kind are optional echoes: when set they must equal the old version, since
// they never change across versions (spec §0, AC-H-106).
type SupersedeInput struct {
	ExpectedVersion int
	Level           string
	ProjectID       string
	Key             string
	Kind            string
	Content
}

// SupersedeKnowledge replaces the current version with version + 1 and makes the
// old one HISTORICAL (T-H-32; PM-H-07, PM-H-08). Under concurrency exactly one
// request wins; the other gets 409 (AC-H-007).
func SupersedeKnowledge(ctx context.Context, tx pgx.Tx, actor authn.Principal, id string, in SupersedeInput) (WriteResult, error) {
	old, err := loadForChange(ctx, tx, actor, id, in.ExpectedVersion)
	if err != nil {
		return WriteResult{}, err
	}
	if differs(in.Level, old.Level) || differs(in.Kind, old.Kind) || differs(in.Key, old.Key) || differs(in.ProjectID, old.ProjectID) {
		return WriteResult{}, kerr.New(kerr.Invalid, "level, key, kind and project do not change across versions")
	}
	if old.Level == LevelAgent && old.CreatedBy != actor.ID {
		// Spec §0: only the agent itself writes its AGENT memory. A version written by
		// someone else would also become unreadable to that agent under row-level security.
		return WriteResult{}, kerr.New(kerr.Forbidden, "AGENT memory is written only by its agent")
	}
	c := in.Content
	if err := checkContent(ctx, tx, actor, old.Level, old.ProjectID, old.Kind, &c); err != nil {
		return WriteResult{}, err
	}
	tasks, err := pinnedTasks(ctx, tx, old)
	if err != nil {
		return WriteResult{}, err
	}
	if len(tasks) > 0 && !actor.IsOwner() {
		// A task's requirement is the owner's baseline; an agent cannot re-point it (REV-003 #10).
		return WriteResult{}, kerr.New(kerr.Conflict, "%d task(s) pin %q: only the owner may change it", len(tasks), old.Key)
	}
	if len(tasks) > 0 && (c.Trust != TrustAuthoritative || c.Aspect != AspectToBe) {
		return WriteResult{}, kerr.New(kerr.Conflict,
			"%d open task(s) pin %q: the new version must be AUTHORITATIVE and TO_BE", len(tasks), old.Key) // AC-H-106
	}

	// The partial unique index allows one current version per key, so the old
	// version ends first; a request that lost the race updates zero rows.
	if err := endVersion(ctx, tx, old, "superseded", "", ""); err != nil {
		return WriteResult{}, err
	}
	next := Item{Level: old.Level, ProjectID: old.ProjectID, Key: old.Key, Version: old.Version + 1, Kind: old.Kind,
		NS:     nsFor(old.Level, c.Trust, actor.ID),
		Aspect: c.Aspect, Trust: c.Trust, Basis: c.Basis, Scope: c.Scope, Title: c.Title, Body: c.Body,
		AppliesWhen: c.AppliesWhen, Limits: c.Limits, SourceIDs: c.SourceIDs, CreatedBy: actor.ID,
		ContentHash: contentHash(old.Level, old.ProjectID, actor.ID, old.Kind, c)}
	out, ok, err := insertVersion(ctx, tx, next)
	if err != nil {
		return WriteResult{}, err
	}
	if !ok {
		return WriteResult{}, kerr.New(kerr.Conflict, "you already store this exact content under another key")
	}
	if _, err := tx.Exec(ctx, `UPDATE knowledge SET replaced_by = $2 WHERE id = $1`, old.ID, out.ID); err != nil {
		return WriteResult{}, wrap("link superseded version", err)
	}

	res := WriteResult{Item: out, Created: true}
	if len(tasks) > 0 {
		res.Repin = &Repin{TaskIDs: tasks, Key: out.Key, Version: out.Version}
	}
	ev := journal.Event{ProjectID: out.ProjectID, Type: EventSuperseded, Payload: eventPayload(out, map[string]any{
		"previous_id": old.ID, "reason": "superseded", "affected_tasks": tasks})}
	return res, emit(ctx, tx, actor, ev, &res.Deferred)
}

func differs(given, old string) bool {
	given = strings.TrimSpace(given)
	return given != "" && given != old
}
