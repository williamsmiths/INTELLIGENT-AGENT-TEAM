package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// The hook side of spec S2 part C. Every mechanism here applies only to a
// HOOK principal holding a FULL lease with the mechanism switched on
// (c.full() and Lease.Intel); a switched-off mechanism neither blocks nor nags.

// gateOn reports whether the understanding gate applies to a lease (C.1 table).
func gateOn(l work.Lease) bool {
	return l.Intel(intel.FeatGate) && intel.NeedsUnderstanding(l.IntelMode)
}

// pendingFor reads what the task still needs before the next write.
func (s *Server) pendingFor(ctx context.Context, t work.Task) (intel.Pending, error) {
	var p intel.Pending
	err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		p, err = intel.GatePending(ctx, tx, t.ID, t.Revision)
		return err
	})
	return p, err
}

// intelMark records the working tree at the first tool call of a FULL lease,
// before the tool runs (lease.intel_mark): a Bash write before the
// understanding (C.2) and the files changed since a feedback (C.5) are told from it.
func (s *Server) intelMark(ctx context.Context, c hookCall) {
	if !c.full() || len(c.held.Lease.IntelMark) > 0 || c.held.Task.BaseCommit == "" {
		return
	}
	mark, err := s.markOf(ctx, c.held.Task)
	if err != nil {
		slog.WarnContext(ctx, "intel: working tree mark not taken", "task", c.held.Task.ID, "err", err)
		return
	}
	raw, _ := json.Marshal(mark)
	_ = s.runtime(ctx, func(tx pgx.Tx) error { return work.SetMark(ctx, tx, c.held.Lease.ID, raw) })
	c.held.Lease.IntelMark = raw
}

// markOf reads the candidate hash and the state of every changed file.
func (s *Server) markOf(ctx context.Context, t work.Task) (intel.Mark, error) {
	cand, changed, err := s.snapshot(ctx, t)
	if err != nil {
		return intel.Mark{}, err
	}
	return intel.Mark{Candidate: cand, Files: intel.FileStates(t.RepoPath, changed)}, nil
}

// intelPreTool refuses a write into the task repo while the task waits for a
// question or after non-progress (C.2, C.4), the missed feedback once
// (AC-S2-039), and any write before the understanding or the delta (AC-S2-031,
// AC-S2-035). Writes outside the repo (private temp root) are not concerned.
func (s *Server) intelPreTool(ctx context.Context, c hookCall, path string) (any, error) {
	if !c.full() || path == "" {
		return nil, nil
	}
	t, l := c.held.Task, c.held.Lease
	if canon, err := guard.Canonical(path, c.in.CWD); err != nil || !guard.Inside(t.RepoPath, canon) {
		return nil, nil // the S1 rules judge it
	}
	if hasReason(t, work.ReasonQuestion) && l.Intel(intel.FeatQuestions) {
		s.journalDenied(ctx, c, "WRITE_DENIED", "question_waiting")
		return deny(fmt.Sprintf("Task %s đang chờ chủ sản phẩm trả lời câu hỏi blocking (xem `kch questions`). Không ghi vào repo cho tới khi có trả lời; "+
			"trong lúc chờ chỉ đọc và tìm hiểu, hoặc dừng và báo cho chủ sản phẩm.", t.Key)), nil
	}
	if hasReason(t, work.ReasonNonprogress) && l.Intel(intel.FeatConvergence) {
		s.journalDenied(ctx, c, "WRITE_DENIED", "nonprogress")
		return deny(fmt.Sprintf("Task %s đã dừng sau %d lần verify thất bại giống hệt nhau và chờ chủ sản phẩm (resume). Không ghi vào repo; "+
			"tóm tắt các giả thuyết đã thử và dừng.", t.Key, intel.ParkAt)), nil
	}
	pending := intel.Pending{}
	if gateOn(l) {
		var err error
		if pending, err = s.pendingFor(ctx, t); err != nil {
			return nil, err
		}
	}
	notice := ""
	if l.WriteNotice != "" {
		if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
			notice, err = work.TakeWriteNotice(ctx, tx, l.ID)
			return err
		}); err != nil {
			return nil, err
		}
	}
	if !pending.Any() {
		if notice != "" { // AC-S2-039 DIRECT: refused once with the owner's words
			return deny(fmt.Sprintf("Context của task %s đã đổi (rev %d): %s\nChạy `kch context` để đọc bản mới rồi tiếp tục; lần ghi kế tiếp sẽ được xét bình thường.",
				t.Key, t.Revision, notice)), nil
		}
		return nil, nil
	}
	var sets intel.Sets
	if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		sets, err = intel.LoadSets(ctx, tx, intel.ProductInput{ProjectID: t.ProjectID, Scope: t.Scope, Pinned: pinnedKeys(t), TaskKey: t.Key})
		return err
	}); err != nil {
		return nil, err
	}
	s.journalDenied(ctx, c, "WRITE_DENIED", "understanding_required")
	if pending.Full {
		text := intel.GateDeny(t.Key, l.IntelMode, sets)
		if notice != "" {
			text = "Thay đổi từ chủ sản phẩm:\n" + notice + "\n\n" + text
		}
		return deny(text), nil
	}
	return deny(intel.DeltaDeny(t.Key, t.Revision, sets, notice)), nil
}

func hasReason(t work.Task, reason string) bool {
	for _, r := range t.WaitingReasons {
		if r == reason {
			return true
		}
	}
	return false
}

// intelPostTool blocks after a Bash command that changed the working tree
// while the task waits (C.2, C.4) or while the understanding is still missing
// (C.2, REV-004 #12). cand is the
// candidate read by the S1 post-tool check, "" when none was read.
func (s *Server) intelPostTool(ctx context.Context, c hookCall, cand string) (any, error) {
	if out, err := s.intelWaitingPost(ctx, c, cand); out != nil || err != nil { // REV-005 #2
		return out, err
	}
	if !c.full() || c.in.ToolName != "Bash" || cand == "" || !gateOn(c.held.Lease) || len(c.held.Lease.IntelMark) == 0 {
		return nil, nil
	}
	var mark intel.Mark
	if json.Unmarshal(c.held.Lease.IntelMark, &mark) != nil || mark.Candidate == cand {
		return nil, nil
	}
	pending, err := s.pendingFor(ctx, c.held.Task)
	if err != nil || !pending.Any() {
		return nil, err
	}
	cmd := intel.UnderstandCommand
	if !pending.Full {
		cmd = intel.DeltaCommand
	}
	return block("Lệnh vừa chạy đã ghi vào repo trước khi bản hiểu được nhận (mode " + c.held.Lease.IntelMode + "). " +
		"Hoàn tác các thay đổi đó, hoặc nộp bản hiểu bằng MỘT lệnh Bash riêng:\n" + cmd + "\n`kch verify` trả 409 cho tới khi có bản hiểu."), nil
}

// packetChars is the character budget of the packet of a FULL lease by its intel mode (C.1 table).
func (s *Server) packetChars(l *work.Lease, actorID string) int {
	if l != nil && l.Mode == "FULL" && l.PrincipalID == actorID && l.IntelMode != "" {
		return intel.PacketChars(l.IntelMode)
	}
	return s.PacketChars
}
