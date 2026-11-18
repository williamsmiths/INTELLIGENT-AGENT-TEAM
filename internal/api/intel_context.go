package api

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// maxExperiences is the number of confirmed experiences added to a packet (C.6).
const maxExperiences = 3

// experiencesBlock lists up to 3 AUTHORITATIVE PROJECT experiences of the
// task's scope, those matching the task title and desired outcome first
// (C.6, AC-S2-036). It reads through memory's public search under the
// recipient's own scope, so only the lease's project can appear; items the
// packet already shows are skipped.
func experiencesBlock(ctx context.Context, tx pgx.Tx, actor authn.Principal, t work.Task, p memory.Packet) (string, error) {
	if strings.TrimSpace(t.Scope) == "" {
		return "", nil
	}
	shown := map[string]bool{}
	for _, r := range p.Refs {
		shown[r.KnowledgeID] = true
	}
	var picked []memory.Item
	for _, q := range []string{t.Title + " " + t.DesiredOutcome, ""} { // matches first, then the rest of the scope
		hits, _, err := memory.SearchWithInfo(ctx, tx, actor, memory.SearchQuery{ProjectID: t.ProjectID, Q: q, Kind: "experience",
			Trust: "AUTHORITATIVE", Level: "PROJECT", Limit: 50})
		if err != nil {
			return "", err
		}
		for _, h := range hits {
			it := h.Item
			if len(picked) == maxExperiences || it.ProjectID != t.ProjectID || it.Scope != t.Scope || shown[it.ID] {
				continue
			}
			shown[it.ID] = true
			picked = append(picked, it)
		}
	}
	if len(picked) == 0 {
		return "", nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Kinh nghiệm đã được chủ sản phẩm xác nhận cho phạm vi %s:\n", intel.Clip(t.Scope, 80))
	for _, it := range picked {
		fmt.Fprintf(&b, "- %s: %s — %s Áp dụng khi: %s. Giới hạn: %s.\n", it.Key, intel.Clip(it.Title, 120), intel.Clip(it.Body, 400),
			intel.Clip(it.AppliesWhen, 200), intel.Clip(it.Limits, 200))
	}
	return b.String(), nil
}

// intelInbox is what waits for the holder of a FULL lease: the steer text of
// convergence (C.4) and the outcome of its consults (C.3). It is delivered
// once, by the next injected context or the next agent verb.
func (s *Server) intelInbox(ctx context.Context, actor authn.Principal, held *work.Held, steer bool) string {
	if held == nil || held.Lease.Mode != "FULL" {
		return ""
	}
	var parts []string
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		if steer && held.Lease.IntelInbox != "" {
			text, err := work.PopInbox(ctx, tx, held.Lease.ID)
			if err != nil {
				return err
			}
			if text != "" {
				parts = append(parts, text)
			}
		}
		rows, err := intel.Outcomes(ctx, tx, actor.ID, held.Task.ID)
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, r := range rows {
			switch r.Status {
			case intel.ConsultExpired:
				parts = append(parts, intel.ExpiredBlock(r.ID))
				continue
			case intel.ConsultFailed:
				parts = append(parts, intel.FailedBlock(r.ID))
				continue
			}
			if _, ok := names[r.Answerer]; !ok {
				p, _ := work.LookupPrincipal(ctx, tx, r.Answerer)
				names[r.Answerer] = p.Name
			}
			parts = append(parts, intel.AnswerBlock(r.ID, names[r.Answerer], r.Answer, r.AnswerRefs))
		}
		return nil
	})
	if len(parts) == 0 {
		return ""
	}
	return "[kc-harness] " + strings.Join(parts, "\n\n") + "\n"
}

// intelCommands names the part C verbs a FULL lease may use.
func intelCommands(held *work.Held) string {
	if held == nil || held.Lease.Mode != "FULL" {
		return ""
	}
	l := held.Lease
	var cmds []string
	if gateOn(l) {
		cmds = append(cmds, "`kch understand <<'END' {json} END` (bản hiểu, bắt buộc trước lần ghi đầu ở mode "+l.IntelMode+")")
	}
	if l.Intel(intel.FeatQuestions) {
		cmds = append(cmds, "`kch ask <<'END' {json} END` (hỏi chủ sản phẩm), `kch questions`")
	}
	if l.Intel(intel.FeatConsult) && intel.ConsultAllowance(l.IntelMode) > 0 {
		cmds = append(cmds, fmt.Sprintf("`kch consult <chuyên môn> <câu hỏi>` (tối đa %d lượt)", intel.ConsultAllowance(l.IntelMode)))
	}
	if l.Intel(intel.FeatLesson) {
		cmds = append(cmds, "`kch lesson <<'END' {json} END` (khi được yêu cầu)")
	}
	if len(cmds) == 0 {
		return ""
	}
	return "Lệnh thêm: " + strings.Join(cmds, ", ") + ".\n"
}

