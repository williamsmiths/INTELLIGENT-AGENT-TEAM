package api_test

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/openbot"
)

// Spec S2 E.9 over HTTP. The host database is a fixture SQLite file with the
// real table and column names of OpenBot (channel-schema.ts,
// agent-thread-schema.ts, openbot-database-schema.ts); the real openbot.db is
// never opened.

type hostDB struct {
	t    *testing.T
	path string
	db   *sql.DB
}

func newHostDB(t *testing.T) *hostDB {
	t.Helper()
	h := &hostDB{t: t, path: filepath.Join(t.TempDir(), "openbot.db")}
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db
	h.exec(`CREATE TABLE projection_channel_contexts (channel_id TEXT NOT NULL, agent_id TEXT NOT NULL,
		thread_id TEXT NOT NULL UNIQUE, session_id TEXT, through_sequence INTEGER NOT NULL DEFAULT 0,
		summary_version INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(channel_id, agent_id))`)
	h.exec(`CREATE TABLE projection_provider_sessions (id TEXT PRIMARY KEY, thread_id TEXT NOT NULL,
		provider TEXT NOT NULL CHECK(provider IN ('codex', 'claude', 'grok')), external_session_id TEXT NOT NULL,
		model TEXT NOT NULL, effort TEXT NOT NULL, state TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		resume_cursor TEXT, last_event_sequence INTEGER NOT NULL, UNIQUE(provider, external_session_id))`)
	h.exec(`CREATE TABLE projection_agent_threads (thread_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL, legacy_task_id TEXT UNIQUE,
		origin TEXT NOT NULL, title TEXT NOT NULL, title_source TEXT NOT NULL, outcome TEXT, error TEXT,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL, archived_at TEXT, last_event_sequence INTEGER NOT NULL)`)
	h.exec(`CREATE TABLE projection_agents (agent_id TEXT PRIMARY KEY, thread_id TEXT, model TEXT NOT NULL, updated_at TEXT,
		sort_order INTEGER NOT NULL, agent_json TEXT NOT NULL, last_event_sequence INTEGER NOT NULL)`)
	return h
}

func (h *hostDB) exec(q string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(q, args...); err != nil {
		h.t.Fatal(err)
	}
}

// channel: the agent's context in a channel runs in Claude session.
func (h *hostDB) channel(channel, agent, session string) {
	h.exec(`INSERT INTO projection_channel_contexts (channel_id, agent_id, thread_id, session_id) VALUES (?, ?, ?, ?)`,
		channel, "agent-"+agent, "ctx-"+channel+"-"+agent, session)
}

// mainThread: thread is the agent's own chat and runs in Claude session.
func (h *hostDB) mainThread(agent, thread, session string) {
	h.exec(`INSERT INTO projection_agents (agent_id, thread_id, model, sort_order, agent_json, last_event_sequence)
		VALUES (?, ?, 'claude-x', 0, '{}', 0)`, "agent-"+agent, thread)
	h.session(thread, session)
}

func (h *hostDB) session(thread, session string) {
	h.exec(`INSERT INTO projection_provider_sessions (id, thread_id, provider, external_session_id, model, effort, state,
		created_at, updated_at, last_event_sequence) VALUES (?, ?, 'claude', ?, 'm', 'e', 'active', 'now', 'now', 0)`,
		"ps-"+session, thread, session)
}

// newBound: a server that finds conversations in host (no cache across steps).
func newBound(t *testing.T) (*env, *hostDB) {
	h := newHostDB(t)
	e := newEnv(t, func(s *api.Server) { s.Conversations = &openbot.Conversations{Path: h.path, TTL: time.Nanosecond} })
	return e, h
}

// bind binds a conversation to a project as the owner.
func (e *env) bind(project, kind, id string) {
	e.t.Helper()
	e.must(http.StatusOK, e.owner, "PUT", "/v1/projects/"+e.id[project]+"/conversations", map[string]any{"kind": kind, "openbot_id": id})
}

// verbIn runs a verb the way kch hook sends it from a Claude session.
func (e *env) verbIn(agent, session, verb string, body any) (int, map[string]any) {
	return e.call(e.tok[agent], "POST", "/v1/agent/"+verb, body, api.SessionHeader, session)
}

// teamTwo: Product Lead PL is lead of alpha (P1) and beta (P2); EB is the
// engineering worker of beta; each project has one knowledge item of its own.
func teamTwo(t *testing.T) (*env, *hostDB) {
	e, h := newBound(t)
	e.project("alpha")
	e.project("beta")
	e.agentP("PL", "alpha", "lead", "product")
	e.must(http.StatusCreated, e.owner, "POST", "/v1/principals/"+e.id["PL"]+"/memberships", map[string]any{"project_id": e.id["beta"], "role": "lead"})
	e.agentP("EB", "beta", "worker", "engineering")
	e.know("alpha", "ALPHA-RULE", "rule", "AUTHORITATIVE", "feature:editor")
	e.know("beta", "BETA-RULE", "rule", "AUTHORITATIVE", "feature:editor")
	return e, h
}

// search is `kch search Nội dung` from a session; it returns status and text.
func (e *env) search(agent, session string) (int, string) {
	status, out := e.verbIn(agent, session, "search", map[string]any{"args": []string{"Nội", "dung"}})
	return status, str(out, "text") + str(out, "error")
}
