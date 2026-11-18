package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// Agent verbs of spec S2 part C (C.9, PM-S2-10, PM-S2-11). Each one takes the
// task and the project from the caller's lease, never from its input.
func init() {
	RegisterAgentVerb("understand", verbUnderstand)
	RegisterAgentVerb("ask", verbAsk)
	RegisterAgentVerb("questions", verbQuestions)
	RegisterAgentVerb("lesson", verbLesson)
	RegisterAgentVerb("consult", verbConsult)
	RegisterAgentVerb("answer", verbAnswer)
}

// fullHolder is the caller's FULL lease (PM-S2-10): 409 without a lease or with another mode.
func fullHolder(c *AgentCall) (*work.Held, error) {
	if _, err := c.Task(); err != nil {
		return nil, err
	}
	if c.Held.Lease.Mode != "FULL" {
		return nil, kerr.New(kerr.Conflict, "lệnh này chỉ dành cho lease FULL (lease của bạn: %s)", c.Held.Lease.Mode)
	}
	return c.Held, nil
}

func pinnedKeys(t work.Task) []string {
	keys := make([]string, 0, len(t.DependencyVersions))
	for k := range t.DependencyVersions {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// verbUnderstand: kch understand <<'END' {…} END (C.2, C.5, AC-S2-031, AC-S2-032, AC-S2-035).
func verbUnderstand(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	held, err := fullHolder(c)
	if err != nil {
		return AgentReply{}, err
	}
	var u intel.Understanding
	if err := c.Decode(&u); err != nil {
		return AgentReply{}, err
	}
	if err := u.CheckShape(); err != nil {
		return AgentReply{}, err
	}
	t, l := held.Task, held.Lease
	blocking := false
	for _, q := range u.Questions {
		blocking = blocking || (q.Blocking && u.Delta == nil)
	}
	cand := s.waitMark(ctx, held, blocking && l.Intel(intel.FeatQuestions)) // the tree when a wait begins (REV-005 #2)
	var reply AgentReply
	err = c.scoped(ctx, s, func(tx pgx.Tx) error {
		var acc accepted
		var err error
		if reply, acc, err = s.submitUnderstanding(ctx, tx, c, t, l, u); err != nil || acc.id == "" {
			return err
		}
		// The understanding, its questions and the wait they cause commit together (REV-005 #8).
		if err := asRuntime(ctx, tx); err != nil {
			return err
		}
		return s.afterUnderstanding(ctx, tx, c, held, acc, cand)
	})
	return reply, err
}

// accepted is what a taken understanding changed.
type accepted struct {
	id, kind, status string
	unaddressed      int
	blocking         []string
}

func (s *Server) submitUnderstanding(ctx context.Context, tx pgx.Tx, c *AgentCall, t work.Task, l work.Lease, u intel.Understanding) (AgentReply, accepted, error) {
	if err := intel.LockTask(ctx, tx, t.ID); err != nil {
		return AgentReply{}, accepted{}, err
	}
	sets, err := intel.LoadSets(ctx, tx, intel.ProductInput{ProjectID: t.ProjectID, Scope: t.Scope, Pinned: pinnedKeys(t), TaskKey: t.Key})
	if err != nil {
		return AgentReply{}, accepted{}, err
	}
	pending, err := intel.GatePending(ctx, tx, t.ID, t.Revision)
	if err != nil {
		return AgentReply{}, accepted{}, err
	}
	kind, limit, missing := intel.KindFull, intel.FullRejectLimit, []intel.Missing(nil)
	if u.Delta != nil {
		if pending.Full {
			return AgentReply{}, accepted{}, kerr.New(kerr.Invalid, "chưa có bản hiểu đầy đủ của task: nộp bản đầy đủ trước, không phải delta")
		}
		kind, limit, missing = intel.KindDelta, intel.DeltaRejectLimit, intel.ValidateDelta(*u.Delta, sets)
	} else {
		missing = intel.Validate(u, sets)
	}
	rejected, err := intel.Rejections(ctx, tx, t.ID, t.Revision, kind)
	if err != nil {
		return AgentReply{}, accepted{}, err
	}
	row := intel.UnderstandingRow{ProjectID: t.ProjectID, TaskID: t.ID, LeaseID: l.ID, TaskRevision: t.Revision,
		Author: c.Actor.ID, Kind: kind, Status: intel.StatusAccept, Body: c.Body, Unaddressed: missing}
	if len(missing) > 0 && rejected < limit { // 422 with the list; the row is kept for the owner (AC-S2-031)
		row.Status = intel.StatusReject
		if _, err := intel.InsertUnderstanding(ctx, tx, row); err != nil {
			return AgentReply{}, accepted{}, err
		}
		return AgentReply{Status: http.StatusUnprocessableEntity, Text: intel.Rejected(missing, limit-rejected, kind == intel.KindDelta),
			Data: map[string]any{"unaddressed": missing, "kind": kind}}, accepted{}, nil
	}
	if len(missing) > 0 {
		row.Status = intel.StatusForced // bounded: the agent is never locked forever
	}
	id, err := intel.InsertUnderstanding(ctx, tx, row)
	if err != nil {
		return AgentReply{}, accepted{}, err
	}
	acc := accepted{id: id, kind: kind, status: row.Status, unaddressed: len(missing)}
	var b strings.Builder
	fmt.Fprintf(&b, "Đã nhận %s (task %s, rev %d).", map[string]string{intel.KindFull: "bản hiểu", intel.KindDelta: "delta"}[kind], t.Key, t.Revision)
	if row.Status == intel.StatusForced {
		b.WriteString(" Nhận kèm các mục CHƯA xử lý (chủ sản phẩm sẽ thấy):\n" + intel.ListMissing(missing))
	}
	data := map[string]any{"id": id, "kind": kind, "status": row.Status, "unaddressed": orEmpty(missing)}
	if kind == intel.KindFull {
		keys, err := s.storeAssumptions(ctx, tx, c, t, u.Assumptions)
		if err != nil {
			return AgentReply{}, accepted{}, err
		}
		qs, err := storeQuestions(ctx, tx, c, t, id, u.Questions)
		if err != nil {
			return AgentReply{}, accepted{}, err
		}
		b.WriteString(describeStored(keys, qs, l.Intel(intel.FeatQuestions)))
		for _, q := range qs {
			if q.Blocking {
				acc.blocking = append(acc.blocking, q.ID)
			}
		}
		data["assumptions"], data["questions"] = orEmpty(keys), orEmpty(qs)
	}
	b.WriteString("\n")
	return AgentReply{Status: http.StatusCreated, Text: b.String(), Data: data}, acc, nil
}

// afterUnderstanding applies the runtime side of an accepted understanding,
// in the transaction that took it, under the runtime scope: the journal
// records it, a blocking question makes the task wait (T-S2-01), and the
// lease leaves DISCOVERY (C.7). The task row is locked before the lease row.
func (s *Server) afterUnderstanding(ctx context.Context, tx pgx.Tx, c *AgentCall, held *work.Held, acc accepted, cand string) error {
	t, l := held.Task, held.Lease
	typ := "UNDERSTANDING_ACCEPTED"
	if acc.status == intel.StatusForced {
		typ = "UNDERSTANDING_FORCED"
	}
	if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: typ, Actor: c.Actor.ID,
		Payload: map[string]any{"understanding_id": acc.id, "kind": acc.kind, "revision": t.Revision, "unaddressed": acc.unaddressed,
			"lease_id": l.ID}}); err != nil {
		return err
	}
	if err := s.blockOnQuestions(ctx, tx, c, held, acc.blocking, cand); err != nil {
		return err
	}
	if l.IntelPhase != intel.PhaseDiscovery {
		return nil
	}
	phase, err := intel.WorkPhase(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	return work.SetPhase(ctx, tx, l.ID, phase)
}

// blockOnQuestions applies T-S2-01 for blocking questions when the questions
// mechanism is on; cand is the tree when the wait begins (REV-005 #2).
func (s *Server) blockOnQuestions(ctx context.Context, tx pgx.Tx, c *AgentCall, held *work.Held, ids []string, cand string) error {
	if len(ids) == 0 || !held.Lease.Intel(intel.FeatQuestions) {
		return nil
	}
	_, err := work.AddWaitingReason(ctx, tx, held.Task.ID, work.ReasonQuestion, "QUESTION_BLOCKING", c.Actor.ID,
		map[string]any{"question_ids": ids, "lease_id": held.Lease.ID, "candidate": cand})
	return err
}