// logPacket records the size of one injected packet (C.7).
func (s *Server) logPacket(ctx context.Context, c hookCall, event string, chars int, hash string) {
	l := c.held.Lease
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		return intel.LogPacket(ctx, tx, l.ProjectID, l.TaskID, l.ID, c.actor.ID, event, l.Mode, l.IntelMode, chars, hash)
	})
}

// intelStop is what the Stop hook asks for besides evidence, once per turn
// (C.2, C.6): OPEN questions and assumptions not yet put to the owner, and a
// requested lesson (once per lease). Listed questions and assumptions are
// marked surfaced.
func (s *Server) intelStop(ctx context.Context, c hookCall) ([]string, error) {
	t, l := c.held.Task, c.held.Lease
	var out []string
	err := s.runtime(ctx, func(tx pgx.Tx) error {
		if l.Intel(intel.FeatQuestions) {
			qs, err := intel.Unsurfaced(ctx, tx, t.ID)
			if err != nil {
				return err
			}
			var lines []string
			for _, q := range qs {
				lines = append(lines, fmt.Sprintf("#%d %s (mặc định %q)", q.Seq, intel.Clip(q.Text, 160), intel.Clip(q.Default, 60)))
			}
			if len(lines) > 0 {
				out = append(out, "Nêu cho chủ sản phẩm các câu hỏi còn mở trong câu trả lời cuối: "+strings.Join(lines, "; ")+".")
			}
		}
		if l.Intel(intel.FeatGate) {
			keys, err := unsurfacedAssumptions(ctx, tx, t)
			if err != nil {
				return err
			}
			if len(keys) > 0 {
				out = append(out, "Nêu cho chủ sản phẩm các giả định chưa được xác nhận: "+strings.Join(keys, ", ")+".")
			}
		}
		if l.Intel(intel.FeatLesson) {
			pending, err := intel.LessonPending(ctx, tx, t.ID, l.ID)
			if err != nil {
				return err
			}
			if pending { // once per lease, not every turn (C.6, REV-005 #7)
				pending, err = intel.LessonStopOnce(ctx, tx, t.ProjectID, t.ID, l.ID)
				if err != nil {
					return err
				}
			}
			if pending {
				facts, err := intel.LeaseFacts(ctx, tx, l.ID, t.ID, t.Revision)
				if err != nil {
					return err
				}
				out = append(out, intel.LessonRequest(facts))
			}
		}
		return nil
	})
	return out, err
}

// unsurfacedAssumptions lists the CANDIDATE assumptions ASM-<task key>-n not
// yet put to the owner, and journals them as surfaced (ids only).
func unsurfacedAssumptions(ctx context.Context, tx pgx.Tx, t work.Task) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT k.id::text, k.key FROM knowledge k
		WHERE k.project_id = $1 AND k.level = 'PROJECT' AND k.is_current AND k.trust = 'CANDIDATE'
		  AND left(k.key, $3) = $2 AND NOT EXISTS (SELECT 1 FROM journal j WHERE j.task_id = $4 AND j.type = 'ASSUMPTION_SURFACED'
		      AND j.payload->>'knowledge_id' = k.id::text) ORDER BY k.key LIMIT 20`,
		t.ProjectID, "ASM-"+t.Key+"-", len([]rune("ASM-"+t.Key+"-")), t.ID)
	if err != nil {
		return nil, err
	}
	type asm struct{ ID, Key string }
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[asm])
	if err != nil {
		return nil, err
	}
	var keys []string
	for _, a := range list {
		if _, err := tx.Exec(ctx, `INSERT INTO journal (project_id, task_id, type, payload) VALUES ($1, $2, 'ASSUMPTION_SURFACED',
			jsonb_build_object('knowledge_id', $3::text))`, t.ProjectID, t.ID, a.ID); err != nil {
			return nil, err
		}
		keys = append(keys, a.Key)
	}
	return slices.Clip(keys), nil
}
