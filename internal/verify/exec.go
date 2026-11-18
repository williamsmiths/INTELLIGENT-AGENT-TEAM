package verify

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// checkEnv is the whole environment of a check: PATH, HOME and LANG. Nothing
// else of harnessd's environment is passed, and a name that looks like a
// secret is refused even if it is ever added to the list.
func checkEnv() []string {
	env := []string{}
	for _, k := range []string{"PATH", "HOME", "LANG"} {
		if v, ok := os.LookupEnv(k); ok && !looksSecret(k) {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func looksSecret(name string) bool {
	name = strings.ToUpper(name)
	for _, word := range []string{"TOKEN", "KEY", "SECRET", "PASSWORD"} {
		if strings.Contains(name, word) {
			return true
		}
	}
	return false
}

// runCheck runs one command with `bash -c` in the repo, inside the bwrap
// sandbox (see sandboxArgs), in its own process group, and kills everything at
// the timeout: bwrap is the group leader, and with --die-with-parent and its
// own pid namespace nothing inside outlives it. PASS needs a real exit code 0
// from a command that really started inside the sandbox; a timeout or a failed
// start is ERROR without an exit code; a sandbox that cannot be set up is
// ERROR "sandbox unavailable" — a check never runs unisolated (AC-S2-010);
// any other exit code is FAIL (AC-H-040, AC-H-041).
func runCheck(ctx context.Context, req Request, c Check) Item {
	it := Item{ID: newID(), CheckID: c.ID, Command: c.Command, Result: ResultError,
		Detail: map[string]any{"sandbox": "none", "network": c.Network}, StartedAt: time.Now().UTC()}
	fail := func(reason string, err error) Item {
		it.Detail["reason"] = reason
		if err != nil {
			it.Detail["error"] = clip(err.Error())
		}
		it.FinishedAt = time.Now().UTC()
		return it
	}
	log, err := openLog(req.LogDir, req.TaskID, c.ID, it.StartedAt)
	if err != nil {
		return fail("log file unavailable", err)
	}
	it.LogPath = log.path
	closeLog := func() { it.LogSHA256, _ = log.finish() }
	if st, err := os.Stat(req.Repo); err != nil || !st.IsDir() {
		closeLog()
		return fail("command did not start", errors.New("the repo root is not a directory"))
	}
	timeout := DefaultTimeout
	if c.TimeoutSeconds > 0 {
		timeout = time.Duration(c.TimeoutSeconds) * time.Second
	}
	bwrap, args, err := sandboxArgs(req, c)
	if err != nil {
		closeLog()
		return fail(ReasonNoSandbox, err)
	}
	// The wrapper writes to fd 3 from inside the sandbox just before it becomes
	// the check: that byte pair is the proof that bwrap finished its setup.
	ready, readyW, err := os.Pipe()
	if err != nil {
		closeLog()
		return fail(ReasonNoSandbox, err)
	}
	defer ready.Close()
	cmd := exec.Command(bwrap, args...)
	cmd.Env = checkEnv()
	cmd.Stdout, cmd.Stderr = log, log
	cmd.ExtraFiles = []*os.File{readyW}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A background child that keeps the output pipe open cannot hold Wait for long.
	cmd.WaitDelay = 2 * time.Second
	started, done := make(chan error, 1), make(chan error, 1)
	go func() {
		// bwrap --die-with-parent dies with the OS thread that started it, so that thread is kept until bwrap ends.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		started <- err
		if err == nil {
			done <- cmd.Wait()
		}
	}()
	err = <-started
	_ = readyW.Close()
	if err != nil {
		closeLog()
		return fail(ReasonNoSandbox, err)
	}
	inside := make(chan bool, 1)
	go func() {
		buf := make([]byte, len(readyMark))
		n, _ := io.ReadFull(ready, buf)
		inside <- n == len(readyMark) && string(buf) == readyMark
	}()
	killGroup := func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr error
	stopped := ""
	select {
	case waitErr = <-done:
	case <-timer.C:
		stopped = "timeout"
		killGroup()
		waitErr = <-done
	case <-ctx.Done():
		stopped = "cancelled"
		killGroup()
		waitErr = <-done
	}
	killGroup() // nothing the check started may outlive it and touch the tree later
	var ran bool
	select {
	case ran = <-inside:
	case <-time.After(2 * time.Second):
		_ = ready.Close() // unblocks the reader
		ran = <-inside
	}
	sum, logErr := log.finish()
	it.LogSHA256 = sum
	if log.truncated {
		it.Detail["log_truncated"] = true
	}
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		it.Detail["leftover_processes"] = true
	}
	if !ran { // bwrap ended before the check began: its message is in the log
		return fail(ReasonNoSandbox, errors.New("bwrap did not start the check"))
	}
	it.Detail["sandbox"] = "bwrap"
	state := cmd.ProcessState
	switch {
	case stopped == "timeout":
		it.Detail["timeout_seconds"] = int(timeout / time.Second)
		return fail("timeout", nil)
	case stopped != "":
		return fail(stopped, ctx.Err())
	case logErr != nil:
		return fail("log write failed", logErr)
	case state == nil || !state.Exited():
		return fail("command ended without an exit code", waitErr)
	}
	code := state.ExitCode()
	it.ExitCode = &code
	it.Result = ResultFail
	if code == 0 {
		it.Result = ResultPass
	}
	it.FinishedAt = time.Now().UTC()
	return it
}
