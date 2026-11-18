package api

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// holderLease is the live lease of a task when the actor holds it.
func holderLease(actor authn.Principal, tv work.TaskView) *work.Lease {
	if tv.Lease != nil && tv.Lease.PrincipalID == actor.ID {
		return tv.Lease
	}
	return nil
}

// verifyGate refuses the holder's verify before the understanding or delta
// is taken (C.2, REV-004 #12): 409.
func (s *Server) verifyGate(ctx context.Context, actor authn.Principal, tv work.TaskView) error {
	l := holderLease(actor, tv)
	if l == nil || actor.Enforcement != "HOOK" || !gateOn(*l) {
		return nil
	}
	p, err := s.pendingFor(ctx, tv.Task)
	if err != nil || !p.Any() {
		return err
	}
	if p.Full {
		return kerr.New(kerr.Conflict, "task %s (mode %s) cần bản hiểu trước khi verify: nộp bằng `kch understand <<'END' {json} END`", tv.Task.Key, l.IntelMode)
	}
	return kerr.New(kerr.Conflict, "task %s đã đổi (rev %d): nộp delta bằng `kch understand` trước khi verify", tv.Task.Key, tv.Task.Revision)
}

// recordAttempt writes the attempt of a holder's verify and applies C.4, C.5
// (measurement) and C.6. Its result is added to the verify answer.
func (s *Server) recordAttempt(ctx context.Context, actor authn.Principal, tv work.TaskView, res verify.Result) (map[string]any, error) {
	l := holderLease(actor, tv)
	if l == nil {
		return nil, nil
	}
	t := tv.Task
	var passing, failing []string
	tails := map[string]string{}
	for _, it := range res.Items {
		if it.Result == "PASS" {
			passing = append(passing, it.CheckID)
			continue
		}
		failing = append(failing, it.CheckID)
		tails[it.CheckID] = s.logTail(it.LogPath)
		if v, _ := it.Detail["violations"].([]string); len(v) > 0 {
			tails[it.CheckID] += "\n" + strings.Join(v, "\n")
		}
	}
	if res.CandidateChanged && len(failing) == 0 {
		failing = append(failing, "candidate_changed")
	}
	mismatch := s.deltaMismatch(ctx, t, *l)
	out := map[string]any{}
	err := s.runtime(ctx, func(tx pgx.Tx) error {
		// Two verifies of one task must not both read the same previous
		// attempt: the task row is locked first (REV-005 #9; task, then lease).
		if _, err := work.LockTask(ctx, tx, t.ID); err != nil {
			return err
		}
		prev, err := intel.PreviousAttempt(ctx, tx, t.ID)
		if err != nil {
			return err
		}
		fp := intel.Fingerprint(failing, tails)
		a, err := intel.InsertAttempt(ctx, tx, intel.AttemptRow{ProjectID: t.ProjectID, TaskID: t.ID, LeaseID: l.ID, PrincipalID: actor.ID,
			TaskRevision: t.Revision, Candidate: res.Candidate, AllPass: len(failing) == 0, Passing: passing, Failing: failing,
			Fingerprint: fp, Repeats: intel.Repeats(prev, failing, fp), DeltaMismatch: mismatch})
		if err != nil {
			return err
		}
		out["attempt"] = map[string]any{"id": a.ID, "repeats": a.Repeats, "failing": orEmpty(a.Failing)}
		if mismatch != nil {
			out["delta_mismatch"] = mismatch
		}
		if l.IntelPhase != intel.PhaseDiscovery { // C.7: REWORK from the first failing attempt until it passes
			phase := intel.PhaseRework
			if a.AllPass {
				phase = intel.PhaseBuild
			}
			if err := work.SetPhase(ctx, tx, l.ID, phase); err != nil {
				return err
			}
		}
		if err := s.converge(ctx, tx, actor, t, *l, a, out); err != nil {
			return err
		}
		return s.requestLesson(ctx, tx, t, *l, a, out)
	})
	return out, err
}

