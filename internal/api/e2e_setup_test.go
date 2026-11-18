package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
)

// fakeHost records what the harness asks the host app to deliver. It stands in
// for OpenBot's automation API only; everything else in these tests is real
// (PostgreSQL, git, the HTTP server, the verification runner).
type fakeHost struct {
	mu     sync.Mutex
	sent   []string
	clears []string
	turns  map[string]string               // delivery id -> host turn id, set by a test (spec S2 C.7)
	fail   func(ctx context.Context) error // non-nil: every call runs it first; an error is the host's answer (REV-005 #4)
}

// failing makes every later host call run fn first (nil: the host answers again).
func (f *fakeHost) failing(fn func(ctx context.Context) error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fn
}

func (f *fakeHost) check(ctx context.Context) error {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if fail == nil {
		return nil
	}
	return fail(ctx)
}

func (f *fakeHost) Send(ctx context.Context, agent, text, _ string) (string, error) {
	if err := f.check(ctx); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, agent+": "+text)
	return fmt.Sprintf("dlv-%d", len(f.sent)), nil
}

func (f *fakeHost) Delivery(ctx context.Context, _, id string) (string, string, error) {
	if err := f.check(ctx); err != nil {
		return "", "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if turn := f.turns[id]; turn != "" {
		return "delivered", turn, nil
	}
	return "queued", "", nil
}

func (f *fakeHost) ClearContext(_ context.Context, agent string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clears = append(f.clears, agent)
	return nil
}

type env struct {
	t     *testing.T
	pool  *pgxpool.Pool
	url   string
	host  *fakeHost
	tmp   string // scratch root every lease holder may write
	owner string // tokens
	tok   map[string]string
	sum   map[string]string // registered settings hash of each HOOK agent (AC-S2-011)
	id    map[string]string // principal, project and task ids by name
	repo  map[string]string
	srv   *api.Server
}

// newEnv starts a server over a fresh database; opts adjust the server before it starts.
func newEnv(t *testing.T, opts ...func(*api.Server)) *env {
	t.Helper()
	pool := testdb.New(t)
	e := &env{t: t, pool: pool, host: &fakeHost{turns: map[string]string{}}, tok: map[string]string{}, sum: map[string]string{}, id: map[string]string{}, repo: map[string]string{}}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e.tmp = filepath.Join(base, "scratch")
	if err := os.MkdirAll(e.tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	token, hash, err := authn.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	e.owner = token
	if err := store.InScope(context.Background(), pool, store.Runtime, func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `INSERT INTO principal (kind,name,token_hash) VALUES ('owner','owner',$1)`, hash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	srv := &api.Server{Pool: pool, Runner: verify.NewRunner(), Notifier: e.host, LogDir: filepath.Join(base, "evidence"),
		TempRoots: []string{e.tmp}, AgentsRoot: filepath.Join(base, "agents"), PacketChars: 6000}
	for _, opt := range opts {
		opt(srv)
	}
	e.srv = srv
	ts := httptest.NewServer(srv.Routes())
	t.Cleanup(ts.Close)
	e.url = ts.URL
	for _, name := range []string{"alpha", "beta"} {
		e.repo[name] = e.gitRepo(filepath.Join(base, "repo-"+name))
	}
	return e
}

// gitRepo creates a real repository with one commit.
func (e *env) gitRepo(dir string) string {
	e.t.Helper()
	for _, d := range []string{"src", "docs", "tests/acceptance"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			e.t.Fatal(err)
		}
	}
	e.write(filepath.Join(dir, "src", "keep.txt"), "keep\n")
	e.write(filepath.Join(dir, "docs", "readme.md"), "docs\n")
	e.write(filepath.Join(dir, "tests", "acceptance", "oracle.txt"), "oracle\n")
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"add", "-A"},
		{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "-q", "-m", "base"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			e.t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func (e *env) write(path, content string) {
	e.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

// call sends one request and decodes the JSON answer.
func (e *env) call(tok, method, path string, body any, headers ...string) (int, map[string]any) {
	e.t.Helper()
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.url+path, rd)
	if err != nil {
		e.t.Fatal(err)
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	out["_raw"] = string(raw)
	return resp.StatusCode, out
}

// must is call + a status assertion.
func (e *env) must(want int, tok, method, path string, body any, headers ...string) map[string]any {
	e.t.Helper()
	status, out := e.call(tok, method, path, body, headers...)
	if status != want {
		e.t.Fatalf("%s %s: status %d, want %d: %s", method, path, status, want, out["_raw"])
	}
	return out
}

// hook posts a Claude hook event as an agent and returns the decision text ("" = no output).
func (e *env) hook(agent, event string, body map[string]any) string {
	e.t.Helper()
	// The hook client reports the effective config with every event (spec S2 A.4).
	sent := map[string]any{"settings_sha256": e.sum[agent]}
	for k, v := range body {
		sent[k] = v
	}
	body = sent
	out := e.must(http.StatusOK, e.tok[agent], http.MethodPost, "/v1/hooks/claude/"+event, body)
	if out["output"] == nil {
		return ""
	}
	raw, _ := json.Marshal(out["output"])
	return string(raw)
}

func (e *env) edit(agent, path string) string {
	return e.hook(agent, "pre-tool", map[string]any{"tool_name": "Edit", "tool_use_id": "pre-" + path,
		"cwd": e.repo["alpha"], "tool_input": map[string]any{"file_path": path}})
}

func str(m map[string]any, keys ...string) string {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func has(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Fatalf("missing %q in: %s", p, text)
		}
	}
}

func lacks(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if strings.Contains(text, p) {
			t.Fatalf("unexpected %q in: %s", p, text)
		}
	}
}
