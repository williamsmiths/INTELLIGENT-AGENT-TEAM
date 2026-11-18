package work

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/intel"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// IntelOptions are the owner's choices for a new lease (spec S2 C.1): a forced
// intel mode ("" = the router decides) and the switched-on mechanisms (nil =
// the default of the mode: all of them in DEEP, all but the gate otherwise).
type IntelOptions struct {
	Mode     string
	Features []string

	writeNotice string // AC-S2-039: refused once at the first write
	via         string // "delegate": journaled with LEASE_GRANTED (spec S2 E.1-12); set only inside this package
}

type routed struct {
	mode     string
	signals  intel.Signals
	features []string
	phase    string
}

// routeLease computes the router signals of a task at its current revision
// and the state a new lease starts with (spec S2 C.1, C.7).
func routeLease(ctx context.Context, tx pgx.Tx, t Task, leaseMode string, opt IntelOptions) (routed, error) {
	if !intel.ValidMode(opt.Mode) {
		return routed{}, kerr.New(kerr.Invalid, "intel_mode must be DIRECT, ASSISTED or DEEP")
	}
	features, err := intel.NormalizeFeatures(opt.Features)
	if err != nil {
		return routed{}, err
	}
	pinned := make([]string, 0, len(t.DependencyVersions))
	for k := range t.DependencyVersions {
		pinned = append(pinned, k)
	}
	slices.Sort(pinned)
	sig, err := intel.Route(ctx, tx, intel.ProductInput{ProjectID: t.ProjectID, Scope: t.Scope, Pinned: pinned, TaskKey: t.Key})
	if err != nil {
		return routed{}, err
	}
	sig.Forced = opt.Mode
	if opt.Features == nil {
		// Default features: the understanding gate only in DEEP (spec S2 C.1, Task 2 ablation): on 9 real
		// runs per arm it cost ~44% more than the same harness without it (0.0287 vs 0.0199 USD) with
		// the same oracle result. The Owner can still ask for it with intel_features.
		features = intel.DefaultFeatures(sig.Mode())
	}
	r := routed{mode: sig.Mode(), signals: sig, features: features}
	pending := false
	if leaseMode == "FULL" && intel.Has(features, intel.FeatGate) && intel.NeedsUnderstanding(r.mode) {
		p, err := intel.GatePending(ctx, tx, t.ID, t.Revision)
		if err != nil {
			return routed{}, err
		}
		pending = p.Any()
	}
	phase, err := intel.WorkPhase(ctx, tx, t.ID)
	if err != nil {
		return routed{}, err
	}
	r.phase = intel.InitialPhase(pending, phase)
	return r, nil
}

// maxMissed bounds the notice built from missed feedback.
const maxMissed = 8000

// missedNotices is the verbatim text of every owner feedback (an answer that
// differed from the agent's default is one too) received since the previous
// lease of the task ended, oldest first, with its revision (AC-S2-039). A task
// that never had a lease has no previous lease and so no missed feedback.
// "Since" is by journal order, not by time (REV-005 #3): a feedback that
// expires an overdue lease in its own transaction has the same now() as the
// lease's end, but its TASK_FEEDBACK comes after that LEASE_ENDED.
func missedNotices(ctx context.Context, tx pgx.Tx, taskID string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT COALESCE((j.payload->>'revision')::int, 0), s.content FROM journal j
		JOIN source s ON s.id = (j.payload->>'source_id')::uuid
		WHERE j.task_id = $1 AND j.type = 'TASK_FEEDBACK'
		  AND j.seq > (SELECT max(e.seq) FROM journal e WHERE e.task_id = $1 AND e.type = 'LEASE_ENDED')
		ORDER BY j.seq`, taskID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var rev int
	var text string
	n := 0
	if _, err := pgx.ForEachRow(rows, []any{&rev, &text}, func() error {
		n++
		fmt.Fprintf(&b, "[rev %d] %s\n", rev, text)
		return nil
	}); err != nil || n == 0 {
		return "", err
	}
	out := "Chủ sản phẩm đã gửi khi task chưa có người giữ (theo thứ tự thời gian):\n" + b.String()
	if r := []rune(out); len(r) > maxMissed {
		out = string(r[:maxMissed]) + "…"
	}
	return strings.TrimRight(out, "\n"), nil
}
