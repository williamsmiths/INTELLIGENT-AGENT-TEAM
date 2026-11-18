package api

import (
	"context"
	"log/slog"
	"net/http"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// Spec S2 E.9: the OpenBot conversation a verb or a hook event comes from, and
// the project it is bound to. It only chooses a project; rights stay those of
// the caller under row-level security (E.9-6).

// SessionHeader carries the Claude session id of a verb call. Only `kch hook`
// sets it, from the hook input; the agent's typed command never reaches the
// request (E.9-2, AC-S2-086).
const SessionHeader = "X-KCH-Session"

// binding is the conversation of one call: kind and id when the host knows
// it, project when the Owner bound it ("" otherwise).
type binding struct {
	kind, id, project string
}

// lookupFailed limits CONVERSATION_LOOKUP_FAILED to one event a minute per caller (E.9-3c).
var lookupFailed rateLimit

// validSession: a printable token of at most 200 characters (Claude uses uuids).
func validSession(s string) bool {
	if s == "" || len(s) > 200 {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

// bindingOf finds the conversation of a session and its bound project. Any
// failure is "no binding": the call behaves as before E.9 (AC-S2-083).
func (s *Server) bindingOf(ctx context.Context, actor authn.Principal, session string) binding {
	if s.Conversations == nil || actor.OpenBotAgentID == "" || !validSession(session) {
		return binding{}
	}
	kind, id, err := s.Conversations.Find(ctx, session, actor.OpenBotAgentID)
	if err != nil {
		slog.WarnContext(ctx, "conversation lookup failed", "err", err)
		if lookupFailed.allow(actor.ID) {
			_ = s.runtime(ctx, func(tx pgx.Tx) error {
				_, e := journal.Append(ctx, tx, journal.Event{Type: "CONVERSATION_LOOKUP_FAILED", Payload: map[string]any{"principal_id": actor.ID}})
				return e
			})
		}
		return binding{}
	}
	if kind == "" {
		return binding{}
	}
	b := binding{kind: kind, id: id}
	if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		b.project, err = work.BoundProject(ctx, tx, kind, id)
		return err
	}); err != nil {
		slog.WarnContext(ctx, "conversation binding unreadable", "err", err)
		return binding{}
	}
	return b
}

// mismatch is the refusal of E.9-4/5 when a lease holder speaks in a channel
// bound to another project, or "" (a thread never blocks a holder, D1).
func (s *Server) mismatch(ctx context.Context, held *work.Held, b binding) string {
	if held == nil || b.kind != work.ConversationChannel || b.project == "" || b.project == held.Task.ProjectID {
		return ""
	}
	slugs := map[string]string{}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, slug FROM project WHERE id = ANY($1::uuid[])`, []string{b.project, held.Task.ProjectID})
		if err != nil {
			return err
		}
		var id, slug string
		_, err = pgx.ForEachRow(rows, []any{&id, &slug}, func() error { slugs[id] = slug; return nil })
		return err
	})
	return "tin này đến từ cuộc hội thoại của " + slugs[b.project] + "; bạn đang giữ task " + held.Task.Key + " ở " +
		slugs[held.Task.ProjectID] + ". Trả lời trong cuộc hội thoại của " + slugs[held.Task.ProjectID] + " hoặc nhả task trước."
}

// bindConversation serves PUT /v1/projects/{p}/conversations (PM-S2-23).
func (s *Server) bindConversation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Kind      string `json:"kind"`
		OpenBotID string `json:"openbot_id"`
	}
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	var out work.ConversationBinding
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.BindConversation(r.Context(), tx, principalOf(r), r.PathValue("p"), in.Kind, in.OpenBotID)
		return err
	})
	s.reply(w, r, http.StatusOK, out, err)
}

// listConversations serves GET /v1/projects/{p}/conversations.
func (s *Server) listConversations(w http.ResponseWriter, r *http.Request) {
	var out []work.ConversationBinding
	err := s.scoped(r, func(tx pgx.Tx) (err error) {
		out, err = work.ListConversations(r.Context(), tx, principalOf(r), r.PathValue("p"))
		return err
	})
	s.reply(w, r, http.StatusOK, map[string]any{"conversations": out}, err)
}

// unbindConversation serves DELETE /v1/projects/{p}/conversations/{kind}/{id}.
func (s *Server) unbindConversation(w http.ResponseWriter, r *http.Request) {
	err := s.scoped(r, func(tx pgx.Tx) error {
		return work.UnbindConversation(r.Context(), tx, principalOf(r), r.PathValue("p"), r.PathValue("kind"), r.PathValue("id"))
	})
	s.reply(w, r, http.StatusOK, map[string]any{"unbound": err == nil}, err)
}

// conflictErr is the 409 of a mismatched channel (E.9-4).
func conflictErr(msg string) error { return kerr.New(kerr.Conflict, "%s", msg) }
