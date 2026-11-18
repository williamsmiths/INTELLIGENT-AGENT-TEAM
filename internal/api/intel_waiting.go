package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// While a FULL task waits for a blocking question or after non-progress, the
// spec refuses writes into the repo (C.2, C.4). A Bash command carries no path
// the harness can judge, so every Bash of the holder is refused before it runs
// and a Bash already running when the wait began is blocked after it when it
// changed the tree (REV-005 #2).
//
// The hook client strips the command text but reports a lone `kch <verb>` it
// parsed (kch_verb): that command is replaced by the verb call and never runs
// as Bash, so it stays allowed while the task waits — the agent can still read
// `kch status` or answer a consult. The deny text carries what the agent waits for.

// waitingFor is the reason a FULL task waits with its mechanism on, or "".
func waitingFor(c hookCall) string {
	if !c.full() {
		return ""
	}
	t, l := c.held.Task, c.held.Lease
	switch {
	case hasReason(t, work.ReasonQuestion) && l.Intel(intel.FeatQuestions):
		return work.ReasonQuestion
	case hasReason(t, work.ReasonNonprogress) && l.Intel(intel.FeatConvergence):
		return work.ReasonNonprogress
	}
	return ""
}

// intelWaitingBash refuses a Bash of the holder of a waiting task.
func (s *Server) intelWaitingBash(ctx context.Context, c hookCall) (any, error) {
	if c.in.ToolName != "Bash" || c.in.KchVerb {
		return nil, nil
	}
	t := c.held.Task
	switch waitingFor(c) {
	case work.ReasonQuestion:
		var open []string
		if err := s.runtime(ctx, func(tx pgx.Tx) error {
			qs, err := intel.ListQuestions(ctx, tx, t.ID, nil, "OPEN")
			for _, q := range qs {
				if q.Blocking {
					open = append(open, fmt.Sprintf("#%d %s", q.Seq, intel.Clip(q.Text, 160)))
				}
			}
			return err
		}); err != nil {
			return nil, err
		}
		s.journalDenied(ctx, c, "TOOL_DENIED", "bash_question_waiting")
		return deny(fmt.Sprintf("Task %s đang chờ chủ sản phẩm trả lời câu hỏi blocking: %s. Mọi lệnh Bash bị từ chối cho tới khi có trả lời, trừ một lệnh `kch <verb>` đơn (vd. kch status); "+
			"đọc bằng Read/Grep/Glob, hoặc dừng và báo cho chủ sản phẩm.", t.Key, strings.Join(open, "; "))), nil
	case work.ReasonNonprogress:
		s.journalDenied(ctx, c, "TOOL_DENIED", "bash_nonprogress")
		return deny(fmt.Sprintf("Task %s đã dừng sau %d lần verify thất bại giống hệt nhau và chờ chủ sản phẩm (resume). Mọi lệnh Bash bị từ chối, trừ một lệnh `kch <verb>` đơn; "+
			"tóm tắt các giả thuyết đã thử và dừng.", t.Key, intel.ParkAt)), nil
	}
	return nil, nil
}

// intelWaitingPost blocks after a Bash that changed the working tree since
// the task began to wait. cand is the candidate read by the post-tool check.
func (s *Server) intelWaitingPost(ctx context.Context, c hookCall, cand string) (any, error) {
	if c.in.ToolName != "Bash" || cand == "" || waitingFor(c) == "" {
		return nil, nil
	}
	var ref string
	if err := s.runtime(ctx, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `SELECT COALESCE(payload->>'candidate', '') FROM journal WHERE task_id = $1
			AND type IN ('QUESTION_BLOCKING','NONPROGRESS_PARKED') ORDER BY seq DESC LIMIT 1`, c.held.Task.ID).Scan(&ref)
		if err == pgx.ErrNoRows {
			return nil
		}
		return err
	}); err != nil || ref == "" || ref == cand {
		return nil, err
	}
	s.journalDenied(ctx, c, "WRITE_DENIED", "bash_while_waiting")
	return block("Lệnh vừa chạy đã đổi file trong repo trong lúc task " + c.held.Task.Key +
		" chờ chủ sản phẩm. Hãy hoàn tác các thay đổi đó; không ghi vào repo cho tới khi task được tiếp tục."), nil
}

// waitMark is the candidate of the task's tree when a wait begins, recorded
// with the event that starts it; "" when there is no tree to read.
func (s *Server) waitMark(ctx context.Context, held *work.Held, starts bool) string {
	if !starts || held == nil || held.Task.BaseCommit == "" {
		return ""
	}
	cand, _, err := s.snapshot(ctx, held.Task)
	if err != nil {
		return ""
	}
	return cand
}

// asRuntime switches the open transaction to the runtime scope once the
// caller's own rows passed row-level security under its scope, so the runtime
// side of the same change commits with them (REV-005 #8).
func asRuntime(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config('kch.role', 'runtime', true), set_config('kch.principal', '', true),
		set_config('kch.projects', '', true), set_config('kch.lead_projects', '', true)`)
	return err
}
