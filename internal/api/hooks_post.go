package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// hookPostTool records the tool call in the budget and, for a FULL lease,
// checks what the call changed (AC-H-026, AC-H-043, AC-H-109). After a Bash
// call the working tree is read once; a tree that cannot be read is answered
// with a block and journaled, never taken as "nothing changed" (AC-S2-051).
func (s *Server) hookPostTool(ctx context.Context, c hookCall) (any, error) {
	if c.held == nil {
		return nil, nil
	}
	id := c.in.ToolUseID
	if id == "" {
		id = fmt.Sprintf("%s:%d", c.in.SessionID, time.Now().UnixNano())
	}
	id = c.actor.ID + ":" + id // tool ids are unique per agent, not across agents (REV-003 #11)
	t := c.held.Task
	wrote := c.in.writePath() != ""
	stale := c.full() && t.State == "VERIFIED" && wrote
	var violations []string
	var snapErr error
	var cand string
	if c.full() && c.in.ToolName == "Bash" && t.BaseCommit != "" {
		var changed []string
		if cand, changed, snapErr = s.snapshot(ctx, t); snapErr == nil {
			wrote = wrote || len(changed) > 0
			violations = verify.ScopeCheck(changed, t.AllowedPaths, t.ProtectedPaths)
			// Bash: stale only when the verified tree really changed. An unreadable
			// tree is not proof of a change (REV-003 #8) — it is reported below instead.
			stale = t.State == "VERIFIED" && cand != t.VerifiedCandidate
		}
	}
	firstFailure := snapshotFailing(c.held.Lease.ID, snapErr != nil)
	err := s.runtime(ctx, func(tx pgx.Tx) error {
		if _, err := work.RecordUsage(ctx, tx, work.LedgerEntry{Source: "hook_tool", ExternalID: id, PrincipalID: c.actor.ID,
			OpenBotAgentID: c.actor.OpenBotAgentID, Usage: work.Usage{ToolCalls: 1}, OccurredAt: time.Now().UTC()}); err != nil {
			return err
		}
		if wrote {
			if err := work.MarkWrote(ctx, tx, c.held.Lease.ID); err != nil {
				return err
			}
		}
		if stale {
			if _, err := work.MarkStale(ctx, tx, t.ID); err != nil { // T-H-04
				return err
			}
		}
		if firstFailure {
			if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "SNAPSHOT_FAILED",
				Actor: c.actor.ID, Payload: map[string]any{"lease_id": c.held.Lease.ID, "error": snapshotReason(snapErr)}}); err != nil {
				return err
			}
		}
		if len(violations) > 0 { // the agent is told every time; the journal records each distinct violation once
			_, err := journalViolation(ctx, tx, c, violations)
			return err
		}
		return nil
	})
	if err != nil {
		if firstFailure {
			snapshotFailing(c.held.Lease.ID, false) // not journaled: the next failure must try again
		}
		return nil, err
	}
	if snapErr != nil {
		slog.WarnContext(ctx, "post-tool: working tree cannot be read", "task", t.ID, "err", snapErr)
		return block("Harness không đọc được trạng thái working tree của repo sau lệnh vừa chạy (" + snapshotReason(snapErr) +
			"), nên không kiểm được phạm vi thay đổi. Hãy khôi phục repo về trạng thái đọc được (quyền đọc thư mục và file, " +
			"không đặt repo git lồng hoặc submodule có file) rồi tiếp tục; verify sẽ ERROR cho tới khi đọc được."), nil
	}
	if len(violations) > 0 {
		return block("Lệnh vừa chạy đã đổi file ngoài phạm vi của task: " + strings.Join(limit(violations, 10), ", ") +
			". Hãy hoàn tác các thay đổi đó (verify sẽ FAIL cho tới khi hoàn tác). Phạm vi cho phép: " + strings.Join(t.AllowedPaths, ", ") + "."), nil
	}
	return s.intelPostTool(ctx, c, cand) // spec S2 C.2: a Bash write before the understanding
}

// snapshot reads the working tree of the task's repo once, inside the time a
// hook may take: the candidate hash and the files changed since the base.
func (s *Server) snapshot(ctx context.Context, t work.Task) (candidate string, changed []string, err error) {
	tree, err := s.treeOf(ctx, t)
	if err != nil {
		return "", nil, err
	}
	gctx, cancel := boundedGit(ctx)
	defer cancel()
	return verify.Snapshot(gctx, tree)
}

