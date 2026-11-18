package openbot

import (
	"context"
	"database/sql"
	"sync"
	"time"
)

// Conversations finds the OpenBot conversation a Claude session of an agent
// belongs to (spec S2 E.9-3), reading the host database read-only like the
// Tailer. Only found conversations are cached, for TTL (5 s when zero); a
// miss is asked again next time (X5, D6).
type Conversations struct {
	Path string
	TTL  time.Duration

	mu    sync.Mutex
	db    *sql.DB
	cache map[string]cachedConversation
}

type cachedConversation struct {
	kind, id string
	until    time.Time
}

// NewConversations returns a lookup over the host database at path.
func NewConversations(path string) *Conversations { return &Conversations{Path: path} }

// Find returns ("channel", channel_id) when the session is the context of
// exactly one channel of the agent; else, when no channel names it,
// ("thread", thread_id) for a Claude session of a thread the agent owns
// (projection_agent_threads.agent_id, or projection_agents.thread_id on the
// agent's own row, D5). Two channels, or nothing found: ("", "") — and a
// session in a channel is never looked up as a thread (D4). An unreadable
// database is an error; the caller treats it as "no conversation".
func (c *Conversations) Find(ctx context.Context, session, agentID string) (kind, id string, err error) {
	key := agentID + "\x00" + session
	ttl := c.TTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if hit, ok := c.cache[key]; ok && time.Now().Before(hit.until) {
		return hit.kind, hit.id, nil
	}
	if c.db == nil {
		if c.db, err = openReadOnly(c.Path); err != nil {
			return "", "", err
		}
	}
	kind, id, err = c.lookup(ctx, session, agentID)
	if err != nil || kind == "" {
		return "", "", err
	}
	if c.cache == nil || len(c.cache) > 4096 {
		c.cache = map[string]cachedConversation{}
	}
	c.cache[key] = cachedConversation{kind: kind, id: id, until: time.Now().Add(ttl)}
	return kind, id, nil
}

func (c *Conversations) lookup(ctx context.Context, session, agentID string) (string, string, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT channel_id FROM projection_channel_contexts
		WHERE session_id = ? AND agent_id = ? LIMIT 2`, session, agentID)
	if err != nil {
		return "", "", err
	}
	var channels []string
	for rows.Next() {
		var ch string
		if err := rows.Scan(&ch); err != nil {
			rows.Close()
			return "", "", err
		}
		channels = append(channels, ch)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", "", err
	}
	switch len(channels) {
	case 1:
		return "channel", channels[0], nil
	case 2:
		return "", "", nil // several channels: no binding (E.9-3a)
	}
	var thread string
	err = c.db.QueryRowContext(ctx, `SELECT s.thread_id FROM projection_provider_sessions s
		WHERE s.provider = 'claude' AND s.external_session_id = ?
		  AND (EXISTS (SELECT 1 FROM projection_agent_threads t WHERE t.thread_id = s.thread_id AND t.agent_id = ?)
		    OR EXISTS (SELECT 1 FROM projection_agents a WHERE a.thread_id = s.thread_id AND a.agent_id = ?))`,
		session, agentID, agentID).Scan(&thread)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	return "thread", thread, nil
}

// Close releases the SQLite handle.
func (c *Conversations) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.db == nil {
		return nil
	}
	err := c.db.Close()
	c.db = nil
	return err
}
