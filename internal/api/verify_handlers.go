package api

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/memory"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// packetFor assembles the context packet of a task as the actor may see it
// (PM-H-12, PM-H-13). It runs in the actor's own scope, so only rows of the
// task's project and confirmed GLOBAL items can reach the packet (AC-H-080).
// The budget line is appended after the packet so the context hash does not
// change with every token spent. maxChars 0 is the server's default budget.
func (s *Server) packetFor(ctx context.Context, tx pgx.Tx, actor authn.Principal, t work.Task, notice string, maxChars int) (memory.Packet, string, error) {
	if maxChars <= 0 {
		maxChars = s.PacketChars
	}
	// spec S2 E.1-11 (REV-006b N1): a delegated task is labelled with its Lead.
	// The holder cannot read principal rows; the name is in the TASK_CREATED payload.
	var by string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(payload->>'by_name'), '') FROM journal
		WHERE task_id = $1 AND type = 'TASK_CREATED' AND payload->>'via' = 'delegate'`, t.ID).Scan(&by); err != nil {
		return memory.Packet{}, "", err
	}
	p, err := memory.AssemblePacket(ctx, tx, actor, memory.PacketInput{
		ProjectID: t.ProjectID, TaskID: t.ID, TaskKey: t.Key, TaskTitle: t.Title, DesiredOutcome: t.DesiredOutcome,
		Scope: t.Scope, Revision: t.Revision, DependencyVersions: t.DependencyVersions,
		AllowedPaths: t.AllowedPaths, ProtectedPaths: t.ProtectedPaths, RepoPath: t.RepoPath,
		PendingNotice: notice, MaxChars: maxChars, DelegatedBy: by,
	})
	if err != nil {
		return memory.Packet{}, "", err
	}
	b, err := work.GetBudget(ctx, tx, actor, t.ID)
	if err != nil {
		return memory.Packet{}, "", err
	}
	return p, budgetLine(b), nil
}

// budgetLine is one line for the agent: what is spent, the limit and the state.
func budgetLine(b work.BudgetView) string {
	limit := "không giới hạn"
	if b.Task.Limits.MaxCost != nil {
		limit = fmt.Sprintf("%.2f USD", float64(*b.Task.Limits.MaxCost)/1e6)
	}
	line := fmt.Sprintf("Ngân sách task: đã dùng %.2f USD / %s, %d tool call — trạng thái %s.",
		float64(b.Task.Used.Cost)/1e6, limit, b.Task.Used.ToolCalls, b.Effective)
	if b.Effective == "WARN" {
		line += " Sắp chạm hạn mức: ưu tiên việc còn thiếu để verify."
	}
	return line
}

func (s *Server) getContext(w http.ResponseWriter, r *http.Request) {
	var p memory.Packet
	var budget string
	err := s.scoped(r, func(tx pgx.Tx) error {
		tv, err := work.GetTask(r.Context(), tx, principalOf(r), r.PathValue("id"))
		if err != nil {
			return err
		}
		p, budget, err = s.packetFor(r.Context(), tx, principalOf(r), tv.Task, "", s.packetChars(tv.Lease, principalOf(r).ID)) // spec S2 C.1
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"context_hash": p.Hash, "text": p.Text + budget + "\n",
		"blocks": p.Blocks, "dropped": p.Dropped, "chars": p.Chars}, err)
}

// verifyTask asks the Runtime to run the task's checks (PM-H-17). The caller
// only requests; the result is produced by the runner on the exact candidate
// and nothing in the request body can influence it (AC-H-042).
func (s *Server) verifyTask(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var tv work.TaskView
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		tv, err = work.GetTask(ctx, tx, principalOf(r), r.PathValue("id"))
		return err
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	t := tv.Task
	if t.State != "RUNNING" && t.State != "VERIFIED" {
		writeErr(w, r, kerr.New(kerr.Conflict, "task đang ở %s; chỉ verify được khi RUNNING hoặc VERIFIED", t.State))
		return
	}
	if err := s.verifyGate(ctx, principalOf(r), tv); err != nil { // spec S2 C.2: 409 before the understanding
		writeErr(w, r, err)
		return
	}
	req, err := s.verifyRequest(ctx, t) // trusted base store, trusted ignore list, sandbox settings (S2 D.1, AC-S2-010)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	res, err := s.Runner.Run(ctx, req)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	var after work.Task
	err = s.runtime(ctx, func(tx pgx.Tx) error {
		if err := verify.Record(ctx, tx, req, res); err != nil {
			return err
		}
		var err error
		switch {
		case res.AllPass:
			after, err = work.MarkVerified(ctx, tx, t.ID, res.Candidate) // T-H-03
		case t.State == "VERIFIED":
			after, err = work.MarkStale(ctx, tx, t.ID) // T-H-04: the verified tree is gone
		default:
			after = t
		}
		if kerr.Is(err, kerr.Conflict) { // the task moved on while the checks ran; the evidence still stands
			after, err = t, nil
		}
		return err
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := map[string]any{"all_pass": res.AllPass, "candidate": res.Candidate,
		"candidate_changed": res.CandidateChanged, "items": res.Items, "task_state": after.State}
	extra, err := s.recordAttempt(ctx, principalOf(r), tv, res) // spec S2 C.4, C.5, C.6: the holder's attempt
	for k, v := range extra {
		out[k] = v
	}
	if err == nil && extra != nil { // a parked task (nonprogress) is WAITING_DECISION now
		err = s.runtime(ctx, func(tx pgx.Tx) error {
			cur, err := work.GetTask(ctx, tx, principalOf(r), t.ID)
			out["task_state"] = cur.Task.State
			return err
		})
	}
	s.reply(w, r, http.StatusOK, out, err)
}

func (s *Server) listEvidence(w http.ResponseWriter, r *http.Request) {
	var out []verify.Evidence
	err := s.scoped(r, func(tx pgx.Tx) error {
		if _, err := work.GetTask(r.Context(), tx, principalOf(r), r.PathValue("id")); err != nil {
			return err
		}
		var err error
		out, err = verify.Latest(r.Context(), tx, r.PathValue("id"))
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"evidence": orEmpty(out)}, err)
}
