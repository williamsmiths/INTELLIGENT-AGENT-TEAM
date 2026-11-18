package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// AC-H-040, AC-S2-010: a timeout is ERROR without an exit code, and nothing
// the check started survives it — not even a process that left the process
// group, because the whole pid namespace of the sandbox dies with bwrap.
func TestRun_AC_H_040_TimeoutKillsTheProcessTree(t *testing.T) {
	repo, base := newRepo(t)
	req := request(t, repo, base, Check{ID: "slow", TimeoutSeconds: 1,
		Command: `(setsid bash -c 'while true; do echo beat >> "$HOME/.cache/beat"; sleep 0.05; done' &) ; sleep 300 & wait`})
	req.CacheDir = t.TempDir() // the one place a check can write that the test can read
	beat := filepath.Join(req.CacheDir, "beat")
	start := time.Now()
	res := run(t, req)
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("run took %s; the timeout did not stop the check", elapsed)
	}
	slow := item(t, res, "slow")
	if slow.Result != ResultError || slow.ExitCode != nil || slow.Detail["reason"] != "timeout" || slow.Detail["sandbox"] != "bwrap" {
		t.Fatalf("slow: %s exit %v detail %v", slow.Result, slow.ExitCode, slow.Detail)
	}
	if res.AllPass {
		t.Fatal("AllPass after a timeout")
	}
	size := func() int64 {
		info, err := os.Stat(beat)
		if err != nil {
			t.Fatalf("the check never started its detached child: %v", err)
		}
		return info.Size()
	}
	if size() == 0 {
		t.Fatal("the detached child never wrote")
	}
	// The detached writer appends every 50 ms while it lives.
	time.Sleep(300 * time.Millisecond)
	before := size()
	time.Sleep(500 * time.Millisecond)
	if after := size(); after != before {
		t.Fatalf("a process of the check is still alive: the file grew from %d to %d bytes", before, after)
	}
}

// AC-H-040: a command that cannot start is ERROR without an exit code, never PASS.
func TestRunCheck_AC_H_040_FailedStartIsError(t *testing.T) {
	req := Request{TaskID: "task-1", Repo: filepath.Join(t.TempDir(), "missing"), LogDir: t.TempDir()}
	it := runCheck(context.Background(), req, Check{ID: "unit", Command: "true"})
	if it.Result != ResultError || it.ExitCode != nil || it.Detail["reason"] != "command did not start" {
		t.Fatalf("%s exit %v detail %v", it.Result, it.ExitCode, it.Detail)
	}
}

func TestRun_AC_H_040_OutputIsCapped(t *testing.T) {
	repo, base := newRepo(t)
	res := run(t, request(t, repo, base, Check{ID: "noisy", Command: fmt.Sprintf("head -c %d /dev/zero", MaxLogBytes+1_000_000)}))
	noisy := item(t, res, "noisy")
	if noisy.Result != ResultPass || noisy.Detail["log_truncated"] != true {
		t.Fatalf("noisy: %s %v", noisy.Result, noisy.Detail)
	}
	info, err := os.Stat(noisy.LogPath)
	if err != nil || info.Size() > MaxLogBytes+200 {
		t.Fatalf("log size %v, err %v", info.Size(), err)
	}
	if sum, _ := fileSHA(t, noisy.LogPath); sum != noisy.LogSHA256 {
		t.Fatal("log_sha256 does not match the truncated log file")
	}
}

// AC-H-045: two verify calls for the same task and candidate run the checks once.
func TestRun_AC_H_045_ConcurrentRunsExecuteOnce(t *testing.T) {
	repo, base := newRepo(t)
	outside := t.TempDir() // bound at $HOME/.cache inside the sandbox: the check and the test meet there
	counter, release := filepath.Join(outside, "runs.txt"), filepath.Join(outside, "release")
	cmd := `echo ran >> "$HOME/.cache/runs.txt"; while [ ! -e "$HOME/.cache/release" ]; do sleep 0.05; done`
	req := request(t, repo, base, Check{ID: "unit", Command: cmd, TimeoutSeconds: 60})
	req.CacheDir = outside
	runner := NewRunner()

	results := make([]Result, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	call := func(i int) {
		defer wg.Done()
		results[i], errs[i] = runner.Run(context.Background(), req)
	}
	wg.Add(2)
	go call(0)
	waitFor(t, "the first run to start its check", func() bool { _, err := os.Stat(counter); return err == nil })
	go call(1)
	waitFor(t, "the second caller to join the first run", func() bool { return runner.joined.Load() == 1 })
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	raw, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(raw), "ran\n"); got != 1 {
		t.Fatalf("the check ran %d times, want 1", got)
	}
	a, b := results[0], results[1]
	if !a.AllPass || !b.AllPass || len(a.Items) != 2 || len(b.Items) != 2 {
		t.Fatalf("results: %+v / %+v", a, b)
	}
	for i := range a.Items {
		if a.Items[i].ID != b.Items[i].ID || a.Items[i].LogPath != b.Items[i].LogPath || a.Items[i].LogSHA256 != b.Items[i].LogSHA256 {
			t.Fatalf("the second caller did not receive the first run's evidence: %+v / %+v", a.Items[i], b.Items[i])
		}
	}
	// The flight is gone: a later request runs again (documented limit of the in-process single-flight).
	if _, err := runner.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(counter)
	if got := strings.Count(string(raw), "ran\n"); got != 2 {
		t.Fatalf("after the first run finished the check ran %d times in total, want 2", got)
	}
}
