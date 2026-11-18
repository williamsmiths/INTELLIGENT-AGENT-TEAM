package verify

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Runner runs verifications and makes sure one (task, revision, candidate) is
// checked once at a time (AC-H-045). The guarantee lives in this value's
// memory: use one Runner per harnessd process. It does not coordinate two
// processes, and it does not cache: a request that arrives after a run has
// finished starts a new run.
type Runner struct {
	mu      sync.Mutex
	flights map[string]*flight
	joined  atomic.Int64 // callers that received another caller's run
}

type flight struct {
	done chan struct{}
	res  Result
	err  error
}

// NewRunner returns a Runner with no run in progress.
func NewRunner() *Runner { return &Runner{flights: map[string]*flight{}} }

var defaultRunner = NewRunner()

// Run verifies with the process-wide Runner. See Runner.Run.
func Run(ctx context.Context, req Request) (Result, error) { return defaultRunner.Run(ctx, req) }

// Run executes one verification:
//  1. hash the candidate;
//  2. run every required check (see runCheck);
//  3. run the built-in check "scope";
//  4. hash the candidate again; if it moved, every item becomes ERROR
//     "candidate changed during run" (AC-H-046).
//
// It returns an error only when the request is invalid or the run was
// cancelled; a failing check is a Result, not an error. A working tree that
// cannot be read completely — before or after the checks — is a Result too:
// every row is ERROR "snapshot failed", so the failure is recorded as evidence
// and the task cannot become VERIFIED (AC-S2-051).
// It does not write evidence and does not change task state: the caller calls
// Record and applies T-H-03 / T-H-04.
//
// A second call for the same task, revision and candidate while the first is
// still running waits and receives the first call's Result. If the first
// caller's context is cancelled, the waiting callers receive that error.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	if err := req.validate(); err != nil {
		return Result{}, err
	}
	candidate, err := CandidateHash(ctx, req.tree())
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("verify: run cancelled: %w", ctx.Err())
		}
		return snapshotFailed(req, Result{StartedAt: time.Now().UTC()}, err), nil
	}
	key := fmt.Sprintf("%s|%d|%s", req.TaskID, req.Revision, candidate)

	r.mu.Lock()
	if f, running := r.flights[key]; running {
		r.mu.Unlock()
		r.joined.Add(1)
		select {
		case <-f.done:
			return f.res, f.err
		case <-ctx.Done():
			return Result{}, fmt.Errorf("verify: waiting for the running verification: %w", ctx.Err())
		}
	}
	f := &flight{done: make(chan struct{})}
	r.flights[key] = f
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.flights, key)
		r.mu.Unlock()
		close(f.done)
	}()
	// If execute never returns normally, waiting callers get an error, not an empty PASS-less Result.
	f.err = fmt.Errorf("verify: run did not complete")
	f.res, f.err = execute(ctx, req, candidate)
	return f.res, f.err
}

func execute(ctx context.Context, req Request, candidate string) (Result, error) {
	res := Result{Candidate: candidate, StartedAt: time.Now().UTC()}
	for _, c := range req.Checks {
		res.Items = append(res.Items, runCheck(ctx, req, c))
	}
	if err := ctx.Err(); err != nil {
		return Result{}, fmt.Errorf("verify: run cancelled: %w", err)
	}
	// One snapshot serves the scope check and the closing hash, so the tree
	// that is judged for scope is exactly the tree that is compared.
	after, err := takeSnapshot(ctx, req.tree())
	if err != nil {
		if ctx.Err() != nil {
			return Result{}, fmt.Errorf("verify: run cancelled: %w", ctx.Err())
		}
		return snapshotFailed(req, res, err), nil
	}
	defer after.close()
	res.Items = append(res.Items, scopeItem(ctx, req, after))

	res.CandidateChanged = after.tree != candidate
	res.AllPass = !res.CandidateChanged
	for i := range res.Items {
		it := &res.Items[i]
		if res.CandidateChanged {
			it.Detail["observed_result"] = it.Result
			it.Detail["reason"] = ReasonCandidateChanged
			it.Detail["candidate_after"] = after.tree
			it.Result = ResultError
		}
		if it.Result != ResultPass || it.ExitCode == nil || *it.ExitCode != 0 {
			res.AllPass = false
		}
	}
	res.FinishedAt = time.Now().UTC()
	res.sealedFor = req.sealKey()
	return res, nil
}

// snapshotFailed turns a run whose tree could not be read into ERROR rows:
// the checks that already ran keep their observed result in the detail, the
// ones that did not run and the built-in "scope" check get a row of their own.
func snapshotFailed(req Request, res Result, cause error) Result {
	now := time.Now().UTC()
	done := map[string]bool{}
	for i := range res.Items {
		it := &res.Items[i]
		done[it.CheckID] = true
		it.Detail["observed_result"] = it.Result
		it.Result, it.ExitCode = ResultError, nil
	}
	missing := append([]Check{}, req.Checks...)
	missing = append(missing, Check{ID: ScopeCheckID, Command: "scope: diff since " + req.BaseCommit})
	for _, c := range missing {
		if !done[c.ID] {
			res.Items = append(res.Items, Item{ID: newID(), CheckID: c.ID, Command: c.Command, Result: ResultError,
				Detail: map[string]any{}, StartedAt: now, FinishedAt: now})
		}
	}
	for i := range res.Items {
		res.Items[i].Detail["reason"] = ReasonSnapshotFailed
		res.Items[i].Detail["error"] = clip(cause.Error())
	}
	res.AllPass = false
	res.FinishedAt = now
	res.sealedFor = req.sealKey()
	return res
}
