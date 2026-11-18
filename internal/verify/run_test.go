package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

func run(t *testing.T, req Request) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	res, err := NewRunner().Run(ctx, req)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func fileSHA(t *testing.T, path string) (sum string, content string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:]), string(raw)
}

func TestRun_AC_H_040_PassNeedsRealExitCodeZero(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "src/a.go", "package a // change inside allowed_paths\n")
	write(t, repo, "src/new.go", "package a // untracked, inside allowed_paths\n")
	t.Setenv("KCH_FAKE_API_TOKEN", "must-not-reach-the-check")
	req := request(t, repo, base,
		Check{ID: "unit", Command: "test -f src/new.go && echo hello-from-check && pwd"},
		Check{ID: "env", Command: `test -z "$KCH_FAKE_API_TOKEN" && test -z "$HARNESS_TEST_DSN" && test -n "$PATH"; echo "env-exit=$?"; env | grep -c -i -E 'TOKEN|KEY|SECRET|PASSWORD' ; true`},
	)
	res := run(t, req)

	if !res.AllPass || res.CandidateChanged {
		t.Fatalf("AllPass=%v CandidateChanged=%v items=%+v", res.AllPass, res.CandidateChanged, res.Items)
	}
	if want := hashOf(t, repo); res.Candidate != want {
		t.Fatalf("candidate %s, working tree %s", res.Candidate, want)
	}
	if len(res.Items) != 3 || res.Items[2].CheckID != ScopeCheckID {
		t.Fatalf("items = %+v", res.Items)
	}
	for _, it := range res.Items {
		if it.Result != ResultPass || it.ExitCode == nil || *it.ExitCode != 0 {
			t.Fatalf("%s: result %s exit %v detail %v", it.CheckID, it.Result, it.ExitCode, it.Detail)
		}
		sum, _ := fileSHA(t, it.LogPath)
		if it.LogSHA256 == "" || it.LogSHA256 != sum {
			t.Fatalf("%s: log_sha256 %q, file %q", it.CheckID, it.LogSHA256, sum)
		}
		name := filepath.Base(it.LogPath)
		if filepath.Dir(it.LogPath) != req.LogDir || !strings.HasPrefix(name, "task-1-"+it.CheckID+"-") {
			t.Fatalf("log path %s", it.LogPath)
		}
		if info, err := os.Stat(it.LogPath); err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("log mode: %v %v", info, err)
		}
	}
	_, unit := fileSHA(t, item(t, res, "unit").LogPath)
	if !strings.Contains(unit, "hello-from-check") || !strings.Contains(unit, repo) {
		t.Fatalf("unit log does not show the command ran in the repo: %q", unit)
	}
	_, env := fileSHA(t, item(t, res, "env").LogPath)
	if !strings.Contains(env, "env-exit=0") || !strings.HasSuffix(strings.TrimSpace(env), "\n0") {
		t.Fatalf("secret-looking variables reached the check: %q", env)
	}
	if got := item(t, res, ScopeCheckID).Command; got != "scope: diff since "+base {
		t.Fatalf("scope command %q", got)
	}
}

func TestRun_AC_H_041_CheckExitsOne(t *testing.T) {
	repo, base := newRepo(t)
	res := run(t, request(t, repo, base, Check{ID: "unit", Command: "echo failing >&2; exit 1"}, Check{ID: "lint", Command: "true"}))
	unit := item(t, res, "unit")
	if unit.Result != ResultFail || unit.ExitCode == nil || *unit.ExitCode != 1 {
		t.Fatalf("unit: %s exit %v", unit.Result, unit.ExitCode)
	}
	if _, log := fileSHA(t, unit.LogPath); !strings.Contains(log, "failing") {
		t.Fatalf("stderr not captured: %q", log)
	}
	if item(t, res, "lint").Result != ResultPass || item(t, res, ScopeCheckID).Result != ResultPass {
		t.Fatalf("other checks must still run and pass: %+v", res.Items)
	}
	if res.AllPass {
		t.Fatal("AllPass with a failing check")
	}
}

