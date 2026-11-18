package work

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
)

// Spec S2 part E: a Lead delegates one task to the single Worker of a
// specialty in its project — create, ready and assign in one transaction
// (PM-S2-20, E.1). Narrower than PM-H-11: Worker only, mode FULL, checks and
// protected paths from the Owner's policy, nothing the Lead writes is trusted.

// Limits of the Lead-written text (E.1-11, REV-006b N1).
const (
	DelegateLineMax    = 120  // title and scope: one line
	DelegateOutcomeMax = 4000 // desired outcome, may span lines
	DelegateRateMax    = 10   // successful delegates per caller in DelegateRateWindow (E.1-10)
	DelegateRateWindow = "60 minutes"
)

// Delegation is the body of `kch delegate` (E.5). The verb decodes it strictly.
type Delegation struct {
	To           string   `json:"to"`
	Title        string   `json:"title"`
	Outcome      string   `json:"outcome"`
	Scope        string   `json:"scope"`
	AllowedPaths []string `json:"allowed_paths"`
	Repo         string   `json:"repo"`
}

// Delegated is what one delegate did.
type Delegated struct {
	Change
	AssigneeID   string
	AssigneeName string
	OpenBotAgent string // where the notice goes (E.1-3 guarantees it is set)
}

// RequireDelegator: only a Lead of the project delegates (E.1-1, PM-S2-22); a
// Worker, and anyone else, gets 403.
func RequireDelegator(actor authn.Principal, projectID string) error {
	if actor.IsOwner() || actor.RoleIn(projectID) != "lead" {
		return kerr.New(kerr.Forbidden, "chỉ Lead của project này được giao việc bằng kch delegate")
	}
	return nil
}

// Validate checks the shape of the Lead's text (E.1-2, E.1-11, N1): title and
// scope are one line without control characters, the outcome may span lines.
func (d Delegation) Validate() error {
	if !slices.Contains(Specialties, d.To) {
		return kerr.New(kerr.Invalid, "to phải là một trong: %s", strings.Join(Specialties, ", "))
	}
	if strings.TrimSpace(d.Title) == "" || !plainText(d.Title, false, DelegateLineMax) {
		return kerr.New(kerr.Invalid, "title: bắt buộc, một dòng, tối đa %d ký tự, không có ký tự điều khiển", DelegateLineMax)
	}
	if !plainText(d.Scope, false, DelegateLineMax) {
		return kerr.New(kerr.Invalid, "scope: một dòng, tối đa %d ký tự, không có ký tự điều khiển", DelegateLineMax)
	}
	if strings.TrimSpace(d.Outcome) == "" || !plainText(d.Outcome, true, DelegateOutcomeMax) {
		return kerr.New(kerr.Invalid, "outcome: bắt buộc, tối đa %d ký tự, không có ký tự điều khiển", DelegateOutcomeMax)
	}
	for _, p := range d.AllowedPaths { // shown to the recipient line by line (N1)
		if strings.TrimSpace(p) == "" || !plainText(p, false, DelegateLineMax) {
			return kerr.New(kerr.Invalid, "allowed_paths: mỗi mục một dòng, tối đa %d ký tự, không có ký tự điều khiển", DelegateLineMax)
		}
	}
	return nil
}

// plainText: valid UTF-8, at most max runes, no control character (newline
// and tab only when multiline), no line/paragraph separator, no bidi control.
func plainText(s string, multiline bool, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		switch {
		case multiline && (r == '\n' || r == '\t'):
		case unicode.IsControl(r), unicode.In(r, unicode.Zl, unicode.Zp), unicode.Is(unicode.Bidi_Control, r):
			return false
		}
	}
	return true
}

// Delegate runs E.1 in the caller's transaction, which must use the runtime
// scope (E.1-8): every right is checked here in code, then CreateTask →
// ReadyTask → AssignTaskWith. Any error leaves nothing behind (rollback).
// The caller resolves projectID from the runtime (AgentCall.Project).
func Delegate(ctx context.Context, tx pgx.Tx, bases verify.Store, actor authn.Principal, projectID string, in Delegation) (Delegated, error) {
	if err := RequireDelegator(actor, projectID); err != nil {
		return Delegated{}, err
	}
	if err := in.Validate(); err != nil {
		return Delegated{}, err
	}
	if err := delegateRate(ctx, tx, actor.ID); err != nil {
		return Delegated{}, err
	}
	policy, err := loadDelegatePolicy(ctx, tx, projectID)
	if err != nil {
		return Delegated{}, err
	}
	if len(policy.RequiredChecks) == 0 { // E.1-4, REV-006 C1
		return Delegated{}, kerr.New(kerr.Conflict, "project chưa có check mặc định; Owner đặt bằng PUT /v1/projects/{p}/delegate-policy")
	}
	var defaultCost *int64
	if err := tx.QueryRow(ctx, `SELECT default_task_max_cost_microusd FROM project WHERE id = $1`, projectID).Scan(&defaultCost); err != nil {
		return Delegated{}, err
	}
	if defaultCost == nil { // E.1-7
		return Delegated{}, kerr.New(kerr.Conflict, "project chưa có default_task_max_cost; Owner đặt bằng PUT /v1/projects/{p}/limits")
	}
	to, err := delegateRecipient(ctx, tx, projectID, in.To)
	if err != nil {
		return Delegated{}, err
	}
	repo, err := delegateRepo(ctx, tx, projectID, in.Repo)
	if err != nil {
		return Delegated{}, err
	}
	if err := delegateFree(ctx, tx, repo, to); err != nil {
		return Delegated{}, err
	}
	if in.AllowedPaths, err = normalizeAllowed(repo, in.AllowedPaths); err != nil { // E.1-13
		return Delegated{}, err
	}
	key, err := delegateKey(ctx, tx, projectID, time.Now())
	if err != nil {
		return Delegated{}, err
	}
	t, err := CreateTask(ctx, tx, actor, projectID, NewTask{Key: key, Title: in.Title, DesiredOutcome: in.Outcome, Scope: in.Scope,
		RepoPath: repo, AllowedPaths: in.AllowedPaths, ProtectedPaths: policy.ProtectedPaths, RequiredChecks: policy.RequiredChecks,
		Via: "delegate"})
	if err != nil {
		return Delegated{}, err
	}
	if _, err := ReadyTask(ctx, tx, actor, t.ID); err != nil { // T-H-01
		return Delegated{}, err
	}
	ch, err := AssignTaskWith(ctx, tx, bases, actor, t.ID, to.id, "FULL", IntelOptions{via: "delegate"}) // T-H-02, E.1-5
	if err != nil {
		return Delegated{}, err
	}
	return Delegated{Change: ch, AssigneeID: to.id, AssigneeName: to.name, OpenBotAgent: to.agent}, nil
}

// delegateRate allows DelegateRateMax successful delegates per caller in a
// sliding window, counted from the journal under a per-caller lock (E.1-10).
// Failed attempts roll back and are not counted.
func delegateRate(ctx context.Context, tx pgx.Tx, actorID string) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('kch.delegate.rate:' || $1, 0))`, actorID); err != nil {
		return err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM journal WHERE actor = $1 AND type = 'TASK_CREATED'
		AND payload->>'via' = 'delegate' AND created_at > now() - $2::interval`, actorID, DelegateRateWindow).Scan(&n); err != nil {
		return err
	}
	if n >= DelegateRateMax {
		return kerr.New(kerr.TooMany, "đã giao %d task trong 60 phút qua (tối đa %d); thử lại sau", n, DelegateRateMax)
	}
	return nil
}
