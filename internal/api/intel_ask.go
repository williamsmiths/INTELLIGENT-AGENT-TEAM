package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// verbAsk: kch ask <<'END' {"text","options","default","impact","blocking"} END (C.2, AC-S2-032).
func verbAsk(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	held, err := fullHolder(c)
	if err != nil {
		return AgentReply{}, err
	}
	var q intel.Question
	if err := c.Decode(&q); err != nil {
		return AgentReply{}, err
	}
	if err := q.CheckShape(); err != nil {
		return AgentReply{}, err
	}
	if !q.HasOptions() { // U4
		return AgentReply{}, kerr.New(kerr.Invalid, "câu hỏi cần ≥ 2 options và default là một trong các option")
	}
	t := held.Task
	cand := s.waitMark(ctx, held, q.Blocking && held.Lease.Intel(intel.FeatQuestions)) // the tree when a wait begins (REV-005 #2)
	var rows []intel.QuestionRow
	if err := c.scoped(ctx, s, func(tx pgx.Tx) error {
		if err := intel.LockTask(ctx, tx, t.ID); err != nil {
			return err
		}
		if rows, err = storeQuestions(ctx, tx, c, t, "", []intel.Question{q}); err != nil || !q.Blocking {
			return err
		}
		// The question and the wait it causes commit together (REV-005 #8).
		if err := asRuntime(ctx, tx); err != nil {
			return err
		}
		return s.blockOnQuestions(ctx, tx, c, held, []string{rows[0].ID}, cand)
	}); err != nil {
		return AgentReply{}, err
	}
	return AgentReply{Status: http.StatusCreated, Text: "Đã gửi câu hỏi cho chủ sản phẩm." + describeStored(nil, rows, held.Lease.Intel(intel.FeatQuestions)) + "\n",
		Data: map[string]any{"id": rows[0].ID, "seq": rows[0].Seq}}, nil
}

// verbQuestions lists the questions of the caller's task and their answers.
func verbQuestions(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	t, err := c.Task()
	if err != nil {
		return AgentReply{}, err
	}
	var rows []intel.QuestionRow
	if err := c.scoped(ctx, s, func(tx pgx.Tx) (err error) {
		rows, err = intel.ListQuestions(ctx, tx, t.ID, nil, "")
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	if len(rows) == 0 {
		return AgentReply{Text: "Task chưa có câu hỏi nào.\n", Data: map[string]any{"count": 0}}, nil
	}
	var b strings.Builder
	for _, q := range rows {
		fmt.Fprintf(&b, "#%d [%s", q.Seq, q.Status)
		if q.Blocking {
			b.WriteString(", blocking")
		}
		fmt.Fprintf(&b, "] %s — mặc định %q", intel.Clip(q.Text, 200), intel.Clip(q.Default, 80))
		if q.Status == "ANSWERED" {
			fmt.Fprintf(&b, " — chủ sản phẩm trả lời: %s", intel.Clip(q.Answer, 400))
		}
		b.WriteString("\n")
	}
	return AgentReply{Text: b.String(), Data: map[string]any{"count": len(rows)}}, nil
}