// converge applies C.4 for a FULL lease with convergence on.
func (s *Server) converge(ctx context.Context, tx pgx.Tx, actor authn.Principal, t work.Task, l work.Lease, a intel.AttemptRow, out map[string]any) error {
	if !l.Intel(intel.FeatConvergence) || a.AllPass {
		return nil
	}
	granted, err := intel.BonusGranted(ctx, tx, t.ID)
	if err != nil {
		return err
	}
	c := intel.Judge(a.Repeats, granted, a.Failing)
	out["convergence"] = c
	switch c.Status {
	case "STEER": // AC-S2-034: steer text now and at the next hook, +1 consult once per task
		if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "NONPROGRESS", Actor: actor.ID,
			Payload: map[string]any{"attempt_id": a.ID, "repeats": a.Repeats, "bonus_consult": c.Bonus, "lease_id": l.ID}}); err != nil {
			return err
		}
		return work.PushInbox(ctx, tx, l.ID, c.Steer)
	case "PARKED":
		_, err := work.AddWaitingReason(ctx, tx, t.ID, work.ReasonNonprogress, "NONPROGRESS_PARKED", actor.ID,
			map[string]any{"attempt_id": a.ID, "repeats": a.Repeats, "lease_id": l.ID, "candidate": a.Candidate})
		return err
	}
	return nil
}

// requestLesson applies C.6: after an all-pass verify on a lease that had a
// failed attempt, a SCOPE_VIOLATION or a feedback, a lesson is requested once.
func (s *Server) requestLesson(ctx context.Context, tx pgx.Tx, t work.Task, l work.Lease, a intel.AttemptRow, out map[string]any) error {
	if !l.Intel(intel.FeatLesson) || !a.AllPass {
		return nil
	}
	facts, err := intel.LeaseFacts(ctx, tx, l.ID, t.ID, t.Revision)
	if err != nil || !facts.Due() {
		return err
	}
	out["lesson_requested"] = map[string]any{"facts": facts, "text": intel.LessonRequest(facts)}
	pending, err := intel.LessonPending(ctx, tx, t.ID, l.ID)
	if err != nil || pending {
		return err
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: t.ProjectID, TaskID: t.ID, Type: "LESSON_REQUESTED",
		Payload: map[string]any{"lease_id": l.ID, "attempt_id": a.ID, "failed_attempts": facts.FailedAttempts}})
	return err
}

// deltaMismatch compares the newest delta's declared paths with the files
// changed since the lease's mark (C.5): measured, never enforced.
func (s *Server) deltaMismatch(ctx context.Context, t work.Task, l work.Lease) map[string]any {
	if len(l.IntelMark) == 0 || l.TaskRevision < 2 {
		return nil
	}
	var declared []string
	var found bool
	if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		declared, found, err = intel.LatestDelta(ctx, tx, t.ID)
		return err
	}); err != nil || !found {
		return nil
	}
	var mark intel.Mark
	if json.Unmarshal(l.IntelMark, &mark) != nil {
		return nil
	}
	now, err := s.markOf(ctx, t)
	if err != nil {
		return map[string]any{"error": "working tree unreadable"}
	}
	return intel.DeltaMismatch(declared, intel.ChangedSince(mark, now.Files))
}

// intelVerifyText adds the part C outcome of a verify to the agent's answer.
func intelVerifyText(out map[string]any) string {
	var b strings.Builder
	if c, ok := out["convergence"].(map[string]any); ok {
		switch c["status"] {
		case "STEER":
			fmt.Fprintf(&b, "Hội tụ: %v\n", c["steer"])
		case "PARKED":
			fmt.Fprintf(&b, "Hội tụ: %v lần verify thất bại giống hệt nhau — task chuyển WAITING_DECISION chờ chủ sản phẩm; không ghi vào repo nữa, tóm tắt và dừng.\n", c["repeats"])
		}
	}
	if l, ok := out["lesson_requested"].(map[string]any); ok {
		fmt.Fprintf(&b, "%v\n", l["text"])
	}
	return b.String()
}
