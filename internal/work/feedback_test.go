package work_test

import (
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

func (e *env) feedback(taskID, text string) work.Change {
	e.t.Helper()
	var ch work.Change
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		ch, err = work.Feedback(e.ctx, tx, e.owner, taskID, text)
		return err
	}))
	return ch
}

// AC-H-050: feedback is stored verbatim, bumps the revision and replaces the lease.
// AC-H-052: an independent task of the same project is not touched.
func TestAC_H_050_AC_H_052_FeedbackReplacesLease(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	other := e.running(a.ID, "U-1", v)
	var first, otherLease work.Lease
	e.must(e.rt(func(tx pgx.Tx) error {
		for p, out := range map[string]*work.Lease{w.ID: &first, v.ID: &otherLease} {
			held, err := work.ActiveLease(e.ctx, tx, p)
			if err != nil {
				return err
			}
			*out = held.Lease
		}
		return nil
	}))
	e.must(e.as(w, func(tx pgx.Tx) error { // the holder acknowledges its context under its own scope
		if _, err := work.AckContext(e.ctx, tx, first.ID); err != nil {
			return err
		}
		return work.MarkWrote(e.ctx, tx, first.ID)
	}))

	const text = "Nút thêm phải nằm trong Part"
	ch := e.feedback(task.ID, text)
	if ch.Task.Revision != 2 || ch.Task.State != "RUNNING" {
		t.Fatalf("task after feedback: rev %d, %s", ch.Task.Revision, ch.Task.State)
	}
	if ch.Ended == nil || ch.Ended.ID != first.ID || ch.Ended.State != "REVOKED" || ch.Ended.Reason != "feedback" {
		t.Fatalf("old lease: %+v", ch.Ended)
	}
	n := ch.Granted
	if n == nil || n.State != "ACTIVE" || n.Epoch != 2 || n.PrincipalID != w.ID || n.TaskRevision != 2 ||
		n.ContextAcked || n.PendingNotice != text || n.Mode != first.Mode || n.WroteFiles {
		t.Fatalf("new lease: %+v", n)
	}
	stored := scalar[string](e, `SELECT origin || '|' || kind || '|' || content FROM source WHERE id = $1 AND project_id = $2
		AND created_by = $3`, ch.SourceID, a.ID, e.owner.ID)
	if stored != "OWNER|feedback|"+text {
		t.Fatalf("source = %q", stored)
	}
	// The journal carries ids and reason codes, never the text (PM-H-20).
	if n := scalar[int](e, `SELECT count(*) FROM journal WHERE payload::text LIKE '%Part%'`); n != 0 {
		t.Fatal("feedback text leaked into the journal")
	}
	// The holder gets the notice exactly once, then its context is acknowledged (hook: deny once, then allow).
	e.must(e.as(w, func(tx pgx.Tx) error {
		wantKind(t, work.TouchLease(e.ctx, tx, first.ID), kerr.NotFound, "heartbeat on the revoked lease")
		for i, want := range []string{text, ""} {
			got, err := work.AckContext(e.ctx, tx, n.ID)
			if err != nil || got != want {
				t.Fatalf("ack %d: notice %q, err %v", i, got, err)
			}
		}
		return nil
	}))
	if l := e.view(task.ID).Lease; !l.ContextAcked || l.PendingNotice != "" {
		t.Fatalf("lease after ack: %+v", l)
	}
	// AC-H-052: V's lease on the other task is the very same row.
	got := e.view(other.ID)
	if got.Task.Revision != 1 || got.Lease == nil || got.Lease.ID != otherLease.ID || got.Lease.Epoch != 1 ||
		got.Lease.PendingNotice != "" || got.Lease.State != "ACTIVE" {
		t.Fatalf("independent task changed: %+v", got)
	}
	// Another agent cannot touch a lease that is not its own (row-level security).
	wantKind(t, e.as(v, func(tx pgx.Tx) error { _, err := work.AckContext(e.ctx, tx, n.ID); return err }),
		kerr.NotFound, "ack on someone else's lease")
}

