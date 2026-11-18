package work

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
)

var commitRe = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// AssignTask grants the lease of a RUNNABLE task (T-H-02, T-H-20). mode is
// OBSERVE, CONTEXT or FULL (default). Caller must use a trusted scope
// (store.Runtime): a Lead cannot read the assignee's membership or leases in
// other projects under its own scope, so this function authorizes the actor
// itself — Owner or Lead of the task's project (AC-H-012), and a Lead never
// assigns to itself (AC-H-017).
//
// The task row lock serializes assigns of one task (AC-H-014); an advisory
// lock per project serializes the WIP count; the unique partial indexes on
// lease stay the last word for "one lease per task, principal and repo root".
// bases is the trusted base store every later comparison reads (spec S2 D.1).
func AssignTask(ctx context.Context, tx pgx.Tx, bases verify.Store, actor authn.Principal, taskID, assigneeID, mode string) (Change, error) {
	return AssignTaskWith(ctx, tx, bases, actor, taskID, assigneeID, mode, IntelOptions{})
}

// AssignTaskWith is AssignTask with the owner's choices for the Intelligence
// Layer (spec S2 C.1, PM-S2-13): a forced intel mode and the switched-on
// mechanisms. Only the Owner may set them (403 for a Lead).
func AssignTaskWith(ctx context.Context, tx pgx.Tx, bases verify.Store, actor authn.Principal, taskID, assigneeID, mode string, opt IntelOptions) (Change, error) {
	if mode == "" {
		mode = "FULL"
	}
	if mode != "OBSERVE" && mode != "CONTEXT" && mode != "FULL" {
		return Change{}, kerr.New(kerr.Invalid, "mode must be OBSERVE, CONTEXT or FULL")
	}
	if (opt.Mode != "" || opt.Features != nil) && !actor.IsOwner() {
		return Change{}, kerr.New(kerr.Forbidden, "only the owner sets intel_mode or intel_features")
	}
	t, err := manageTask(ctx, tx, actor, taskID)
	if err != nil {
		return Change{}, err
	}
	if !actor.IsOwner() && assigneeID == actor.ID {
		return Change{}, kerr.New(kerr.Forbidden, "a lead cannot assign a task to itself")
	}
	var member bool
	if validID(assigneeID) {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM project_member WHERE project_id = $1 AND principal_id = $2)`,
			t.ProjectID, assigneeID).Scan(&member); err != nil {
			return Change{}, err
		}
	}
	if !member {
		return Change{}, kerr.New(kerr.Invalid, "assignee is not a member of the project")
	}
	if err := requireHookInstall(ctx, tx, assigneeID, mode); err != nil { // AC-S2-011
		return Change{}, err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kch.wip:' || $1, 0))`, t.ProjectID); err != nil {
		return Change{}, err
	}
	if err := expireBlocking(ctx, tx, t, assigneeID); err != nil {
		return Change{}, err
	}
	if t, err = loadTask(ctx, tx, taskID, false); err != nil { // an overdue lease of this task just ended (T-H-05)
		return Change{}, err
	}
	if t.State != "RUNNABLE" {
		return Change{}, kerr.New(kerr.Conflict, "task is %s, only a RUNNABLE task can be assigned", t.State)
	}
	var busy, repoBusy bool
	var active int
	var wip *int
	if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM lease WHERE principal_id = $1 AND state = 'ACTIVE'),
			EXISTS (SELECT 1 FROM lease WHERE repo_path = $2 AND repo_path <> '' AND state = 'ACTIVE'),
			(SELECT count(*) FROM lease WHERE project_id = $3 AND state = 'ACTIVE'),
			(SELECT max_active_leases FROM project WHERE id = $3)`,
		assigneeID, t.RepoPath, t.ProjectID).Scan(&busy, &repoBusy, &active, &wip); err != nil {
		return Change{}, err
	}
	switch {
	case busy:
		return Change{}, kerr.New(kerr.Conflict, "assignee already holds an active lease")
	case repoBusy:
		return Change{}, kerr.New(kerr.Conflict, "the repo root already has an active lease")
	case wip != nil && active >= *wip:
		return Change{}, kerr.New(kerr.Conflict, "project reached max_active_leases (%d)", *wip)
	}
	b, err := budgetView(ctx, tx, t)
	if err != nil {
		return Change{}, err
	}
	for level, state := range map[string]string{"TASK": b.Task.State, "PROJECT": b.ProjectState, "SYSTEM": b.SystemState} {
		if state == "EXHAUSTED" {
			return Change{}, kerr.New(kerr.Conflict, "%s budget is EXHAUSTED", level)
		}
	}
	if t.BaseCommit, err = pinBase(ctx, tx, bases, t); err != nil { // pinned again at every assignment: a no-op once it is there
		return Change{}, err
	}
	notice, err := missedNotices(ctx, tx, t.ID) // AC-S2-039: what the owner said while nobody held the task
	if err != nil {
		return Change{}, err
	}
	opt.writeNotice = notice
	l, err := grantLease(ctx, tx, t, assigneeID, mode, notice, "assigned", actor.ID, opt)
	if err != nil {
		return Change{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE task SET state = 'RUNNING', base_commit = $2, updated_at = now() WHERE id = $1`,
		t.ID, t.BaseCommit); err != nil {
		return Change{}, err
	}
	ch := Change{Granted: l}
	var prev string
	err = tx.QueryRow(ctx, `SELECT project_id FROM lease WHERE principal_id = $1 AND id <> $2
		ORDER BY granted_at DESC, epoch DESC LIMIT 1`, assigneeID, l.ID).Scan(&prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Change{}, err
	}
	ch.ProjectChanged = prev != "" && prev != t.ProjectID
	ch.Task, err = loadTask(ctx, tx, t.ID, false)
	return ch, err
}

