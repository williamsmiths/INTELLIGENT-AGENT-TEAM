package api

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// answerQuestion serves POST /v1/questions/{id}/answer {option|text} (PM-S2-12,
// T-S2-02, AC-S2-032). The answer becomes an AUTHORITATIVE decision linked to
// the question. An answer other than the default the agent went on with is a
// feedback, exactly the S1 path (revision + 1, lease replaced with the notice,
// T-H-04 when VERIFIED); the same answer only confirms. When no blocking
// question is left, the reason "question" leaves the task's set.
func (s *Server) answerQuestion(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Option string `json:"option"`
		Text   string `json:"text"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	answer := strings.TrimSpace(in.Option + in.Text)
	if (in.Option == "") == (in.Text == "") || answer == "" {
		writeErr(w, r, kerr.New(kerr.Invalid, "give exactly one of option or text"))
		return
	}
	ctx, actor := r.Context(), principalOf(r)
	var q intel.QuestionRow
	var ch work.Change
	var differs bool
	var state string
	err := s.scoped(r, func(tx pgx.Tx) error {
		var err error
		if q, err = intel.GetQuestion(ctx, tx, r.PathValue("id"), true); err != nil {
			return err
		}
		if q.Status != "OPEN" {
			return kerr.New(kerr.Conflict, "question is already %s", q.Status)
		}
		if in.Option != "" && !slices.Contains(q.Options, in.Option) {
			return kerr.New(kerr.Invalid, "option is not one of the question's options")
		}
		tv, err := work.GetTask(ctx, tx, actor, q.TaskID)
		if err != nil {
			return err
		}
		t := tv.Task
		differs = !strings.EqualFold(strings.Join(strings.Fields(answer), " "), strings.Join(strings.Fields(q.Default), " "))
		decision, err := storeDecision(ctx, tx, actor, t, q, answer)
		if err != nil {
			return err
		}
		if err := intel.MarkAnswered(ctx, tx, q.ID, answer, actor.ID, differs, decision); err != nil {
			return err
		}
		if differs {
			notice := fmt.Sprintf("Chủ sản phẩm trả lời câu hỏi #%d %q: %s (khác mặc định %q bạn đã dùng).", q.Seq, intel.Clip(q.Text, 200), answer, q.Default)
			if ch, err = work.Feedback(ctx, tx, actor, t.ID, notice); err != nil {
				return err
			}
			if err := s.feedbackImpact(ctx, tx, actor, &ch, notice); err != nil {
				return err
			}
		}
		if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "QUESTION_ANSWERED", Actor: actor.ID,
			Payload: map[string]any{"question_id": q.ID, "differs": differs, "decision_id": decision}}); err != nil {
			return err
		}
		if n, err := intel.OpenBlocking(ctx, tx, t.ID); err != nil || n > 0 {
			return err
		}
		state, _, err = work.RemoveWaitingReason(ctx, tx, t.ID, work.ReasonQuestion)
		return err
	})
	if err == nil {
		if ch.Granted != nil {
			s.notifyChanges(ctx, "chủ sản phẩm trả lời câu hỏi của bạn khác mặc định", ch)
		} else {
			s.tellAgent(ctx, q.AskedBy, q.ProjectID, q.TaskID, fmt.Sprintf("[kc-harness] Chủ sản phẩm trả lời câu hỏi #%d: %s (trùng mặc định; đi tiếp như đã làm).", q.Seq, answer))
		}
	}
	s.reply(w, r, http.StatusOK, map[string]any{"question_id": q.ID, "differs": differs, "task_state": state, "change": ch}, err)
}

// storeDecision keeps the owner's answer as AUTHORITATIVE decision knowledge,
// with the verbatim answer as its Source (C.2).
func storeDecision(ctx context.Context, tx pgx.Tx, owner authn.Principal, t work.Task, q intel.QuestionRow, answer string) (string, error) {
	src, err := memory.CreateSource(ctx, tx, owner, memory.SourceInput{ProjectID: t.ProjectID, Kind: "answer", Content: answer})
	if err != nil {
		return "", err
	}
	res, err := memory.CreateKnowledge(ctx, tx, owner, memory.CreateInput{Level: "PROJECT", ProjectID: t.ProjectID,
		Key: fmt.Sprintf("DEC-%s-Q%d", t.Key, q.Seq), Kind: "decision", Content: memory.Content{Aspect: "TO_BE", Trust: "AUTHORITATIVE",
			Scope: t.Scope, Title: intel.Clip(q.Text, 120), Body: "Câu hỏi: " + q.Text + "\nTrả lời của chủ sản phẩm: " + answer,
			SourceIDs: []string{src.ID}}})
	return res.Item.ID, err
}

// feedbackImpact adds to the notice of the replaced lease the items of the
// project related to the owner's words and the pinned keys that stay (C.5).
// It uses memory's search (all its stages, semantic included), top 5.
func (s *Server) feedbackImpact(ctx context.Context, tx pgx.Tx, owner authn.Principal, ch *work.Change, text string) error {
	if ch.Granted == nil {
		return nil
	}
	hits, _, err := memory.SearchWithInfo(ctx, tx, owner, memory.SearchQuery{ProjectID: ch.Task.ProjectID, Q: text, Level: "PROJECT", Limit: 5})
	if err != nil {
		return err
	}
	// Without the semantic stage a long verbatim text rarely matches as a whole:
	// keys the owner names in it are looked up too (key stage), up to 5 items.
	for _, k := range keyRe.FindAllString(text, 10) {
		if len(hits) >= 5 || slices.ContainsFunc(hits, func(h memory.Hit) bool { return h.Item.Key == k }) {
			continue
		}
		more, _, err := memory.SearchWithInfo(ctx, tx, owner, memory.SearchQuery{ProjectID: ch.Task.ProjectID, Q: k, Level: "PROJECT", Limit: 1})
		if err != nil {
			return err
		}
		if len(more) == 1 && more[0].Item.Key == k {
			hits = append(hits, more[0])
		}
	}
	var related, keys []string
	for _, h := range hits {
		if h.Item.ProjectID == ch.Task.ProjectID {
			related = append(related, fmt.Sprintf("%s (%s)", h.Item.Key, intel.Clip(h.Item.Title, 80)))
			keys = append(keys, h.Item.Key)
		}
	}
	var keep []string
	for _, k := range pinnedKeys(ch.Task) {
		if !slices.Contains(keys, k) {
			keep = append(keep, k)
		}
	}
	note := "Có thể liên quan: " + orNone(related) + "\nGiữ nguyên: " + orNone(keep)
	ch.Granted.PendingNotice += "\n\n" + note // what the host message carries too
	return work.AppendNotice(ctx, tx, ch.Granted.ID, note)
}

// keyRe finds knowledge keys such as REQ-12 or UI-EDITOR-3 in a text.
var keyRe = regexp.MustCompile(`\b[A-Za-z][A-Za-z0-9]*(?:-[A-Za-z0-9]+)+\b`)

func orNone(v []string) string {
	if len(v) == 0 {
		return "(không có)"
	}
	return strings.Join(v, ", ")
}

// tellAgent sends a message to a principal through the host and journals the outcome.
func (s *Server) tellAgent(ctx context.Context, principalID, projectID, taskID, text string) {
	if s.Notifier == nil {
		return
	}
	var agentID string
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		p, err := work.LookupPrincipal(ctx, tx, principalID)
		agentID = p.OpenBotAgentID
		return err
	})
	if agentID == "" {
		return
	}
	hctx, cancel := context.WithTimeout(ctx, hostDeadline)
	_, err := s.Notifier.Send(hctx, agentID, text, "")
	cancel()
	typ := "NOTIFY_SENT"
	if err != nil {
		typ = "NOTIFY_FAILED"
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: projectID, TaskID: taskID, Type: typ, Payload: map[string]any{"reason": "question_answered"}})
		return err
	})
}
