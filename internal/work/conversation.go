package work

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Spec S2 E.9: an OpenBot conversation bound to a project chooses the project
// of a verb called without a lease. It selects a project, it grants nothing
// (E.9-6): the caller must still be a member.

// Conversation kinds (E.9-1).
const (
	ConversationChannel = "channel"
	ConversationThread  = "thread"
)

// ConversationBinding is one row of conversation_binding.
type ConversationBinding struct {
	Kind      string    `json:"kind"`
	OpenBotID string    `json:"openbot_id"`
	ProjectID string    `json:"project_id"`
	BoundBy   string    `json:"bound_by,omitempty"`
	BoundAt   time.Time `json:"bound_at"`
}

func checkConversation(kind, openbotID string) error {
	if kind != ConversationChannel && kind != ConversationThread {
		return kerr.New(kerr.Invalid, "kind must be channel or thread")
	}
	if strings.TrimSpace(openbotID) == "" || !plainText(openbotID, false, 200) {
		return kerr.New(kerr.Invalid, "openbot_id must be 1–200 characters without control characters")
	}
	return nil
}

// BindConversation binds (or rebinds) a conversation to a project
// (PUT /v1/projects/{p}/conversations, PM-S2-23; owner only; actor scope).
func BindConversation(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID, kind, openbotID string) (ConversationBinding, error) {
	if err := requireOwner(actor); err != nil {
		return ConversationBinding{}, err
	}
	if err := checkConversation(kind, openbotID); err != nil {
		return ConversationBinding{}, err
	}
	if !validID(projectID) {
		return ConversationBinding{}, notFound("project")
	}
	if _, err := loadProject(ctx, tx, projectID); err != nil {
		return ConversationBinding{}, err
	}
	var b ConversationBinding
	if err := tx.QueryRow(ctx, `INSERT INTO conversation_binding (kind, openbot_id, project_id, bound_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (kind, openbot_id) DO UPDATE SET project_id = EXCLUDED.project_id, bound_by = EXCLUDED.bound_by, bound_at = now()
		RETURNING kind, openbot_id, project_id::text, COALESCE(bound_by::text, ''), bound_at`, kind, openbotID, projectID, actor.ID).
		Scan(&b.Kind, &b.OpenBotID, &b.ProjectID, &b.BoundBy, &b.BoundAt); err != nil {
		return ConversationBinding{}, err
	}
	_, err := journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "CONVERSATION_BOUND", Actor: actor.ID,
		Payload: map[string]any{"kind": kind, "openbot_id": openbotID}})
	return b, err
}

// ListConversations returns the bindings of a project (owner only; actor scope).
func ListConversations(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID string) ([]ConversationBinding, error) {
	if err := requireOwner(actor); err != nil {
		return nil, err
	}
	if !validID(projectID) {
		return nil, notFound("project")
	}
	if _, err := loadProject(ctx, tx, projectID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT kind, openbot_id, project_id::text, COALESCE(bound_by::text, ''), bound_at
		FROM conversation_binding WHERE project_id = $1 ORDER BY kind, openbot_id`, projectID)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ConversationBinding, error) {
		var b ConversationBinding
		return b, r.Scan(&b.Kind, &b.OpenBotID, &b.ProjectID, &b.BoundBy, &b.BoundAt)
	})
	if out == nil {
		out = []ConversationBinding{}
	}
	return out, err
}

// UnbindConversation removes a binding of the project (owner only; actor scope); 404 when absent.
func UnbindConversation(ctx context.Context, tx pgx.Tx, actor authn.Principal, projectID, kind, openbotID string) error {
	if err := requireOwner(actor); err != nil {
		return err
	}
	if !validID(projectID) {
		return notFound("project")
	}
	tag, err := tx.Exec(ctx, `DELETE FROM conversation_binding WHERE kind = $1 AND openbot_id = $2 AND project_id = $3`,
		kind, openbotID, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return notFound("conversation binding")
	}
	_, err = journal.Append(ctx, tx, journal.Event{ProjectID: projectID, Type: "CONVERSATION_UNBOUND", Actor: actor.ID,
		Payload: map[string]any{"kind": kind, "openbot_id": openbotID}})
	return err
}

// BoundProject returns the project a conversation is bound to, or "" (trusted scope).
func BoundProject(ctx context.Context, tx pgx.Tx, kind, openbotID string) (string, error) {
	var project string
	err := tx.QueryRow(ctx, `SELECT project_id::text FROM conversation_binding WHERE kind = $1 AND openbot_id = $2`,
		kind, openbotID).Scan(&project)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return project, err
}
