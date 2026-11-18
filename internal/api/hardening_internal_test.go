package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// AC-S2-054: an answer PostgreSQL cannot keep (a \u0000 escape is not valid in
// jsonb) must not leave the key "in flight" forever. Real HTTP, real
// PostgreSQL, the real middleware chain; only the handler is a stand-in,
// because no route of the harness echoes a NUL byte any more.
func TestIdempotency_AC_S2_054_UnstorableAnswer(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	token, hash, err := authn.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ('owner', 'owner', $1)`, hash)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		writeJSON(w, http.StatusCreated, map[string]string{"echo": "before\x00after"})
	})
	ts := httptest.NewServer(authenticate(TokenAuth{Pool: pool})(idempotent(pool)(handler)))
	defer ts.Close()
	post := func() (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/echo", strings.NewReader(`{"a":1}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Idempotency-Key", "nul-body")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		return resp, strings.TrimSpace(string(raw))
	}

	first, body := post()
	if first.StatusCode != http.StatusCreated || !strings.Contains(body, `\u0000`) {
		t.Fatalf("first call: %d %s", first.StatusCode, body)
	}
	var status int
	var stored string
	if err := store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, body::text FROM idempotency WHERE key = 'nul-body'`).Scan(&status, &stored)
	}); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusCreated || stored != "null" {
		t.Fatalf("stored row: status %d body %s, want the real status with a null body", status, stored)
	}
	for i := 0; i < 2; i++ {
		again, body := post()
		// A created answer replays as 200 (AC-H-006); what matters here: it is answered, not 409.
		if again.StatusCode != http.StatusOK || again.Header.Get("Idempotent-Replay") != "true" || body != "null" {
			t.Fatalf("retry %d: %d %q replay=%q", i, again.StatusCode, body, again.Header.Get("Idempotent-Replay"))
		}
	}
	if calls != 1 {
		t.Fatalf("the handler ran %d times, want once", calls)
	}
}

// Spec S2 D.1 (REV-003 recheck #5): the lease lock fails closed. A resource
// that does not exist has no project; a database error is a 500, never "no
// project, let the request through".
func TestLeaseLock_S2_D1_DatabaseErrorIsNotNoProject(t *testing.T) {
	pool := testdb.New(t)
	s := &Server{Pool: pool}
	ctx := context.Background()
	const missing = "/v1/tasks/00000000-0000-4000-8000-000000000000/verify"
	if project, err := s.resourceProject(ctx, missing); project != "" || err != nil {
		t.Fatalf("missing task: project %q, err %v; want no project and no error", project, err)
	}
	if project, err := s.resourceProject(ctx, "/v1/me"); project != "" || err != nil {
		t.Fatalf("route without a resource: %q, %v", project, err)
	}

	pool.Close() // every query fails from here on
	if project, err := s.resourceProject(ctx, missing); err == nil {
		t.Fatalf("database error swallowed: project %q", project)
	}
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })
	agent := authn.Principal{ID: "11111111-1111-4111-8111-111111111111", Kind: "agent", Roles: map[string]string{}}
	req := httptest.NewRequest(http.MethodPost, missing, nil)
	rec := httptest.NewRecorder()
	s.leaseLock(next).ServeHTTP(rec, req.WithContext(context.WithValue(req.Context(), principalKey, agent)))
	if rec.Code != http.StatusInternalServerError || reached {
		t.Fatalf("status %d, handler reached = %v; want 500 and the handler not reached", rec.Code, reached)
	}
}