func TestRun_AC_H_041_WriteOutsideAllowedPathsFailsScope(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "docs/sneaked.md", "written through Bash, no hook saw it\n")
	write(t, repo, ".claude/settings.json", "{}\n")
	res := run(t, request(t, repo, base, Check{ID: "unit", Command: "true"}))
	scope := item(t, res, ScopeCheckID)
	if scope.Result != ResultFail || scope.ExitCode == nil || *scope.ExitCode != 1 {
		t.Fatalf("scope: %s exit %v detail %v", scope.Result, scope.ExitCode, scope.Detail)
	}
	if got, want := scope.Detail["violations"], []string{".claude/settings.json", "docs/sneaked.md"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("violations = %v, want %v", got, want)
	}
	if item(t, res, "unit").Result != ResultPass || res.AllPass {
		t.Fatalf("unit must pass and AllPass must be false: %+v", res)
	}
}

func TestRun_AC_H_046_CommitOutsideAllowedPathsAfterBase(t *testing.T) {
	repo, base := newRepo(t)
	write(t, repo, "README.md", "changed and committed after base\n")
	gitRun(t, repo, "commit", "-q", "-am", "outside scope")
	if status := gitRun(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("tree not clean: %q", status)
	}
	res := run(t, request(t, repo, base, Check{ID: "unit", Command: "true"}))
	scope := item(t, res, ScopeCheckID)
	if scope.Result != ResultFail || !reflect.DeepEqual(scope.Detail["violations"], []string{"README.md"}) {
		t.Fatalf("scope must fail against base_commit, not HEAD: %s %v", scope.Result, scope.Detail)
	}
	if res.AllPass {
		t.Fatal("AllPass with a scope violation")
	}
}

func TestRun_AC_H_046_CandidateChangedDuringRun(t *testing.T) {
	repo, base := newRepo(t)
	before := hashOf(t, repo)
	res := run(t, request(t, repo, base, Check{ID: "unit", Command: "echo '// touched by the check' >> src/a.go"}))
	if !res.CandidateChanged || res.AllPass || res.Candidate != before {
		t.Fatalf("changed=%v allPass=%v candidate=%s before=%s", res.CandidateChanged, res.AllPass, res.Candidate, before)
	}
	for _, it := range res.Items {
		if it.Result != ResultError || it.Detail["reason"] != ReasonCandidateChanged {
			t.Fatalf("%s: %s %v", it.CheckID, it.Result, it.Detail)
		}
	}
	if got := item(t, res, "unit").Detail["observed_result"]; got != ResultPass {
		t.Fatalf("observed_result = %v", got)
	}
}

func TestRun_RejectsInvalidRequest(t *testing.T) {
	repo, base := newRepo(t)
	ok := Check{ID: "unit", Command: "true"}
	cases := map[string]func(*Request){
		"reserved check id":   func(r *Request) { r.Checks = []Check{{ID: ScopeCheckID, Command: "true"}} },
		"repeated check id":   func(r *Request) { r.Checks = []Check{ok, ok} },
		"no checks":           func(r *Request) { r.Checks = nil },
		"empty command":       func(r *Request) { r.Checks = []Check{{ID: "unit", Command: " "}} },
		"log dir inside repo": func(r *Request) { r.LogDir = filepath.Join(r.Repo, "logs") },
		"base is not a hash":  func(r *Request) { r.BaseCommit = "main" },
		"relative repo":       func(r *Request) { r.Repo = "repo" },
		"no runner id":        func(r *Request) { r.RunnerID = "" },
	}
	for name, breakIt := range cases {
		req := request(t, repo, base, ok)
		breakIt(&req)
		if _, err := NewRunner().Run(context.Background(), req); !kerr.Is(err, kerr.Invalid) {
			t.Errorf("%s: err = %v, want Invalid", name, err)
		}
	}
}
