package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
)

// verbLesson: kch lesson <<'END' {cause, fix, applies_when, limits} END (C.6, AC-S2-036).
// The facts of the task's attempts become an immutable Source with origin
// RUNTIME; the lesson becomes a CANDIDATE experience at PROJECT level in the
// task scope, which only the owner can confirm.
func verbLesson(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	held, err := fullHolder(c)
	if err != nil {
		return AgentReply{}, err
	}
	var in intel.Lesson
	if err := c.Decode(&in); err != nil {
		return AgentReply{}, err
	}
	if err := in.Check(); err != nil {
		return AgentReply{}, err
	}
	t, l := held.Task, held.Lease
	var sourceID string
	if err := s.runtime(ctx, func(tx pgx.Tx) error {
		attempts, err := intel.ListAttempts(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		facts, err := intel.LeaseFacts(ctx, tx, l.ID, t.ID, t.Revision)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(map[string]any{"task_id": t.ID, "task_key": t.Key, "revision": t.Revision, "lease_id": l.ID,
			"lease": facts, "attempts": attempts})
		if err != nil {
			return err
		}
		sourceID, err = intel.InsertRuntimeSource(ctx, tx, t.ProjectID, c.Actor.ID, "attempt_facts", string(raw))
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	var item memory.Item
	if err := c.scoped(ctx, s, func(tx pgx.Tx) error {
		if err := intel.LockTask(ctx, tx, t.ID); err != nil {
			return err
		}
		prefix := "EXP-" + t.Key + "-"
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(DISTINCT key) FROM knowledge WHERE project_id = $1 AND level = 'PROJECT' AND left(key, $2) = $3`,
			t.ProjectID, len([]rune(prefix)), prefix).Scan(&n); err != nil {
			return err
		}
		res, err := memory.CreateKnowledge(ctx, tx, c.Actor, memory.CreateInput{Level: "PROJECT", ProjectID: t.ProjectID,
			Key: fmt.Sprintf("%s%d", prefix, n+1), Kind: "experience", Content: memory.Content{Aspect: "AS_IS", Trust: "CANDIDATE",
				Basis: "inferred", Scope: t.Scope, Title: "Bài học: " + intel.Clip(in.Cause, 100),
				Body: "Nguyên nhân: " + in.Cause + "\nCách sửa: " + in.Fix, AppliesWhen: in.AppliesWhen, Limits: in.Limits,
				SourceIDs: []string{sourceID}}})
		item = res.Item
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	if err := s.runtime(ctx, func(tx pgx.Tx) error {
		_, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "LESSON_RECORDED", Actor: c.Actor.ID,
			Payload: map[string]any{"lease_id": l.ID, "knowledge_id": item.ID, "source_id": sourceID}})
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	return AgentReply{Status: http.StatusCreated,
		Text: fmt.Sprintf("Đã lưu bài học %s (CANDIDATE, phạm vi %s): chủ sản phẩm xác nhận thì các task cùng phạm vi sẽ nhận được.\n", item.Key, t.Scope),
		Data: map[string]any{"id": item.ID, "key": item.Key, "source_id": sourceID}}, nil
}
