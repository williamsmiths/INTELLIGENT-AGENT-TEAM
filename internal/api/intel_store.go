package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// storeAssumptions keeps each assumption as CANDIDATE decision knowledge
// ASM-<task key>-<n> in the task scope, with the declared basis (C.2, AC-S2-032).
func (s *Server) storeAssumptions(ctx context.Context, tx pgx.Tx, c *AgentCall, t work.Task, list []intel.Assumption) ([]string, error) {
	var keys []string
	if len(list) == 0 {
		return keys, nil
	}
	prefix := "ASM-" + t.Key + "-"
	var n int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(substr(key, $2)::int), 0) FROM knowledge
		WHERE project_id = $1 AND level = 'PROJECT' AND left(key, $2 - 1) = $3 AND substr(key, $2) ~ '^[0-9]{1,9}$'`,
		t.ProjectID, len([]rune(prefix))+1, prefix).Scan(&n); err != nil {
		return nil, err
	}
	for _, a := range list {
		n++
		body := a.Text
		if strings.TrimSpace(a.Evidence) != "" {
			body += "\nBằng chứng: " + a.Evidence
		}
		res, err := memory.CreateKnowledge(ctx, tx, c.Actor, memory.CreateInput{Level: "PROJECT", ProjectID: t.ProjectID,
			Key: fmt.Sprintf("%s%d", prefix, n), Kind: "decision", Content: memory.Content{Aspect: "TO_BE", Trust: "CANDIDATE",
				Basis: a.Basis, Scope: t.Scope, Title: intel.Clip(a.Text, 120), Body: body}})
		if err != nil {
			return nil, err
		}
		keys = append(keys, res.Item.Key)
	}
	return keys, nil
}

func storeQuestions(ctx context.Context, tx pgx.Tx, c *AgentCall, t work.Task, understandingID string, list []intel.Question) ([]intel.QuestionRow, error) {
	var out []intel.QuestionRow
	for _, q := range list {
		row, err := intel.InsertQuestion(ctx, tx, intel.QuestionRow{ProjectID: t.ProjectID, TaskID: t.ID, UnderstandingID: understandingID,
			AskedBy: c.Actor.ID, TaskRevision: t.Revision, Text: q.Text, Options: q.Options, Default: q.Default, Impact: q.Impact, Blocking: q.Blocking})
		if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

func describeStored(keys []string, qs []intel.QuestionRow, questionsOn bool) string {
	var b strings.Builder
	if len(keys) > 0 {
		fmt.Fprintf(&b, " Giả định đã lưu (CANDIDATE, chờ chủ sản phẩm xác nhận): %s.", strings.Join(keys, ", "))
	}
	for _, q := range qs {
		if q.Blocking && questionsOn {
			fmt.Fprintf(&b, " Câu hỏi #%d là blocking: task chờ chủ sản phẩm trả lời, không ghi vào repo cho tới lúc đó.", q.Seq)
		} else {
			fmt.Fprintf(&b, " Câu hỏi #%d: đi tiếp theo mặc định %q.", q.Seq, intel.Clip(q.Default, 80))
		}
	}
	return b.String()
}
