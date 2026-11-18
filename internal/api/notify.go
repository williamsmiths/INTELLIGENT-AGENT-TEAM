package api

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// notifyChanges tells the holder of every newly granted lease about it through
// the host app, so an idle agent learns of an assignment or a feedback
// (REV-001 #12). Delivery is best effort and never fails the request: the
// outcome is journaled as NOTIFY_SENT or NOTIFY_FAILED (AC-H-056).
func (s *Server) notifyChanges(ctx context.Context, reason string, changes ...work.Change) {
	if s.Notifier == nil {
		return
	}
	for _, c := range changes {
		if c.Granted == nil {
			continue
		}
		s.notifyOne(ctx, reason, c)
	}
}

func (s *Server) notifyOne(ctx context.Context, reason string, c work.Change) {
	var agentID string
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		p, err := work.LookupPrincipal(ctx, tx, c.Granted.PrincipalID)
		agentID = p.OpenBotAgentID
		return err
	})
	if agentID == "" {
		return // not an OpenBot agent: nothing to deliver to
	}
	// Only the key: the title may be text a Lead wrote (spec E.1-11); the packet shows it fenced (REV-007 M1).
	text := fmt.Sprintf("[kc-harness] Task %s, rev %d — %s. %s", c.Task.Key, c.Task.Revision, reason, notifyHowTo)
	if c.Granted.PendingNotice != "" {
		text += "\n\nNội dung thay đổi:\n" + c.Granted.PendingNotice
	}
	s.deliver(ctx, agentID, reason, text, c)
}

const notifyHowTo = "Context của task sẽ được nạp tự động ở lượt này; chạy `kch context` để xem lại, `kch verify` khi xong."

// deliver sends one notice about a granted lease to an OpenBot agent and
// journals NOTIFY_SENT or NOTIFY_FAILED; it reports whether the host took it.
// A nil Notifier delivers nothing (false).
func (s *Server) deliver(ctx context.Context, agentID, reason, text string, c work.Change) bool {
	if s.Notifier == nil || agentID == "" || c.Granted == nil {
		return false
	}
	var err error
	if c.ProjectChanged {
		// A new project must not inherit the conversation of the previous one (spec §0, L8).
		err = s.Notifier.ClearContext(ctx, agentID)
	}
	if err == nil {
		_, err = s.Notifier.Send(ctx, agentID, text, "kch-"+c.Granted.ID)
	}
	typ, payload := "NOTIFY_SENT", map[string]any{"lease_id": c.Granted.ID, "reason": reason}
	if err != nil {
		typ = "NOTIFY_FAILED"
	}
	_ = s.runtime(ctx, func(tx pgx.Tx) error {
		_, e := journal.Append(ctx, tx, journal.Event{ProjectID: c.Task.ProjectID, TaskID: c.Task.ID, Type: typ, Payload: payload})
		return e
	})
	return err == nil
}
