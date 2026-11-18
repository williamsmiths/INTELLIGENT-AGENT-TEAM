package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func deny(reason string) any {
	return map[string]any{"hookSpecificOutput": map[string]any{
		"hookEventName": "PreToolUse", "permissionDecision": "deny", "permissionDecisionReason": "[kc-harness] " + reason}}
}

func block(reason string) any {
	return map[string]any{"decision": "block", "reason": "[kc-harness] " + reason}
}

// repoOwner finds the registered repo root that contains path and the project it belongs to.
func (s *Server) repoOwner(ctx context.Context, path string) (projectID, root string, err error) {
	err = s.runtime(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT project_id::text, root FROM project_repo`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pid, r string
			if err := rows.Scan(&pid, &r); err != nil {
				return err
			}
			if guard.Inside(r, path) {
				projectID, root = pid, r
			}
		}
		return rows.Err()
	})
	return projectID, root, err
}

// hookPreTool decides one tool call before it runs (spec §4 mode table, S2 A.1).
// Order: effective config, hard SYSTEM ceiling, tool allowlist, then the write
// rules of the lease mode.
func (s *Server) hookPreTool(ctx context.Context, c hookCall) (any, error) {
	if c.actor.Enforcement != "HOOK" {
		return nil, nil // SUPERVISED: observed only, never claimed as blocked (AC-H-035, L5)
	}
	if c.config != configOK { // AC-S2-005: nothing runs under a config the harness did not generate
		s.journalUnregistered(ctx, c)
		return deny("cấu hình hook/sandbox của workspace này không khớp bản đã đăng ký (" + c.config +
			"). Mọi tool bị từ chối cho tới khi chủ sản phẩm chạy lại `kch enroll`. Dừng lại và báo cho chủ sản phẩm."), nil
	}
	if c.mismatch != "" && writesTool(c.in.ToolName) && !c.in.KchVerb { // spec S2 E.9-5; a lone kch verb gets the 409 of E.9-4 (D3)
		s.journalDenied(ctx, c, "WRITE_DENIED", "conversation_mismatch")
		return deny(c.mismatch), nil
	}
	exhausted, err := s.systemExhausted(ctx, c)
	if err != nil {
		return nil, err
	}
	if exhausted { // AC-S2-008: a safety ceiling for every mode
		return deny("ngân sách hệ thống đã hết. Dừng lại, tóm tắt việc đã làm và việc còn thiếu; chỉ chủ sản phẩm nâng được hạn mức."), nil
	}
	if out, err := s.toolGuard(ctx, c); out != nil || err != nil { // AC-S2-007
		return out, err
	}
	path := c.in.writePath()
	if path != "" {
		canon, err := guard.Canonical(path, c.in.CWD)
		if err != nil {
			return deny("không xác định được đường dẫn ghi (" + err.Error() + "); dùng đường dẫn tuyệt đối."), nil // fail closed
		}
		pid, root, err := s.repoOwner(ctx, canon)
		if err != nil {
			return nil, err
		}
		// A registered repo root is writable only through a lease on that very
		// root, in every mode: one root, one responsible agent (AC-H-018, AC-H-024,
		// AC-H-081; REV-003 #4). OBSERVE and CONTEXT relax the task scope, not this.
		switch {
		case root == "":
		case c.actor.RoleIn(pid) == "":
			s.journalDenied(ctx, c, "WRITE_DENIED", "foreign_project")
			return deny("đường dẫn thuộc repo của project khác; bạn không có quyền ở đó."), nil
		case c.held == nil:
			s.journalDeniedIn(ctx, c, "WRITE_DENIED", pid, "", "no_lease")
			return deny("repo này do harness quản lý; cần được giao task (lease) trước khi ghi."), nil
		case root != c.held.Task.RepoPath:
			s.journalDenied(ctx, c, "WRITE_DENIED", "other_repo")
			return deny("đường dẫn thuộc repo khác với repo của task bạn đang giữ (" + c.held.Task.RepoPath + ")."), nil
		}
	}
	if !c.full() {
		return s.guardLooseWrite(ctx, c, path), nil // OBSERVE, CONTEXT and no lease: no task scope, but still an allowlist
	}
	var state string
	if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		state, err = work.EffectiveBudgetState(ctx, tx, c.held.Task.ID)
		return err
	}); err != nil {
		return nil, err
	}
	if state == "EXHAUSTED" { // AC-H-015
		return deny("đã hết ngân sách (task, project hoặc hệ thống). Dừng lại, tóm tắt việc đã làm và việc còn thiếu; chỉ chủ sản phẩm nâng được hạn mức."), nil
	}
	if out, err := s.intelWaitingBash(ctx, c); out != nil || err != nil { // spec S2 C.2, C.4: no Bash while waiting (REV-005 #2)
		return out, err
	}
	s.intelMark(ctx, c) // spec S2 C.2, C.5: the tree before the first tool of the lease runs
	if path == "" {
		return nil, nil // Bash and tools without a path: judged after the call (AC-H-109, L4)
	}
	if !c.held.Lease.ContextAcked { // feedback or a new lease not yet seen: steer at the next write (AC-H-050)
		var notice string
		if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
			if notice, err = work.AckContext(ctx, tx, c.held.Lease.ID); err != nil {
				return err
			}
			_, err = work.TakeWriteNotice(ctx, tx, c.held.Lease.ID) // AC-S2-039: refused once, not twice
			return err
		}); err != nil {
			return nil, err
		}
		if notice == "" {
			notice = "task vừa được giao hoặc context vừa đổi"
		}
		return deny(fmt.Sprintf("Context của task %s đã đổi (rev %d): %s\nChạy `kch context` để đọc bản mới rồi tiếp tục; lần ghi kế tiếp sẽ được xét bình thường.",
			c.held.Task.Key, c.held.Task.Revision, notice)), nil
	}
	if out, err := s.intelPreTool(ctx, c, path); out != nil || err != nil { // spec S2 C.2, C.4, AC-S2-039
		return out, err
	}
	d := guard.Rules{Repo: c.held.Task.RepoPath, Allowed: c.held.Task.AllowedPaths,
		Protected: c.held.Task.ProtectedPaths, ExtraRoots: s.writeRoots(c.actor)}.CheckWrite(path, c.in.CWD)
	if !d.Allow {
		s.journalDenied(ctx, c, "WRITE_DENIED", d.Rule)
		return deny(d.Reason + " Phạm vi của task: " + strings.Join(c.held.Task.AllowedPaths, ", ") + "."), nil
	}
	return nil, nil
}

// writesTool: the tools that write (E.9-5); Bash is judged by its effects, so it counts.
func writesTool(name string) bool {
	switch name {
	case "Write", "Edit", "MultiEdit", "NotebookEdit", "Bash":
		return true
	}
	return false
}

// systemExhausted reads the hard ceiling of AC-S2-008 for a lease holder.
func (s *Server) systemExhausted(ctx context.Context, c hookCall) (bool, error) {
	if c.held == nil {
		return false, nil // usage outside a lease is charged to no budget (spec §0)
	}
	var state string
	err := s.runtime(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM budget WHERE level = 'SYSTEM'`).Scan(&state)
	})
	return state == "EXHAUSTED", err
}

