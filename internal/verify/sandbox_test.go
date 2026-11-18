package verify

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// outsideTmp creates a directory that is not below /tmp or /run, so that only
// HideDirs — not the sandbox's own tmpfs on /tmp — can hide it.
func outsideTmp(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/var/tmp", "kch-sandbox-test-")
	if err != nil {
		t.Skipf("no directory outside /tmp available: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// listener is a real TCP server on 127.0.0.1 that counts connections.
func listener(t *testing.T) (port int, accepted *atomic.Int64) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	accepted = &atomic.Int64{}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			_ = c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, accepted
}

func logOf(t *testing.T, it Item) string {
	t.Helper()
	_, text := fileSHA(t, it.LogPath)
	return text
}

// AC-S2-010: every check runs inside bwrap; secrets, the network and /run are
// out of reach, the repo and the per-project cache are writable, and a check
// that cannot be isolated is ERROR, never PASS.
func TestRun_AC_S2_010_Sandbox(t *testing.T) {
	hostNet, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		t.Fatal(err)
	}
	t.Run("AC-S2-010 hidden secrets, no network, empty /run and /tmp", func(t *testing.T) {
		repo, base := newRepo(t)
		secrets := outsideTmp(t)
		write(t, secrets, "kc-harness/harnessd.env", "HARNESS_DSN=TOP-SECRET-IN-DIR\n")
		write(t, secrets, "creds.json", "TOP-SECRET-IN-FILE\n")
		write(t, secrets, "plain.txt", "not-a-secret\n")
		write(t, os.TempDir(), filepath.Base(secrets)+".txt", "TOP-SECRET-IN-TMP\n") // a neighbour of the repo in /tmp
		t.Cleanup(func() { _ = os.Remove(filepath.Join(os.TempDir(), filepath.Base(secrets)+".txt")) })
		port, accepted := listener(t)
		if entries, err := os.ReadDir("/run"); err != nil || len(entries) == 0 {
			t.Skipf("the host's /run is empty or unreadable (%v): nothing to compare with", err)
		}
		cmd := fmt.Sprintf(`cat '%[1]s/kc-harness/harnessd.env'; echo "dir=$?"
echo "file=[$(cat '%[1]s/creds.json')]"; echo "plain=[$(cat '%[1]s/plain.txt')]"
(exec 3<>/dev/tcp/127.0.0.1/%[2]d) 2>/dev/null; echo "listener=$?"
(exec 3<>/dev/tcp/127.0.0.1/5439) 2>/dev/null; echo "postgres=$?"
echo "run=[$(ls -A /run)]"; echo "tmp=[$(ls -A /tmp)]"; cat /tmp/*.txt; echo "net=$(readlink /proc/self/ns/net)"; echo "pid=$$"`, secrets, port)
		req := request(t, repo, base, Check{ID: "probe", Command: cmd})
		req.HideDirs = []string{filepath.Join(secrets, "kc-harness"), filepath.Join(secrets, "does-not-exist")}
		req.HideFiles = []string{filepath.Join(secrets, "creds.json"), filepath.Join(secrets, "missing.json")}
		res := run(t, req)
		probe := item(t, res, "probe")
		out := logOf(t, probe)
		if probe.Result != ResultPass || probe.Detail["sandbox"] != "bwrap" || probe.Detail["network"] != false || !res.AllPass {
			t.Fatalf("probe: %s %v\n%s", probe.Result, probe.Detail, out)
		}
		for _, want := range []string{"dir=1", "file=[]", "plain=[not-a-secret]", "listener=1", "postgres=1", "run=[]",
			"tmp=[" + strings.Split(strings.TrimPrefix(repo, os.TempDir()+"/"), "/")[0] + "]"} { // /tmp holds the way to the repo, nothing else
			if !strings.Contains(out, want+"\n") {
				t.Errorf("missing %q in the check's output:\n%s", want, out)
			}
		}
		if strings.Contains(out, "TOP-SECRET") || strings.Contains(out, "net="+hostNet) || accepted.Load() != 0 {
			t.Fatalf("the check reached a secret or the host network (connections: %d):\n%s", accepted.Load(), out)
		}
		if !strings.Contains(out, "pid=2\n") {
			t.Errorf("the check does not run in its own pid namespace:\n%s", out)
		}
	})
	t.Run("AC-S2-010 repo and per-project cache are writable, the rest is read-only", func(t *testing.T) {
		repo, base := newRepo(t)
		marker := fmt.Sprintf("kch-sandbox-test-%d", os.Getpid())
		cmd := fmt.Sprintf(`mkdir -p build && echo made > build/out.txt && echo cached > "$HOME/.cache/%[1]s" || exit 1
touch /etc/%[1]s 2>/dev/null; echo "etc=$?"; touch "$HOME/%[1]s" 2>/dev/null; echo "home=$?"`, marker)
		req := request(t, repo, base, Check{ID: "write", Command: cmd})
		req.CacheDir = filepath.Join(t.TempDir(), "cache", "project-1")
		res := run(t, req)
		out := logOf(t, item(t, res, "write"))
		if !res.AllPass || !strings.Contains(out, "etc=1\n") || !strings.Contains(out, "home=1\n") {
			t.Fatalf("allPass=%v items=%+v\n%s", res.AllPass, res.Items, out)
		}
		if _, text := fileSHA(t, filepath.Join(repo, "build/out.txt")); text != "made\n" {
			t.Fatalf("the write into the repo did not reach the real repo: %q", text)
		}
		if _, text := fileSHA(t, filepath.Join(req.CacheDir, marker)); text != "cached\n" {
			t.Fatalf("the write into ~/.cache did not reach the project cache: %q", text)
		}
		home, _ := os.UserHomeDir()
		for _, p := range []string{filepath.Join(home, ".cache", marker), filepath.Join(home, marker), "/etc/" + marker} {
			if _, err := os.Lstat(p); err == nil {
				_ = os.Remove(p)
				t.Fatalf("the check wrote %s on the host", p)
			}
		}
	})
	t.Run("AC-S2-010 network: true shares the host network namespace", func(t *testing.T) {
		repo, base := newRepo(t)
		port, accepted := listener(t)
		cmd := fmt.Sprintf(`echo "net=$(readlink /proc/self/ns/net)"; (exec 3<>/dev/tcp/127.0.0.1/%d) && echo listener=ok`, port)
		res := run(t, request(t, repo, base, Check{ID: "online", Command: cmd, Network: true}, Check{ID: "offline", Command: cmd + "; true"}))
		online, offline := item(t, res, "online"), item(t, res, "offline")
		if out := logOf(t, online); online.Result != ResultPass || online.Detail["network"] != true ||
			!strings.Contains(out, "net="+hostNet+"\n") || !strings.Contains(out, "listener=ok") {
			t.Fatalf("online: %s %v\n%s", online.Result, online.Detail, out)
		}
		if out := logOf(t, offline); strings.Contains(out, "net="+hostNet) || strings.Contains(out, "listener=ok") {
			t.Fatalf("the check without network: true reached the host network:\n%s", out)
		}
		if accepted.Load() != 1 {
			t.Fatalf("the listener saw %d connections, want exactly the one of the online check", accepted.Load())
		}
	})
	t.Run("AC-S2-010 repo below a hidden directory stays usable", func(t *testing.T) {
		parent := outsideTmp(t)
		repo := filepath.Join(parent, "repo")
		write(t, parent, "sibling/secret.txt", "TOP-SECRET-SIBLING\n")
		write(t, repo, "src/a.go", "package a\n")
		gitRun(t, repo, "init", "-q", "-b", "main")
		gitRun(t, repo, "add", "-A")
		gitRun(t, repo, "commit", "-q", "-m", "base")
		base := gitRun(t, repo, "rev-parse", "HEAD")
		pin(t, repo, base)
		req := request(t, repo, base, Check{ID: "unit", Command: fmt.Sprintf("test -f src/a.go && ! cat '%s/sibling/secret.txt'", parent)})
		req.HideDirs = []string{parent}
		if res := run(t, req); !res.AllPass || strings.Contains(logOf(t, item(t, res, "unit")), "TOP-SECRET") {
			t.Fatalf("%+v", res.Items)
		}
	})
	t.Run("AC-S2-010 no bwrap: ERROR sandbox unavailable, the command never runs", func(t *testing.T) {
		repo, base := newRepo(t)
		// A program that ends at once, and one that would run the command without any isolation.
		direct := filepath.Join(t.TempDir(), "not-a-sandbox")
		if err := os.WriteFile(direct, []byte("#!/bin/bash\nwhile [ \"$1\" != -- ]; do shift; done; shift; exec \"$@\" 3>&-\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		for name, bwrap := range map[string]string{"missing": "/nonexistent/bwrap", "exits": "/bin/false", "directory": t.TempDir(), "unisolated": direct} {
			req := request(t, repo, base, Check{ID: "unit", Command: "echo ran-unisolated; true"})
			req.Bwrap = bwrap
			res, err := NewRunner().Run(context.Background(), req)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			unit := item(t, res, "unit")
			if unit.Result != ResultError || unit.ExitCode != nil || unit.Detail["reason"] != ReasonNoSandbox || unit.Detail["sandbox"] != "none" || res.AllPass {
				t.Errorf("%s: %s exit %v detail %v allPass=%v", name, unit.Result, unit.ExitCode, unit.Detail, res.AllPass)
			}
			if name != "unisolated" && strings.Contains(logOf(t, unit), "ran-unisolated") {
				t.Errorf("%s: the command ran without a sandbox", name)
			}
		}
	})
}
