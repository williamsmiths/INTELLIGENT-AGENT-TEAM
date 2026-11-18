// Package attesttest builds REAL process trees for the attestation tests
// (AC-S2-001..003): no kernel interface is mocked. A small helper binary
// (./helper) is built once per test process and copied to the paths that play
// the host app, the provider and the client. A test starts a copy as the top
// of a tree, and every level starts the next one until the last level acts —
// an HTTP request over a unix socket, or a command run as its child.
//
// A test package opts in with:
//
//	func TestMain(m *testing.M) { attesttest.Main(m) }
package attesttest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

const (
	envPlan   = "KC_ATTEST_PLAN"   // JSON Plan of the levels still to start
	envParent = "KC_ATTEST_PARENT" // pid of the level that started this one
	budget    = 60 * time.Second   // one tree never lives longer
)

// Step is one level of the tree.
type Step struct {
	Exe    string   `json:"exe,omitempty"`    // binary of this level (a Copy of the test binary); "" = same as the level above
	Dir    string   `json:"dir,omitempty"`    // working directory the level is started in
	Chdir  string   `json:"chdir,omitempty"`  // the level moves here once it runs
	Env    []string `json:"env,omitempty"`    // extra environment for this level and below
	Orphan bool     `json:"orphan,omitempty"` // the level above exits at once: this level is detached from the tree (double fork)
}

// Plan is a tree, top first, and what its last level does.
type Plan struct {
	Steps []Step `json:"steps"`
	// HTTP request over a unix socket, made by the last level.
	Socket string            `json:"socket,omitempty"`
	Method string            `json:"method,omitempty"`
	Path   string            `json:"path,omitempty"`
	Body   string            `json:"body,omitempty"`
	Header map[string]string `json:"header,omitempty"`
	// Or: a command run as a child of the last level, with Stdin.
	Exec  []string `json:"exec,omitempty"`
	Stdin string   `json:"stdin,omitempty"`
}

// Answer is what the last level printed.
type Answer struct {
	Status int    `json:"status"`          // HTTP status; 0 when the request failed
	Body   string `json:"body"`            // HTTP body, or stdout of Exec
	Error  string `json:"error,omitempty"` // why there is no status
	Exit   int    `json:"exit"`            // exit code of Exec
}

const helperPackage = "github.com/williamsmiths/kongcowork-harness/internal/attest/attesttest/helper"

var (
	buildOnce sync.Once
	buildDir  string // holds the helper binary of this test process
	buildErr  error
)

// Main runs the tests and removes the helper binary afterwards.
func Main(m *testing.M) {
	code := m.Run()
	if buildDir != "" {
		os.RemoveAll(buildDir)
	}
	os.Exit(code)
}

// helper builds the helper binary once per test process and returns its path.
func helper(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		if buildDir, buildErr = os.MkdirTemp("", "kc-helper"); buildErr != nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "go", "build", "-o", filepath.Join(buildDir, "helper"), helperPackage).CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build the helper: %v: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return filepath.Join(buildDir, "helper")
}

// Copy puts a copy of the helper binary at dir/name and returns its canonical
// path: a distinct executable for /proc/<pid>/exe.
func Copy(t testing.TB, dir, name string) string {
	t.Helper()
	src, err := os.Open(helper(t))
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	path := filepath.Join(dir, name)
	dst, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		dst.Close()
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// ShortDir is a temp directory with a short path: a unix socket path is
// limited to about 100 bytes, which t.TempDir() can exceed.
func ShortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return real
}

// Run starts the tree as a child of the test process and returns what its last level answered.
func Run(t testing.TB, p Plan) Answer {
	t.Helper()
	if len(p.Steps) == 0 || p.Steps[0].Exe == "" {
		t.Fatal("attesttest: the first step needs an executable")
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Steps[0].Exe)
	cmd.Dir = p.Steps[0].Dir
	cmd.Env = append(append(os.Environ(), p.Steps[0].Env...), envPlan+"="+string(raw), envParent+"="+strconv.Itoa(os.Getpid()))
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = 5 * time.Second
	out, err := cmd.Output() // returns when the last level closed stdout, even a detached one
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatalf("attesttest: tree did not run: %v", err)
	}
	var a Answer
	if json.Unmarshal(out, &a) != nil {
		t.Fatalf("attesttest: no answer from the tree (err %v): %q", err, out)
	}
	return a
}
