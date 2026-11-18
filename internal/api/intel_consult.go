package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// verbConsult: kch consult <specialty> <câu hỏi> [--refs k1,k2] (C.3, AC-S2-033).
// Only the holder of a FULL lease with consult switched on; allowance by mode
// (+1 once per task from convergence); the same question returns the earlier
// consult without a new message; the answerer is chosen by the runtime.
func verbConsult(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	held, err := fullHolder(c)
	if err != nil {
		return AgentReply{}, err
	}
	if !held.Lease.Intel(intel.FeatConsult) {
		return AgentReply{}, kerr.New(kerr.Conflict, "tham vấn không bật cho lease này")
	}
	words, refKeys := splitRefs(c.Args)
	if len(words) < 1 || !intel.ValidSpecialty(words[0]) {
		return AgentReply{}, kerr.New(kerr.Invalid, "cách dùng: kch consult <product|engineering|design|quality> <câu hỏi> [--refs KEY1,KEY2]")
	}
	specialty, question := words[0], strings.TrimSpace(strings.Join(words[1:], " ")+"\n"+c.Stdin)
	if question == "" || utf8.RuneCountInString(question) > intel.MaxConsultChars {
		return AgentReply{}, kerr.New(kerr.Invalid, "câu hỏi tham vấn cần 1–%d ký tự", intel.MaxConsultChars)
	}
	t, l := held.Task, held.Lease
	var row intel.ConsultRow
	var dup bool
	err = s.runtime(ctx, func(tx pgx.Tx) error {
		if err := intel.LockTask(ctx, tx, t.ID); err != nil {
			return err
		}
		hash := intel.QuestionHash(specialty, question)
		if old, err := intel.FindDuplicate(ctx, tx, t.ID, hash); err != nil || old != nil {
			if old != nil {
				row, dup = *old, true
			}
			return err
		}
		allowed, used, err := intel.ConsultBudget(ctx, tx, t.ID, l.IntelMode)
		if err != nil {
			return err
		}
		if used >= allowed {
			return kerr.New(kerr.Conflict, "hết lượt tham vấn của task (mode %s: %d lượt, đã dùng %d)", l.IntelMode, allowed, used)
		}
		answerer, _, _, err := intel.Answerer(ctx, tx, t.ProjectID, specialty, c.Actor.ID)
		if err != nil {
			return err
		}
		refs, err := consultRefs(ctx, tx, t.ProjectID, refKeys)
		if err != nil {
			return err
		}
		if row, err = intel.InsertConsult(ctx, tx, intel.ConsultRow{ProjectID: t.ProjectID, TaskID: t.ID, LeaseID: l.ID, Asker: c.Actor.ID,
			Answerer: answerer, Specialty: specialty, Question: question, Hash: hash, Refs: refs}); err != nil {
			return err
		}
		_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "CONSULT_OPENED", Actor: c.Actor.ID,
			Payload: map[string]any{"consult_id": row.ID, "answerer_id": answerer, "specialty": specialty, "lease_id": l.ID}})
		return err
	})
	if err != nil {
		return AgentReply{}, err
	}
	data := map[string]any{"id": row.ID, "status": row.Status, "duplicate": dup}
	if dup { // AC-S2-033: the earlier consult, no new message
		text := fmt.Sprintf("Câu hỏi này đã được gửi trước đó (tham vấn %s, trạng thái %s); không gửi lại.", row.ID, row.Status)
		if row.Status == "ANSWERED" {
			text += "\n" + intel.AnswerBlock(row.ID, "người được hỏi", row.Answer, row.AnswerRefs)
		}
		return AgentReply{Text: text + "\n", Data: data}, nil
	}
	text := fmt.Sprintf("Đã gửi tham vấn %s (chuyên môn %s). Hạn %d phút; câu trả lời đến ở lượt kế tiếp.", row.ID, specialty, int(intel.ConsultTTL.Minutes()))
	if !s.sendConsult(ctx, row) { // C.3: PENDING_SEND, retried by the worker; FAILED gives the allowance back
		data["status"] = intel.ConsultPending
		text = fmt.Sprintf("Tham vấn %s (chuyên môn %s) CHƯA gửi được qua OpenBot; harness tự thử lại cho tới hạn %d phút. "+
			"Không gửi được thì tham vấn thành FAILED và lượt được hoàn lại. Trong lúc chờ, đi tiếp bằng giả định của bạn.",
			row.ID, specialty, int(intel.ConsultTTL.Minutes()))
	} else {
		data["status"] = intel.ConsultOpen
	}
	return AgentReply{Status: http.StatusCreated, Text: text + "\n", Data: data}, nil
}

// hostDeadline bounds every host call of part C, retries included, so an
// agent's request never waits long on a slow or rate-limited host; what does
// not fit is the minute worker's job (REV-005 #4).
const hostDeadline = 5 * time.Second

// sendConsult sends the message of a PENDING_SEND consult to its answerer
// within hostDeadline and records the outcome: OPEN with the delivery id, or
// one more failed attempt with the next retry scheduled. Both are journaled.
func (s *Server) sendConsult(ctx context.Context, row intel.ConsultRow) bool {
	var agentID, askerName, taskKey string
	if err := s.runtime(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(a.openbot_agent_id, ''), q.name, t.key FROM principal a, principal q, task t
			WHERE a.id = $1 AND q.id = $2 AND t.id = $3`, row.Answerer, row.Asker, row.TaskID).Scan(&agentID, &askerName, &taskKey)
	}); err != nil {
		return false
	}
	var id string
	err := fmt.Errorf("no notifier")
	if s.Notifier != nil && agentID != "" {
		hctx, cancel := context.WithTimeout(ctx, hostDeadline)
		id, err = s.Notifier.Send(hctx, agentID, intel.ConsultMessage(row.ID, row.Specialty, askerName, taskKey, row.Question, row.Refs),
			"kch-consult-"+row.ID)
		cancel()
	}
	typ := "NOTIFY_SENT"
	if err != nil {
		typ = "NOTIFY_FAILED"
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		var mark error
		if err == nil {
			mark = intel.MarkSent(ctx, tx, row.ID, id)
		} else {
			mark = intel.MarkSendFailed(ctx, tx, row.ID)
		}
		if mark != nil {
			return mark
		}
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: row.ProjectID, TaskID: row.TaskID, Type: typ,
			Payload: map[string]any{"consult_id": row.ID, "reason": "consult", "attempt": row.Attempts + 1}})
		return err
	})
	return err == nil
}

// consultRefs reads key, title and trust of the referenced current PROJECT items; unknown keys are left out.
func consultRefs(ctx context.Context, tx pgx.Tx, projectID string, keys []string) ([]intel.ConsultRef, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT key, title, trust FROM knowledge WHERE project_id = $1 AND level = 'PROJECT' AND is_current
		AND key = ANY($2::text[]) ORDER BY key LIMIT 10`, projectID, keys)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[intel.ConsultRef])
}

// splitRefs separates "--refs a,b" (or --refs=a,b) from the other words.
func splitRefs(args []string) (words, refs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--refs" && i+1 < len(args):
			refs = append(refs, strings.Split(args[i+1], ",")...)
			i++
		case strings.HasPrefix(a, "--refs="):
			refs = append(refs, strings.Split(strings.TrimPrefix(a, "--refs="), ",")...)
		default:
			words = append(words, a)
		}
	}
	out := refs[:0]
	for _, r := range refs {
		if r = strings.TrimSpace(r); r != "" {
			out = append(out, r)
		}
	}
	return words, out
}
