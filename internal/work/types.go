// Package work owns the work model of the harness: projects, repo roots,
// principals, tasks, leases, feedback and budgets (spec §0, §1, §2.1, §2.2, §2.4).
//
// Every function runs in the caller's transaction, which the caller opened with
// store.InScope. Functions that take an actor check the Permission Matrix role
// and the data scope themselves and return kerr errors; "not a member" and
// "outside the data scope" are kerr.NotFound. The scope each function expects:
//
//   - actor scope (actor.Scope()): every function that takes an actor, except
//     AssignTask and ReleaseLease.
//   - trusted scope (store.Runtime; the owner's scope also works): every
//     function without an actor that changes a task, a lease or a budget,
//     plus AssignTask and ReleaseLease. Row-level security does not let a Lead
//     read the assignee's membership nor a Worker update its task, so these two
//     run trusted and authorize the actor themselves.
//   - any scope that can see the row: TouchLease, AckContext, MarkWrote (the
//     holder may update its own lease) and EffectiveBudgetState (read only).
//
// Lock order, to stay free of deadlocks: task row, then lease, never a budget
// row after a task row; budget rows always TASK, then PROJECT, then SYSTEM.
package work

import (
	"encoding/json"
	"time"
)

// LeaseTTL is how long a lease lives without a hook event of its holder (T-H-20, T-H-23).
const LeaseTTL = 8 * time.Hour

// Limits are the ceilings of one budget. Nil means no limit on that dimension (§2.4).
type Limits struct {
	MaxCost         *int64 `json:"max_cost_microusd"`
	MaxInputTokens  *int64 `json:"max_input_tokens"`
	MaxOutputTokens *int64 `json:"max_output_tokens"`
	MaxToolCalls    *int64 `json:"max_tool_calls"`
	MaxTurns        *int64 `json:"max_turns"`
}

