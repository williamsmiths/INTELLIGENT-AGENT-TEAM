package openbot

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// Spec S2 E.9-3 and E.9-7 on a fixture with OpenBot's real table and column
// names: channel first, thread only when no channel names the session, only
// the caller's own thread, two channels = none, and only hits are cached.
func TestE9_ConversationLookupAndCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "openbot.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, q := range []string{
		`CREATE TABLE projection_channel_contexts (channel_id TEXT NOT NULL, agent_id TEXT NOT NULL, thread_id TEXT NOT NULL UNIQUE,
			session_id TEXT, through_sequence INTEGER NOT NULL DEFAULT 0, summary_version INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(channel_id, agent_id))`,
		`CREATE TABLE projection_provider_sessions (id TEXT PRIMARY KEY, thread_id TEXT NOT NULL, provider TEXT NOT NULL,
			external_session_id TEXT NOT NULL, UNIQUE(provider, external_session_id))`,
		`CREATE TABLE projection_agent_threads (thread_id TEXT PRIMARY KEY, agent_id TEXT NOT NULL)`,
		`CREATE TABLE projection_agents (agent_id TEXT PRIMARY KEY, thread_id TEXT)`,
		`INSERT INTO projection_channel_contexts (channel_id, agent_id, thread_id, session_id) VALUES
			('C1', 'a', 'x1', 'S-ch'), ('C1', 'b', 'x2', 'S-two'), ('C2', 'b', 'x3', 'S-two'), ('C3', 'a', 'x4', 'S-both')`,
		`INSERT INTO projection_provider_sessions (id, thread_id, provider, external_session_id) VALUES
			('1', 'T-main', 'claude', 'S-main'), ('2', 'T-side', 'claude', 'S-side'), ('3', 'T-main', 'codex', 'S-codex'),
			('4', 'T-other', 'claude', 'S-other'), ('5', 'T-main', 'claude', 'S-both')`,
		`INSERT INTO projection_agents (agent_id, thread_id) VALUES ('a', 'T-main'), ('z', 'T-other')`,
		`INSERT INTO projection_agent_threads (thread_id, agent_id) VALUES ('T-side', 'a')`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	c := &Conversations{Path: path, TTL: time.Hour}
	defer c.Close()
	ctx := context.Background()
	for _, tc := range []struct{ session, agent, kind, id string }{
		{"S-ch", "a", "channel", "C1"},
		{"S-ch", "b", "", ""},               // another agent's channel context
		{"S-two", "b", "", ""},              // two channels: no binding
		{"S-main", "a", "thread", "T-main"}, // projection_agents.thread_id of the caller's own row
		{"S-side", "a", "thread", "T-side"}, // projection_agent_threads
		{"S-other", "a", "", ""},            // thread of another agent (D5)
		{"S-codex", "a", "", ""},            // not a Claude session
		{"S-both", "a", "channel", "C3"},    // channel wins (E.9-7)
	} {
		kind, id, err := c.Find(ctx, tc.session, tc.agent)
		if err != nil || kind != tc.kind || id != tc.id {
			t.Fatalf("%s/%s: %q %q %v, want %q %q", tc.session, tc.agent, kind, id, err, tc.kind, tc.id)
		}
	}
	// A hit is served from the cache; a miss is not cached.
	if _, err := db.Exec(`DELETE FROM projection_channel_contexts WHERE session_id = 'S-ch'`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO projection_channel_contexts (channel_id, agent_id, thread_id, session_id) VALUES ('C9', 'b', 'x9', 'S-ch')`); err != nil {
		t.Fatal(err)
	}
	if kind, id, _ := c.Find(ctx, "S-ch", "a"); kind != "channel" || id != "C1" {
		t.Fatalf("cached hit: %q %q", kind, id)
	}
	if kind, id, _ := c.Find(ctx, "S-ch", "b"); kind != "channel" || id != "C9" {
		t.Fatalf("a miss was cached: %q %q", kind, id)
	}
	if _, _, err := NewConversations(filepath.Join(t.TempDir(), "missing.db")).Find(ctx, "S", "a"); err == nil {
		t.Fatal("a missing host database must be an error (treated as no binding)")
	}
}
