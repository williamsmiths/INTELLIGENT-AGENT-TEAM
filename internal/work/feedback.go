package work

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Feedback stores the owner's text verbatim as a Source (origin OWNER) and
// steers the one task it is about (PM-H-14; owner only; actor scope):
// revision + 1 in every open state, VERIFIED goes through T-H-04, the ACTIVE
// lease is replaced per T-H-22 with the text as pending notice (AC-H-050,
// AC-H-056). A SUPERSEDED task answers 409 (AC-H-054). No other task, lease or
// project is touched (AC-H-052, AC-H-082).
func Feedback(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID, text string) (Change, error) {
	if err := requireOwner(actor); err != nil {
		return Change{}, err
	}
	if strings.TrimSpace(text) == "" {
		return Change{}, kerr.New(kerr.Invalid, "feedback text is required")
	}
	t, err := loadTask(ctx, tx, taskID, true)
	if err != nil {
		return Change{}, err
	}
	if t.State == "SUPERSEDED" {
		return Change{}, kerr.New(kerr.Conflict, "task is SUPERSEDED")
	}
	sum := sha256.Sum256([]byte(text))
	var sourceID string
	if err := tx.QueryRow(ctx, `INSERT INTO source (project_id, kind, origin, content, content_hash, created_by)
		VALUES ($1, 'feedback', 'OWNER', $2, $3, $4) RETURNING id`,
		t.ProjectID, text, hex.EncodeToString(sum[:]), actor.ID).Scan(&sourceID); err != nil {
		return Change{}, err
	}
	ch, err := reviseTask(ctx, tx, t, "feedback", text, actor.ID)
	if err != nil {
		return Change{}, err
	}
	ch.SourceID = sourceID
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "TASK_FEEDBACK",
		Actor: actor.ID, Payload: map[string]any{"source_id": sourceID, "revision": ch.Task.Revision}})
	return ch, err
}

// reviseTask is the shared effect of feedback and of a changed dependency on a
// locked, open task: revision + 1, T-H-04 when VERIFIED, T-H-22 with re-grant.
func reviseTask(ctx context.Context, tx pgx.Tx, t Task, reason, notice, actorID string) (Change, error) {
	var rev int
	if err := tx.QueryRow(ctx, `UPDATE task SET revision = revision + 1, updated_at = now() WHERE id = $1
		RETURNING revision`, t.ID).Scan(&rev); err != nil {
		return Change{}, err
	}
	if _, err := staleTask(ctx, tx, t, reason, actorID); err != nil {
		return Change{}, err
	}
	return replaceLease(ctx, tx, t.ID, reason, notice, rev, actorID)
}

// OnDependencyChanged is called by the memory package after it superseded
// (oldKey == newKey) or invalidated-and-merged (oldKey -> newKey) a knowledge
// key that open tasks of the same project pin (T-H-32, T-H-34). Each listed
// task that still names oldKey is re-pinned to newKey at newVersion and gets
// revision + 1, T-H-04 and T-H-22 with reason "dependency_changed" and the
// notice as pending notice (AC-H-053, AC-H-106). SUPERSEDED tasks and tasks
// that do not name oldKey are skipped. Tasks are locked in id order.
// Caller must use a trusted scope (owner or store.Runtime).
func OnDependencyChanged(ctx context.Context, tx pgx.Tx, taskIDs []string, oldKey, newKey string, newVersion int, notice string) ([]Change, error) {
	if oldKey == "" || newKey == "" || newVersion < 1 {
		return nil, kerr.New(kerr.Invalid, "old key, new key and new version are required")
	}
	ids := slices.Clone(taskIDs)
	slices.Sort(ids)
	var out []Change
	for _, id := range slices.Compact(ids) {
		t, err := loadTask(ctx, tx, id, true)
		if err != nil {
			return nil, err
		}
		if t.State == "SUPERSEDED" || !slices.Contains(t.RequirementKeys, oldKey) {
			continue
		}
		keys := make([]string, 0, len(t.RequirementKeys))
		for _, k := range t.RequirementKeys {
			if k == oldKey {
				k = newKey
			}
			if !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		// A PROPOSED task has pinned nothing yet: only its key list follows the merge.
		if t.State != "PROPOSED" {
			delete(t.DependencyVersions, oldKey)
			t.DependencyVersions[newKey] = newVersion
		}
		pinned, err := json.Marshal(t.DependencyVersions)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE task SET requirement_keys = $2, dependency_versions = $3 WHERE id = $1`,
			t.ID, keys, pinned); err != nil {
			return nil, err
		}
		ch, err := reviseTask(ctx, tx, t, "dependency_changed", notice, "")
		if err != nil {
			return nil, err
		}
		if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID,
			Type: "TASK_DEPENDENCY_CHANGED", Payload: map[string]any{"old_key": oldKey, "new_key": newKey,
				"new_version": newVersion, "revision": ch.Task.Revision}}); err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}