// journalDenied records a refused tool call of a lease holder (or, without a
// lease, a system event) with ids and a rule code only.
func (s *Server) journalDenied(ctx context.Context, c hookCall, typ, rule string) {
	s.journalDeniedIn(ctx, c, typ, heldProject(c), heldTask(c), rule)
}

func (s *Server) journalDeniedIn(ctx context.Context, c hookCall, typ, projectID, taskID, rule string) {
	payload := map[string]any{"rule": rule, "tool": toolLabel(c.in.ToolName)}
	if c.held != nil {
		payload["lease_id"] = c.held.Lease.ID
	}
	if projectID == "" {
		payload["principal_id"] = c.actor.ID // a system event has no actor column an agent could read
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: projectID, TaskID: taskID, Type: typ, Actor: c.actor.ID, Payload: payload})
		return err
	})
}

// unregistered limits the journal of hook calls from a workspace without a registered install.
var unregistered rateLimit

// journalUnregistered records a hook call of a HOOK principal that has no
// registered install; a tampered one is journaled by hookinstall.Observe.
func (s *Server) journalUnregistered(ctx context.Context, c hookCall) {
	if c.config != configUnregistered || !unregistered.allow(c.actor.ID) {
		return
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{Type: hookinstall.EventTampered,
			Payload: map[string]any{"principal_id": c.actor.ID, "reason": "unregistered"}})
		return err
	})
}

// toolLabel keeps a tool name short and printable for the journal.
func toolLabel(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if len(name) > 80 {
		name = name[:80]
	}
	return name
}

func heldProject(c hookCall) string {
	if c.held == nil {
		return ""
	}
	return c.held.Task.ProjectID
}

func heldTask(c hookCall) string {
	if c.held == nil {
		return ""
	}
	return c.held.Task.ID
}
