package api

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// verbAnswer: kch answer <consult id> <<'END' … END [--refs k1,k2] (C.3, PM-S2-11).
// Taken only from the selected answerer; the project comes from the consult
// row, never from a lease. Anyone else gets 404, a late answer 409, and so
// does an answerer who took a lease in another project since (REV-005 #11).
// Only --refs that are current knowledge keys of the consult's project are
// kept (REV-005 #6). The host is told within hostDeadline; the turn that
// handled the consult is resolved by the worker (REV-005 #4).
func verbAnswer(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	words, refKeys := splitRefs(c.Args)
	if len(words) < 1 {
		return AgentReply{}, kerr.New(kerr.Invalid, "cách dùng: kch answer <mã tham vấn> <<'END'\n<câu trả lời>\nEND")
	}
	answer := strings.TrimSpace(strings.Join(words[1:], " ") + "\n" + c.Stdin)
	var row *intel.ConsultRow
	var refs []intel.ConsultRef
	var expired, busy bool
	err := s.runtime(ctx, func(tx pgx.Tx) error {
		var err error
		if row, err = intel.GetConsult(ctx, tx, words[0], false); err != nil || row == nil || row.Answerer != c.Actor.ID {
			if err == nil {
				err = kerr.New(kerr.NotFound, "consult not found")
			}
			return err
		}
		if row.Status == intel.ConsultOpen {
			gone, err := intel.ExpireDue(ctx, tx, row.ID)
			if expired = len(gone) > 0; expired {
				*row = gone[0]
			}
			if err != nil {
				return err
			}
		}
		if refs, err = consultRefs(ctx, tx, row.ProjectID, refKeys); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lease WHERE principal_id = $1 AND state = 'ACTIVE'
			AND expires_at >= now() AND project_id <> $2)`, c.Actor.ID, row.ProjectID).Scan(&busy)
	})
	if err != nil {
		return AgentReply{}, err
	}
	if expired {
		s.consultExpired(ctx, *row)
		return AgentReply{}, kerr.New(kerr.Conflict, "tham vấn %s đã hết hạn; câu trả lời muộn không được nhận", row.ID)
	}
	if row.Status != intel.ConsultOpen {
		return AgentReply{}, kerr.New(kerr.Conflict, "tham vấn %s đã %s; không nhận thêm câu trả lời", row.ID, row.Status)
	}
	if busy { // C.3: an answerer holds no lease in another project, at the answer too
		return AgentReply{}, kerr.New(kerr.Conflict, "bạn đang giữ lease ở một project khác; tham vấn %s không trả lời được nữa", row.ID)
	}
	keys := make([]string, 0, len(refs))
	for _, r := range refs {
		keys = append(keys, r.Key)
	}
	if answer == "" || utf8.RuneCountInString(answer) > intel.MaxAnswerChars {
		return AgentReply{}, kerr.New(kerr.Invalid, "câu trả lời cần 1–%d ký tự", intel.MaxAnswerChars)
	}
	var entry memory.BlackboardEntry // a finding of the consult's project, written as the answerer
	if err := c.scoped(ctx, s, func(tx pgx.Tx) (err error) {
		entry, err = memory.PostBlackboard(ctx, tx, c.Actor, memory.BlackboardInput{ProjectID: row.ProjectID, TaskID: row.TaskID,
			Kind: "finding", Body: "Trả lời tham vấn " + row.ID + ":\n" + answer, Refs: keys})
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	if err := s.runtime(ctx, func(tx pgx.Tx) error {
		if err := intel.SetAnswer(ctx, tx, row.ID, answer, refs, entry.ID); err != nil {
			return err
		}
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: row.ProjectID, TaskID: row.TaskID, Type: "CONSULT_ANSWERED",
			Actor: c.Actor.ID, Payload: map[string]any{"consult_id": row.ID, "finding_id": entry.ID}})
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	s.tellAsker(ctx, *row, intel.AnswerBlock(row.ID, c.Actor.Name, answer, refs), "answer")
	return AgentReply{Status: 201, Text: fmt.Sprintf("Đã gửi câu trả lời cho tham vấn %s (ghi thành finding #%d trên blackboard của project).\n", row.ID, entry.ID),
		Data: map[string]any{"consult_id": row.ID, "finding_id": entry.ID}}, nil
}

// consultExpired tells the asker that a consult ended without an answer and
// records it: EXPIRED (sent, not answered in time) or FAILED (never sent; its
// allowance is back, REV-005 #4).
func (s *Server) consultExpired(ctx context.Context, row intel.ConsultRow) {
	typ, text, what := "CONSULT_EXPIRED", intel.ExpiredBlock(row.ID), "expired"
	if row.Status == intel.ConsultFailed {
		typ, text, what = "CONSULT_FAILED", intel.FailedBlock(row.ID), "failed"
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: row.ProjectID, TaskID: row.TaskID, Type: typ,
			Payload: map[string]any{"consult_id": row.ID, "send_attempts": row.Attempts}})
		return err
	})
	s.tellAsker(ctx, row, text, what)
}

// tellAsker sends the outcome of a consult to the asker through the host; the
// next hook and the next verb of the asker carry it too (see intelInbox).
func (s *Server) tellAsker(ctx context.Context, row intel.ConsultRow, text, what string) {
	if s.Notifier == nil {
		return
	}
	var agentID string
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		p, err := work.LookupPrincipal(ctx, tx, row.Asker)
		agentID = p.OpenBotAgentID
		return err
	})
	if agentID == "" {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, hostDeadline)
	_, err := s.Notifier.Send(hctx, agentID, "[kc-harness] "+text, "kch-consult-"+what+"-"+row.ID)
	cancel()
	typ := "NOTIFY_SENT"
	if err != nil {
		typ = "NOTIFY_FAILED"
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: row.ProjectID, TaskID: row.TaskID, Type: typ,
			Payload: map[string]any{"consult_id": row.ID, "reason": "consult_" + what}})
		return err
	})
}

// resolveTurn asks the host which turn handled the consult message and moves
// the usage of exactly that turn to the asker's task (C.7, AC-S2-037). Only
// the worker calls it: the turn is not urgent for anyone waiting (REV-005 #4).
func (s *Server) resolveTurn(ctx context.Context, id string) {
	if s.Notifier == nil {
		return
	}
	var row *intel.ConsultRow
	var agentID string
	_ = s.runtime(ctx, func(tx pgx.Tx) (err error) {
		if row, err = intel.GetConsult(ctx, tx, id, false); err != nil || row == nil {
			return err
		}
		p, err := work.LookupPrincipal(ctx, tx, row.Answerer)
		agentID = p.OpenBotAgentID
		return err
	})
	if row == nil || row.DeliveryID == "" || row.TurnID != "" || agentID == "" {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, hostDeadline)
	_, turn, err := s.Notifier.Delivery(hctx, agentID, row.DeliveryID)
	cancel()
	if err != nil || turn == "" {
		return
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		if set, err := intel.SetTurn(ctx, tx, row.ID, turn); err != nil || !set {
			return err
		}
		_, err := work.AttributeCollab(ctx, tx, row.ID)
		return err
	})
}

// SweepIntel is the periodic worker of part C (cmd/harnessd): QUESTION_WAITING
// for blocking questions open longer than intel.WaitingAfter, the end of
// consults past their deadline (EXPIRED, or FAILED when never sent), the
// retries of consult messages the host did not take, and the host turn of
// delivered consults.
func SweepIntel(ctx context.Context, pool *pgxpool.Pool, n Notifier) error {
	s := &Server{Pool: pool, Notifier: n}
	var expired, due, unresolved []intel.ConsultRow
	if err := s.runtime(ctx, func(tx pgx.Tx) error {
		if _, err := intel.JournalWaitingQuestions(ctx, tx, intel.WaitingAfter); err != nil {
			return err
		}
		var err error
		if expired, err = intel.ExpireDue(ctx, tx, ""); err != nil {
			return err
		}
		if due, err = intel.DueSends(ctx, tx); err != nil {
			return err
		}
		unresolved, err = intel.Unresolved(ctx, tx)
		return err
	}); err != nil {
		return err
	}
	for _, row := range expired {
		s.consultExpired(ctx, row)
	}
	for _, row := range due {
		s.sendConsult(ctx, row)
	}
	for _, row := range unresolved {
		s.resolveTurn(ctx, row.ID)
	}
	return nil
}