// Usage is an amount on the five measured dimensions. Cost is in micro-USD.
type Usage struct {
	Cost         int64 `json:"cost_microusd"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	ToolCalls    int64 `json:"tool_calls"`
	Turns        int64 `json:"turns"`
}

// Budget is one budget row. State is OK, WARN or EXHAUSTED.
type Budget struct {
	Level     string `json:"level"`
	ProjectID string `json:"project_id,omitempty"`
	TaskID    string `json:"task_id,omitempty"`
	Limits    Limits `json:"limits"`
	Used      Usage  `json:"used"`
	State     string `json:"state"`
}

// BudgetView is a task budget with the states above it (GET /tasks/{id}/budget).
type BudgetView struct {
	Task         Budget `json:"task"`
	ProjectState string `json:"project_state"`
	SystemState  string `json:"system_state"`
	Effective    string `json:"effective_state"` // worst of the three
}

// Project is a project with its limits, repo roots and PROJECT budget.
type Project struct {
	ID                 string   `json:"id"`
	Slug               string   `json:"slug"`
	Name               string   `json:"name"`
	MaxActiveLeases    *int     `json:"max_active_leases"`
	DefaultTaskMaxCost *int64   `json:"default_task_max_cost_microusd"`
	Repos              []string `json:"repos"`
	Budget             Budget   `json:"budget"`
}

// ProjectLimits is the body of PUT /projects/{p}/limits. It replaces all limits.
type ProjectLimits struct {
	MaxActiveLeases    *int   `json:"max_active_leases"`
	DefaultTaskMaxCost *int64 `json:"default_task_max_cost_microusd"`
	Budget             Limits `json:"budget"`
}

// Membership is the role of a principal in one project.
type Membership struct {
	ProjectID string `json:"project_id"`
	Role      string `json:"role"`
}

// Identity is a principal with its memberships and its live lease (GET /me).
type Identity struct {
	ID             string       `json:"id"`
	Kind           string       `json:"kind"`
	Name           string       `json:"name"`
	OpenBotAgentID string       `json:"openbot_agent_id,omitempty"`
	Enforcement    string       `json:"enforcement"`
	Memberships    []Membership `json:"memberships"`
	Lease          *Lease       `json:"lease,omitempty"`
}

// NewPrincipal is the input of CreatePrincipal: an agent with its first membership.
type NewPrincipal struct {
	Name           string `json:"name"`
	OpenBotAgentID string `json:"openbot_agent_id"`
	Enforcement    string `json:"enforcement"` // HOOK or SUPERVISED (default)
	ProjectID      string `json:"project_id"`
	Role           string `json:"role"` // lead or worker
}

// Check is one required check of a task; the runner executes Command inside
// its sandbox, without network unless Network is set (AC-S2-010).
type Check struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Network bool   `json:"network,omitempty"`
}

// NewTask is the input of CreateTask.
type NewTask struct {
	Key             string   `json:"key"`
	Title           string   `json:"title"`
	DesiredOutcome  string   `json:"desired_outcome"`
	Scope           string   `json:"scope"`
	RepoPath        string   `json:"repo_path"`
	AllowedPaths    []string `json:"allowed_paths"`
	ProtectedPaths  []string `json:"protected_paths"`
	RequiredChecks  []Check  `json:"required_checks"`
	RequirementKeys []string `json:"requirement_keys"`
	// Via names the path that created the task in the TASK_CREATED journal
	// payload ("delegate", spec S2 E.1-12). Never read from a request body.
	Via string `json:"-"`
}

// Task is one task row (§2.1).
type Task struct {
	ID                 string         `json:"id"`
	ProjectID          string         `json:"project_id"`
	Key                string         `json:"key"`
	Title              string         `json:"title"`
	DesiredOutcome     string         `json:"desired_outcome"`
	Scope              string         `json:"scope"`
	State              string         `json:"state"`
	Revision           int            `json:"revision"`
	RepoPath           string         `json:"repo_path"`
	AllowedPaths       []string       `json:"allowed_paths"`
	ProtectedPaths     []string       `json:"protected_paths"`
	RequiredChecks     []Check        `json:"required_checks"`
	RequirementKeys    []string       `json:"requirement_keys"`
	DependencyVersions map[string]int `json:"dependency_versions"`
	VerifiedCandidate  string         `json:"verified_candidate,omitempty"`
	BaseCommit         string         `json:"base_commit"`
	WaitingReasons     []string       `json:"waiting_reasons"` // ⊆ {budget, question, nonprogress}: WAITING_DECISION while not empty (spec S2 C.8)
	CreatedBy          string         `json:"created_by"`
	CreatedAt          time.Time      `json:"created_at"`
	UpdatedAt          time.Time      `json:"updated_at"`
}

// Lease is one lease row (§2.2).
type Lease struct {
	ID            string     `json:"id"`
	ProjectID     string     `json:"project_id"`
	TaskID        string     `json:"task_id"`
	PrincipalID   string     `json:"principal_id"`
	Epoch         int        `json:"epoch"`
	TaskRevision  int        `json:"task_revision"`
	RepoPath      string     `json:"repo_path"`
	Mode          string     `json:"mode"`
	State         string     `json:"state"`
	ContextAcked  bool       `json:"context_acked"`
	PendingNotice string     `json:"pending_notice"`
	WroteFiles    bool       `json:"wrote_files"`
	Reason        string     `json:"reason"`
	GrantedAt     time.Time  `json:"granted_at"`
	ExpiresAt     time.Time  `json:"expires_at"`
	EndedAt       *time.Time `json:"ended_at,omitempty"`
	// Intelligence Layer (spec S2 C.1, C.7): router outcome, switched-on
	// mechanisms, cost phase, steer text for the next context, and the
	// feedback refused once at the first write (AC-S2-039).
	IntelMode     string          `json:"intel_mode"`
	IntelSignals  json.RawMessage `json:"intel_signals"`
	IntelFeatures []string        `json:"intel_features"`
	IntelForced   bool            `json:"intel_forced"`
	IntelPhase    string          `json:"intel_phase"`
	IntelMark     json.RawMessage `json:"-"`
	IntelInbox    string          `json:"-"`
	WriteNotice   string          `json:"-"`
	due           bool            // expires_at < now() when the row was read (T-H-23)
}

// TaskView is a task with its live lease and budget (GET /tasks/{id}).
type TaskView struct {
	Task   Task       `json:"task"`
	Lease  *Lease     `json:"lease,omitempty"`
	Budget BudgetView `json:"budget"`
}

// Held is the live lease of a principal with its task, as the hooks need it.
type Held struct {
	Lease Lease `json:"lease"`
	Task  Task  `json:"task"`
}

// Change reports what one command did, so the caller can answer and notify.
type Change struct {
	Task           Task   `json:"task"`
	Ended          *Lease `json:"ended_lease,omitempty"`   // lease that ended in this call
	Granted        *Lease `json:"granted_lease,omitempty"` // lease that was granted in this call
	SourceID       string `json:"source_id,omitempty"`     // Feedback: the stored verbatim text
	ProjectChanged bool   `json:"project_changed"`         // AssignTask: the holder's previous lease was in another project (§0)
}