// expireBlocking ends overdue leases that would block this assign: T-H-23 is
// checked whenever a lease is read, not only by the sweep. It may lock a second
// task out of id order; PostgreSQL resolves that rare deadlock and store.InTx retries.
func expireBlocking(ctx context.Context, tx pgx.Tx, t Task, assigneeID string) error {
	rows, err := tx.Query(ctx, `SELECT task_id::text FROM lease WHERE state = 'ACTIVE' AND expires_at < now()
		AND (task_id = $1 OR principal_id = $2 OR (repo_path = $3 AND repo_path <> '')) ORDER BY task_id`,
		t.ID, assigneeID, t.RepoPath)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := expireTask(ctx, tx, id); err != nil {
			return err
		}
	}
	return nil
}

// grantLease inserts the next lease of a task (T-H-20): epoch = highest epoch + 1,
// context not acknowledged, 8 hours to live.
// The router of spec S2 C.1 runs here, so every new lease — assignment,
// feedback, changed requirement, answered question — gets the mode of the
// current product model (AC-S2-030).
func grantLease(ctx context.Context, tx pgx.Tx, t Task, principalID, mode, notice, reason, actorID string, opt IntelOptions) (*Lease, error) {
	in, err := routeLease(ctx, tx, t, mode, opt)
	if err != nil {
		return nil, err
	}
	l, err := scanLease(tx.QueryRow(ctx, `INSERT INTO lease
			(project_id, task_id, principal_id, epoch, task_revision, repo_path, mode, pending_notice, expires_at,
			 intel_mode, intel_signals, intel_features, intel_forced, intel_phase, write_notice)
		VALUES ($1, $2, $3, (SELECT COALESCE(max(epoch), 0) + 1 FROM lease WHERE task_id = $2), $4, $5, $6, $7,
			now() + interval '8 hours', $8, $9, $10, $11, $12, $13) RETURNING `+leaseCols,
		t.ProjectID, t.ID, principalID, t.Revision, t.RepoPath, mode, notice,
		in.mode, in.signals, in.features, opt.Mode != "", in.phase, opt.writeNotice))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, kerr.New(kerr.Conflict, "lease conflict (%s)", pgErr.ConstraintName)
		}
		return nil, err
	}
	payload := map[string]any{"lease_id": l.ID, "principal_id": principalID, "epoch": l.Epoch,
		"mode": mode, "revision": t.Revision, "reason": reason, "intel_mode": in.mode, "intel_phase": in.phase} // phase at grant: RecordUsage (REV-005 #10)
	if opt.via != "" {
		payload["via"] = opt.via
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "LEASE_GRANTED", Actor: actorID, Payload: payload})
	return l, err
}

// pinBase copies the task's base commit into the trusted base store and
// returns it (T-H-02, spec S2 D.1). At the first assignment the base is HEAD
// and the root must be clean, or leftovers of an earlier task would fail this
// task's scope check forever (REV-003 #5). "Clean" = ChangedFiles(repo, HEAD)
// is empty under the verification snapshot — not `git status`, which believes
// the repository's own config, index flags and ignore files (AC-S2-052).
// Files on the trusted ignore list do not count. No git command here can be
// made to start a program by the repository's config (AC-S2-050 e).
func pinBase(ctx context.Context, tx pgx.Tx, bases verify.Store, t Task) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitBudget)
	defer cancel()
	base, first := t.BaseCommit, t.BaseCommit == ""
	if first {
		head, err := verify.HeadCommit(ctx, t.RepoPath)
		if err != nil || !commitRe.MatchString(head) {
			return "", kerr.New(kerr.Conflict, "repo root has no HEAD commit to pin as base_commit")
		}
		base = head
	}
	if err := bases.PinBase(ctx, t.RepoPath, base); err != nil {
		if kerr.Is(err, kerr.Invalid) { // the store itself is misconfigured: not the caller's conflict
			return "", fmt.Errorf("assign: %w", err)
		}
		slog.WarnContext(ctx, "assign: base commit cannot be pinned", "task", t.ID, "err", err)
		return "", kerr.New(kerr.Conflict, "base_commit cannot be copied into the trusted base store; the repository's objects are damaged or the commit is gone")
	}
	if !first {
		return base, nil
	}
	ignore, err := RepoIgnore(ctx, tx, t.RepoPath)
	if err != nil {
		return "", err
	}
	changed, err := verify.ChangedFiles(ctx, verify.Tree{Repo: t.RepoPath, Base: base, Store: bases, Ignore: ignore})
	if err != nil {
		slog.WarnContext(ctx, "assign: repo root cannot be read", "task", t.ID, "err", err)
		return "", kerr.New(kerr.Conflict, "cannot read the state of the repo root")
	}
	if len(changed) > 0 {
		return "", kerr.New(kerr.Conflict, "repo root has %d uncommitted or untracked file(s) outside the trusted ignore list (first: %s); "+
			"commit or clean them, or have the owner extend the list, before the first assignment", len(changed), changed[0])
	}
	return base, nil
}