// failingLeases holds the leases whose last post-tool snapshot failed, so that
// SNAPSHOT_FAILED is journaled once per run of consecutive failures
// (AC-S2-051). It lives in process memory: after a restart of harnessd a run
// that is still failing is journaled once more.
var failingLeases sync.Map // lease id -> struct{}

// snapshotFailing records the outcome of a snapshot for a lease and reports
// whether this failure is the first of its run.
func snapshotFailing(leaseID string, failed bool) (first bool) {
	if !failed {
		failingLeases.Delete(leaseID)
		return false
	}
	_, already := failingLeases.LoadOrStore(leaseID, struct{}{})
	return !already
}

// snapshotReason is the cause of a failed snapshot, short enough for a hook answer and a journal payload.
func snapshotReason(err error) string {
	msg := strings.ReplaceAll(err.Error(), "\x00", "")
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	return msg
}

// journalViolation records SCOPE_VIOLATION once per distinct set of paths of a lease.
func journalViolation(ctx context.Context, tx pgx.Tx, c hookCall, violations []string) (bool, error) {
	sum := sha256.Sum256([]byte(strings.Join(violations, "\n")))
	digest := hex.EncodeToString(sum[:8])
	var last string
	err := tx.QueryRow(ctx, `SELECT coalesce(payload->>'digest','') FROM journal
		WHERE task_id = $1 AND type = 'SCOPE_VIOLATION' AND payload->>'lease_id' = $2 ORDER BY seq DESC LIMIT 1`,
		c.held.Task.ID, c.held.Lease.ID).Scan(&last)
	if err != nil && err != pgx.ErrNoRows {
		return false, err
	}
	if last == digest {
		return false, nil
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: c.held.Task.ProjectID, TaskID: c.held.Task.ID, Type: "SCOPE_VIOLATION",
		Actor: c.actor.ID, Payload: map[string]any{"lease_id": c.held.Lease.ID, "digest": digest, "count": len(violations)}})
	return err == nil, err
}

// hookStop blocks at most once per turn (stop_hook_active) with everything
// still missing in one message (spec S2 C.2): evidence when the agent changed
// files without a PASS on the current candidate (AC-H-027), questions and
// assumptions not yet put to the owner, and a requested lesson (C.6).
func (s *Server) hookStop(ctx context.Context, c hookCall) (any, error) {
	if !c.full() || c.in.StopHookActive {
		return nil, nil
	}
	reasons, err := s.intelStop(ctx, c)
	if err != nil {
		return nil, err
	}
	evidence, err := s.stopEvidence(ctx, c)
	if err != nil {
		return nil, err
	}
	if evidence != "" {
		reasons = append([]string{evidence}, reasons...)
	}
	if len(reasons) == 0 {
		return nil, nil
	}
	return block(strings.Join(reasons, "\n")), nil
}

// stopEvidence is the S1 reason of AC-H-027, or "".
func (s *Server) stopEvidence(ctx context.Context, c hookCall) (string, error) {
	t := c.held.Task
	if !c.held.Lease.WroteFiles || len(t.RequiredChecks) == 0 || t.State != "RUNNING" {
		return "", nil
	}
	// A tree that cannot be read has no candidate, so no PASS can stand for it:
	// the agent is asked once to verify, which records the failure (AC-S2-051).
	cand, _, snapErr := s.snapshot(ctx, t)
	ids := []string{verify.ScopeCheckID}
	for _, ch := range t.RequiredChecks {
		ids = append(ids, ch.ID)
	}
	var pass bool
	if snapErr == nil {
		if err := store.InScope(ctx, s.Pool, c.actor.Scope(), func(tx pgx.Tx) (err error) {
			pass, err = verify.HasPassFor(ctx, tx, t.ID, t.Revision, cand, ids)
			return err
		}); err != nil {
			return "", err
		}
	}
	if pass {
		return "", nil
	}
	return fmt.Sprintf("Task %s có thay đổi nhưng chưa có evidence PASS trên đúng trạng thái code hiện tại. Chạy `kch verify`. "+
		"Nếu không thể hoàn thành, nêu rõ blocker và dừng — không tự báo là đã xong.", t.Key), nil
}

func limit(v []string, n int) []string {
	if len(v) <= n {
		return v
	}
	return append(append([]string{}, v[:n]...), fmt.Sprintf("… (+%d)", len(v)-n))
}