// AC-H-051: only the owner sends feedback.
func TestAC_H_051_FeedbackIsOwnerOnly(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 10, 1)
	lead := e.agent("lead", a.ID, "lead", "HOOK")
	w := e.agent("w", a.ID, "worker", "HOOK")
	task := e.running(a.ID, "T-1", w)
	for _, p := range []authn.Principal{w, lead} {
		wantKind(t, e.as(p, func(tx pgx.Tx) error {
			_, err := work.Feedback(e.ctx, tx, p, task.ID, "change it")
			return err
		}), kerr.Forbidden, "feedback by "+p.Name)
	}
	if v := e.view(task.ID); v.Task.Revision != 1 || v.Lease.Epoch != 1 || scalar[int](e, `SELECT count(*) FROM source`) != 0 {
		t.Fatalf("a refused feedback changed the task: %+v", v)
	}
	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.Feedback(e.ctx, tx, e.owner, task.ID, "  ")
		return err
	}), kerr.Invalid, "empty feedback")
}

// AC-H-056: feedback on a VERIFIED task, with and without a lease; and in the other open states.
// AC-H-054: feedback on a SUPERSEDED task is a 409.
func TestAC_H_056_AC_H_054_FeedbackByState(t *testing.T) {
	e := newEnv(t)
	a := e.project("alpha", 100, 1)
	w := e.agent("w", a.ID, "worker", "HOOK")
	v := e.agent("v", a.ID, "worker", "HOOK")

	held := e.verified(e.running(a.ID, "T-1", w), "tree-c1")
	ch := e.feedback(held.ID, "one more thing")
	if ch.Task.State != "RUNNING" || ch.Task.VerifiedCandidate != "" || ch.Task.Revision != 2 ||
		ch.Granted == nil || ch.Granted.Epoch != 2 || ch.Ended.State != "REVOKED" {
		t.Fatalf("VERIFIED with a lease: %+v", ch)
	}
	if scalar[int](e, `SELECT count(*) FROM journal WHERE type = 'EVIDENCE_STALE' AND task_id = $1
		AND payload->>'reason' = 'feedback'`, held.ID) != 1 {
		t.Fatal("no EVIDENCE_STALE event")
	}
	// The old evidence is on revision 1, so it cannot verify revision 2 (T-H-03).
	wantKind(t, e.rt(func(tx pgx.Tx) error {
		_, err := work.MarkVerified(e.ctx, tx, held.ID, "tree-c1")
		return err
	}), kerr.Conflict, "verify revision 2 with evidence of revision 1")

	free := e.verified(e.running(a.ID, "T-2", v), "tree-c2")
	e.must(e.rt(func(tx pgx.Tx) error {
		_, err := work.ReleaseLease(e.ctx, tx, v, free.ID)
		return err
	}))
	ch = e.feedback(free.ID, "not quite")
	if ch.Task.State != "RUNNABLE" || ch.Task.VerifiedCandidate != "" || ch.Task.Revision != 2 || ch.Granted != nil || ch.Ended != nil {
		t.Fatalf("VERIFIED without a lease: %+v", ch)
	}

	// PROPOSED and RUNNABLE keep their state; only the revision moves.
	var proposed work.Task
	e.must(e.as(e.owner, func(tx pgx.Tx) (err error) {
		proposed, err = work.CreateTask(e.ctx, tx, e.owner, a.ID, newTask("T-3", ""))
		return err
	}))
	for id, state := range map[string]string{proposed.ID: "PROPOSED", e.runnable(a.ID, "T-4").ID: "RUNNABLE"} {
		if ch := e.feedback(id, "note"); ch.Task.State != state || ch.Task.Revision != 2 || ch.Granted != nil {
			t.Fatalf("%s after feedback: %+v", state, ch)
		}
	}

	// AC-H-054: a closed task takes no feedback.
	e.must(e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.SupersedeTask(e.ctx, tx, e.owner, proposed.ID)
		return err
	}))
	sources := scalar[int](e, `SELECT count(*) FROM source`)
	wantKind(t, e.as(e.owner, func(tx pgx.Tx) error {
		_, err := work.Feedback(e.ctx, tx, e.owner, proposed.ID, "too late")
		return err
	}), kerr.Conflict, "feedback on a SUPERSEDED task")
	if got := e.view(proposed.ID).Task; got.Revision != 2 || scalar[int](e, `SELECT count(*) FROM source`) != sources {
		t.Fatalf("refused feedback left a trace: rev %d", got.Revision)
	}
}
