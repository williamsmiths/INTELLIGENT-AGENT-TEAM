// Package verify is the trusted verification runner of the harness: it runs a
// task's required checks on the real working tree and produces evidence
// (spec PM-H-18, Feature E: AC-H-040..046; spec S2 part D: AC-S2-050..052,
// part A "Runner cô lập": AC-S2-010).
//
// What it guarantees:
//   - Evidence is bound to a candidate: the git tree hash of the working tree
//     (files tracked in the base commit plus every other file the trusted
//     ignore list does not match), taken before and after the checks. A tree
//     that moved while the checks ran makes every row of the run ERROR
//     "candidate changed during run" (AC-H-046).
//   - The snapshot trusts nothing an agent can write in the repository: the
//     base commit is read from the harness's own store (Store), never from
//     the repository's objects; the repository's .git/config, index, info
//     files, every .gitignore and .gitattributes in the tree and the OS user's
//     git configuration are not read (AC-S2-050).
//   - A tree that cannot be read completely is a failure, never "nothing
//     changed": CandidateHash and ChangedFiles return an error, Run returns
//     ERROR evidence (AC-S2-051).
//   - The built-in check "scope" compares every change since the task's
//     base_commit, committed or not, with allowed_paths / protected_paths, so a
//     write done through Bash is caught at verify time (limitation L4).
//   - PASS needs a real exit code 0 from a command that ran inside the bwrap
//     sandbox. Record only accepts a Result produced by Run in this process;
//     there is no entry point for a result claimed by someone else (AC-H-042).
//   - Every check runs under bubblewrap: read-only root, the repo writable,
//     /tmp and /run empty, the caller's secret directories hidden, no network
//     unless the check asks for it. Without a working sandbox the result is
//     ERROR "sandbox unavailable" (AC-S2-010).
//
// What it does NOT do — do not read more into it:
//   - The sandbox is a mount, pid, ipc and network namespace of the same OS
//     user. It hides only what the caller lists in HideDirs / HideFiles; every
//     other file that user can read stays readable (read-only).
//   - It does not decide task state. The caller applies T-H-03 / T-H-04.
//   - It does not know which files are acceptance tests (limitation L6).
//   - Checks run on the live working tree, not on an export (limitation L9).
//   - Files matched by the trusted ignore list, and anything under a
//     directory named .git (the root's or a nested repository's), are not
//     part of the candidate.
//   - A base commit with a submodule whose directory holds files is refused
//     (ErrSubmodule): the runner cannot hash a submodule's working tree.
//   - Single-flight (AC-H-045) is per Runner value, in process memory. It holds
//     only because the service is one process; it does not cover two harnessd
//     processes, and a request that arrives after a run finished runs again.
package verify

import (
	"crypto/rand"
	"fmt"
	"time"
)

// Evidence results (column evidence.result).
const (
	ResultPass  = "PASS"
	ResultFail  = "FAIL"
	ResultError = "ERROR"
)

const (
	// ScopeCheckID is the id of the built-in check; a task cannot define its own check with this id.
	ScopeCheckID = "scope"
	// ReasonCandidateChanged is the detail reason of every row of a run whose tree moved (AC-H-046).
	ReasonCandidateChanged = "candidate changed during run"
	// ReasonSnapshotFailed is the detail reason of every row of a run whose tree could not be read (AC-S2-051).
	ReasonSnapshotFailed = "snapshot failed"
	// ReasonNoSandbox is the detail reason of a check that could not be isolated (AC-S2-010).
	ReasonNoSandbox = "sandbox unavailable"
	// DefaultTimeout applies to a check without TimeoutSeconds.
	DefaultTimeout = 600 * time.Second
	// MaxLogBytes caps the captured output of one check; the rest is drained and dropped.
	MaxLogBytes = 4 << 20
)

// newID returns a random UUID (version 4) for an evidence row.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read does not return an error (Go 1.24+)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
