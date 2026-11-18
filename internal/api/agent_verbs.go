package api

// Agent verbs: POST /v1/agent/{verb} on the agent socket (spec S2 A.4,
// PM-S2-02, AC-S2-004). An agent's Bash has no socket; when it types exactly
// one `kch <verb> …`, the pre-tool hook client posts the verb here and shows
// the answer's "text" to the model.
//
// HOW TO ADD A VERB. Put a file in this package that registers it in init():
//
//	func init() { RegisterAgentVerb("lesson", verbLesson) }
//
//	func verbLesson(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
//		t, err := c.Task()            // 409 when the caller holds no lease
//		...
//		return AgentReply{Status: http.StatusCreated, Text: "Đã ghi bài học.", Data: map[string]any{"id": id}}, nil
//	}
//
// The handler signature is AgentVerb. What a handler may rely on:
//   - c.Actor is the attested caller. c.Held is its live lease with the task,
//     or nil. The task and the project ALWAYS come from c.Task() / c.Project():
//     never from an argument, never from the body (spec §0.1).
//   - c.Args are the words typed after the verb; c.Stdin is a heredoc sent next
//     to them; c.Body is the raw JSON body when the command was only a heredoc
//     (`kch understand <<'EOF' {…} EOF`). Decode it with c.Decode(&in).
//   - Return kerr errors (Forbidden 403, NotFound 404, Conflict 409, Invalid
//     422); an untyped error is a 500 and its text is not shown.
//   - Reply.Text is what the model reads: plain, compact Vietnamese, no JSON
//     dump. Reply.Data adds machine-readable fields next to "text".
//   - Open transactions with s.scoped-like helpers: c.scoped(ctx, s, fn) runs
//     under the caller's own row-level-security scope; s.runtime only for the
//     writes the spec reserves to the Runtime role, after authorizing.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// AgentVerb handles one verb.
type AgentVerb func(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error)

// AgentCall is one verb call of an attested agent.
type AgentCall struct {
	Actor authn.Principal
	Held  *work.Held      // live lease and its task; nil without one
	Args  []string        // words after the verb
	Stdin string          // heredoc text sent together with Args
	Body  json.RawMessage // raw JSON body of a heredoc-only command; nil otherwise
	bound binding         // spec S2 E.9: the conversation of the hook's session and its project
}

// AgentReply is the answer of a verb.
type AgentReply struct {
	Status int            // HTTP status; 0 = 200
	Text   string         // shown to the model
	Data   map[string]any // extra fields of the JSON answer
}

var (
	verbMu sync.RWMutex
	verbs  = map[string]AgentVerb{}
)

// RegisterAgentVerb adds a verb. Call it from init(); a name registered twice panics at start.
func RegisterAgentVerb(name string, fn AgentVerb) {
	verbMu.Lock()
	defer verbMu.Unlock()
	if _, dup := verbs[name]; dup || name == "" || fn == nil {
		panic("api: agent verb " + name + " registered twice or empty")
	}
	verbs[name] = fn
}

// AgentVerbNames lists the registered verbs.
func AgentVerbNames() []string {
	verbMu.RLock()
	defer verbMu.RUnlock()
	names := make([]string, 0, len(verbs))
	for n := range verbs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Task returns the task of the caller's live lease (409 without one).
func (c *AgentCall) Task() (work.Task, error) {
	if c.Held == nil {
		return work.Task{}, kerr.New(kerr.Conflict, "bạn chưa được giao task nào (không có lease)")
	}
	return c.Held.Task, nil
}

// Project returns the project a call works in (spec S2 E.9-4): the lease's
// project; else the project the conversation of the call is bound to, which
// the caller must be a member of (409 otherwise, no fallback); else the
// caller's only membership. Several memberships and nothing else is 409: the
// agent never picks a project itself (spec §0.1). A channel bound to another
// project than the lease's was refused before the verb ran (agentVerb).
func (c *AgentCall) Project() (string, error) {
	if c.Held != nil {
		return c.Held.Task.ProjectID, nil
	}
	if c.bound.project != "" {
		if c.Actor.RoleIn(c.bound.project) == "" {
			return "", kerr.New(kerr.Conflict, "cuộc hội thoại này gắn với một project mà bạn không là thành viên; chủ sản phẩm cần sửa việc gắn")
		}
		return c.bound.project, nil
	}
	if len(c.Actor.Roles) == 1 {
		for id := range c.Actor.Roles {
			return id, nil
		}
	}
	return "", kerr.New(kerr.Conflict, "không xác định được project: chưa giữ task nào và thuộc %d project", len(c.Actor.Roles))
}

// Decode reads the raw JSON body of a heredoc command into dst (422 when absent or malformed).
func (c *AgentCall) Decode(dst any) error {
	if len(c.Body) == 0 {
		return kerr.New(kerr.Invalid, "lệnh này cần nội dung JSON qua heredoc (<<'EOF' … EOF)")
	}
	if err := json.Unmarshal(c.Body, dst); err != nil {
		return kerr.New(kerr.Invalid, "JSON không hợp lệ")
	}
	return nil
}

// scoped runs fn under the caller's own row-level-security scope.
func (c *AgentCall) scoped(ctx context.Context, s *Server, fn func(tx pgx.Tx) error) error {
	return store.InScope(ctx, s.Pool, c.Actor.Scope(), fn)
}

// agentVerb serves POST /v1/agent/{verb}. The answer is {"text": …, …}.
func (s *Server) agentVerb(w http.ResponseWriter, r *http.Request) {
	actor := principalOf(r)
	if actor.IsOwner() {
		writeErr(w, r, kerr.New(kerr.Forbidden, "lệnh agent chỉ dành cho agent"))
		return
	}
	verbMu.RLock()
	fn := verbs[r.PathValue("verb")]
	verbMu.RUnlock()
	if fn == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "không có lệnh này. Các lệnh: " + strings.Join(AgentVerbNames(), ", ")})
		return
	}
	c := &AgentCall{Actor: actor}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	switch {
	case err != nil || len(raw) > maxBody:
		writeErr(w, r, kerr.New(kerr.Invalid, "body không đọc được hoặc quá lớn"))
		return
	case len(bytes.TrimSpace(raw)) > 0 && !json.Valid(raw):
		writeErr(w, r, kerr.New(kerr.Invalid, "JSON không hợp lệ"))
		return
	}
	var envelope struct {
		Args  *[]string `json:"args"`
		Stdin string    `json:"stdin"`
	}
	if json.Unmarshal(raw, &envelope) == nil && envelope.Args != nil {
		c.Args, c.Stdin = *envelope.Args, envelope.Stdin
	} else if len(bytes.TrimSpace(raw)) > 0 {
		c.Body = raw // exactly the bytes of the heredoc (AC-S2-004)
	}
	err = s.runtime(r.Context(), func(tx pgx.Tx) (err error) {
		c.Held, err = work.ActiveLease(r.Context(), tx, actor.ID)
		return err
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	c.bound = s.bindingOf(r.Context(), actor, r.Header.Get(SessionHeader)) // spec S2 E.9-2, E.9-3
	if msg := s.mismatch(r.Context(), c.Held, c.bound); msg != "" {        // E.9-4: nothing is returned, nothing created
		writeErr(w, r, conflictErr(msg))
		return
	}
	reply, err := fn(r.Context(), s, c)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	out := map[string]any{}
	for k, v := range reply.Data {
		out[k] = v
	}
	out["text"] = reply.Text
	if reply.Status == 0 {
		reply.Status = http.StatusOK
	}
	writeJSON(w, reply.Status, out)
}
