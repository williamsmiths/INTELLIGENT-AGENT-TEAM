package work

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// recipient is a member of the project with the asked specialty.
type recipient struct {
	id, name, role, kind, enforcement, agent string
	registered, tampered                     bool
}

// delegateRecipient finds the single Worker with the specialty (E.1-2, N6):
// only role=worker members are candidates, a Lead of the same specialty is
// ignored. No Worker but a Lead has it → 403; nobody → 422; several → 409.
// The Worker must be really protected (E.1-3, REV-006 H3) → else 409.
func delegateRecipient(ctx context.Context, tx pgx.Tx, projectID, specialty string) (recipient, error) {
	rows, err := tx.Query(ctx, `SELECT p.id::text, p.name, m.role, p.kind, p.enforcement, COALESCE(p.openbot_agent_id, ''),
			h.principal_id IS NOT NULL, COALESCE(h.tampered, false)
		FROM project_member m JOIN principal p ON p.id = m.principal_id
		LEFT JOIN hook_install h ON h.principal_id = p.id
		WHERE m.project_id = $1 AND p.specialty = $2 ORDER BY p.created_at`, projectID, specialty)
	if err != nil {
		return recipient{}, err
	}
	var all []recipient
	var r recipient
	if _, err := pgx.ForEachRow(rows, []any{&r.id, &r.name, &r.role, &r.kind, &r.enforcement, &r.agent, &r.registered, &r.tampered},
		func() error { all = append(all, r); return nil }); err != nil {
		return recipient{}, err
	}
	workers := slices.DeleteFunc(slices.Clone(all), func(m recipient) bool { return m.role != "worker" })
	switch {
	case len(workers) == 0 && len(all) > 0:
		return recipient{}, kerr.New(kerr.Forbidden, "thành viên chuyên môn %s của project là Lead; kch delegate chỉ giao cho Worker", specialty)
	case len(workers) == 0:
		return recipient{}, kerr.New(kerr.Invalid, "project không có Worker chuyên môn %s", specialty)
	case len(workers) > 1:
		return recipient{}, kerr.New(kerr.Conflict, "project có %d Worker chuyên môn %s; Owner phải giao trực tiếp", len(workers), specialty)
	}
	w := workers[0]
	var missing string
	switch {
	case w.kind != "agent" || w.enforcement != "HOOK":
		missing = "không chạy với enforcement HOOK"
	case !w.registered:
		missing = "chưa ghi danh hook (kch enroll)"
	case w.tampered:
		missing = "có cấu hình hook bị sửa"
	case w.agent == "":
		missing = "không có openbot_agent_id"
	}
	if missing != "" {
		return recipient{}, kerr.New(kerr.Conflict, "người nhận %s %s; Owner cần sửa trước khi giao", w.name, missing)
	}
	return w, nil
}

// delegateRepo picks the repo root (E.1-6): the given one must be registered
// for the project (422); without one the project must have exactly one (422
// when it has several, 409 when it has none).
func delegateRepo(ctx context.Context, tx pgx.Tx, projectID, want string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT root FROM project_repo WHERE project_id = $1 ORDER BY root`, projectID)
	if err != nil {
		return "", err
	}
	roots, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return "", err
	}
	if want != "" {
		if c, err := guard.Canonical(want, ""); err == nil {
			want = c
		}
		if !slices.Contains(roots, want) {
			return "", kerr.New(kerr.Invalid, "repo không phải repo root đã đăng ký của project")
		}
		return want, nil
	}
	switch len(roots) {
	case 0:
		return "", kerr.New(kerr.Conflict, "project chưa có repo root nào; Owner đăng ký repo trước")
	case 1:
		return roots[0], nil
	}
	return "", kerr.New(kerr.Invalid, "project có %d repo; ghi rõ \"repo\" (một trong: %v)", len(roots), roots)
}

// delegateFree refuses a busy repo (E.1-6, lease_active_repo_uq) or a busy
// recipient (T-H-02, N4) before anything is created. Overdue leases are left to
// AssignTask, which ends them (T-H-23).
func delegateFree(ctx context.Context, tx pgx.Tx, repo string, to recipient) error {
	var repoBusy, toBusy bool
	if err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM lease WHERE repo_path = $1 AND state = 'ACTIVE' AND expires_at >= now()),
			EXISTS (SELECT 1 FROM lease WHERE principal_id = $2 AND state = 'ACTIVE' AND expires_at >= now())`,
		repo, to.id).Scan(&repoBusy, &toBusy); err != nil {
		return err
	}
	switch {
	case repoBusy:
		return kerr.New(kerr.Conflict, "repo đang bận: %s đang có lease ACTIVE; chờ lease đó kết thúc", repo)
	case toBusy:
		return kerr.New(kerr.Conflict, "người nhận %s đang giữ một lease ACTIVE; chờ xong rồi giao", to.name)
	}
	return nil
}

// delegateKey returns D-<yyMMdd>-<n> with n = the largest n already used with
// that prefix in the project + 1, so a key the Owner wrote by hand is never hit
// again (E.1-9, REV-006b N5). The per-project lock serializes delegates; a
// retried transaction computes the key again.
func delegateKey(ctx context.Context, tx pgx.Tx, projectID string, now time.Time) (string, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kch.delegate:' || $1, 0))`, projectID); err != nil {
		return "", err
	}
	prefix := "D-" + now.Format("060102") + "-"
	var n int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(substr(key, length($2) + 1)::bigint), 0) + 1 FROM task
		WHERE project_id = $1 AND key ~ ('^' || $2 || '[0-9]{1,9}$')`, projectID, prefix).Scan(&n); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%d", prefix, n), nil
}
