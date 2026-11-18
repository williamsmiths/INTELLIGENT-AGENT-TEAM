package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// Agent verbs of spec S2 part E: `kch delegate` (PM-S2-20, E.1, E.5) and
// `kch delegate-policy` (PM-S2-24, REV-006b N2). The project always comes
// from the runtime (AgentCall.Project), never from the body.
func init() {
	RegisterAgentVerb("delegate", verbDelegate)
	RegisterAgentVerb("delegate-policy", verbDelegatePolicy)
}

// ownerFields are what only the Owner decides (E.1-4, E.1-5): 422 when a Lead sends one.
var ownerFields = []string{"required_checks", "protected_paths", "mode", "intel_mode", "intel_features"}

func verbDelegate(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	reply, err := s.delegate(ctx, c)
	if err != nil { // REV-006b N3: a refused delegate leaves no row, so it is at least logged
		slog.WarnContext(ctx, "delegate refused", "caller", c.Actor.ID, "err", err)
	}
	return reply, err
}

func (s *Server) delegate(ctx context.Context, c *AgentCall) (AgentReply, error) {
	project, err := s.runtimeProject(ctx, c)
	if err != nil {
		return AgentReply{}, err
	}
	if err := work.RequireDelegator(c.Actor, project); err != nil {
		return AgentReply{}, err
	}
	in, err := decodeDelegation(c.Body)
	if err != nil {
		return AgentReply{}, err
	}
	var d work.Delegated
	if err := s.runtime(ctx, func(tx pgx.Tx) (err error) { // E.1-8: one runtime-scope transaction
		d, err = work.Delegate(ctx, tx, s.bases(), c.Actor, project, in)
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	in.AllowedPaths = d.Task.AllowedPaths                                                                                  // the normalized scope (E.1-13), not what the Lead typed
	notified := s.deliver(ctx, d.OpenBotAgent, "delegate", delegateNotice(c.Actor.Name, d.Task, in, newFence()), d.Change) // after commit
	checks := make([]string, 0, len(d.Task.RequiredChecks))
	for _, ch := range d.Task.RequiredChecks {
		checks = append(checks, ch.ID)
	}
	text := fmt.Sprintf("Đã giao task %s cho %s (repo %s, phạm vi ghi: %s, mode FULL, check: %s).", d.Task.Key, d.AssigneeName, d.Task.RepoPath,
		strings.Join(d.Task.AllowedPaths, ", "), strings.Join(checks, ", "))
	if !notified {
		text += " Chưa gửi được tin báo cho người nhận; nhắc họ trong channel."
	}
	return AgentReply{Status: http.StatusCreated, Text: text + "\n", Data: map[string]any{"task_id": d.Task.ID, "key": d.Task.Key,
		"assignee": d.AssigneeName, "repo": d.Task.RepoPath, "allowed_paths": d.Task.AllowedPaths, "checks": checks, "notified": notified}}, nil
}

// runtimeProject is AgentCall.Project with the slugs of the caller's projects
// in the 409 when only the membership rule was left (E.1-1, AC-S2-062). A
// conversation bound to a foreign project names no project (AC-S2-082).
func (s *Server) runtimeProject(ctx context.Context, c *AgentCall) (string, error) {
	project, err := c.Project()
	if err == nil || !kerr.Is(err, kerr.Conflict) || c.bound.project != "" {
		return project, err
	}
	ids := make([]string, 0, len(c.Actor.Roles))
	for id := range c.Actor.Roles {
		ids = append(ids, id)
	}
	var slugs []string
	if e := c.scoped(ctx, s, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT slug FROM project WHERE id = ANY($1::uuid[]) ORDER BY slug`, ids)
		if err != nil {
			return err
		}
		slugs, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); e != nil {
		return "", e
	}
	return "", kerr.New(kerr.Conflict, "không xác định được project: chưa giữ task nào và thuộc %d project (%s)", len(slugs), strings.Join(slugs, ", "))
}

// decodeDelegation reads the heredoc strictly (E.1-4, AC-S2-063): an Owner
// field, an unknown field or trailing data is 422.
func decodeDelegation(body json.RawMessage) (work.Delegation, error) {
	var in work.Delegation
	if len(bytes.TrimSpace(body)) == 0 {
		return in, kerr.New(kerr.Invalid, "cách dùng: kch delegate <<'EOF' {\"to\":\"engineering\",\"title\":…,\"outcome\":…,\"allowed_paths\":[…]} EOF")
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		return in, kerr.New(kerr.Invalid, "nội dung phải là một JSON object")
	}
	for _, f := range ownerFields {
		if _, ok := probe[f]; ok {
			return in, kerr.New(kerr.Invalid, "trường %s do Owner quyết định (delegate policy, mode FULL); Lead không gửi được", f)
		}
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, kerr.New(kerr.Invalid, "JSON không hợp lệ hoặc có trường lạ (%s)", strings.TrimPrefix(err.Error(), "json: "))
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return in, kerr.New(kerr.Invalid, "chỉ được một JSON object")
	}
	return in, nil
}

// verbDelegatePolicy: kch delegate-policy — a Lead reads the Owner's policy of
// its runtime project (PM-S2-24); a Worker gets 403.
func verbDelegatePolicy(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	project, err := s.runtimeProject(ctx, c)
	if err != nil {
		return AgentReply{}, err
	}
	var p work.DelegatePolicy
	if err := c.scoped(ctx, s, func(tx pgx.Tx) (err error) {
		p, err = work.GetDelegatePolicy(ctx, tx, c.Actor, project)
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	var b strings.Builder
	if len(p.RequiredChecks) == 0 {
		b.WriteString("Project chưa có check mặc định: kch delegate sẽ bị từ chối cho tới khi Owner đặt policy.\n")
	}
	for _, ch := range p.RequiredChecks {
		fmt.Fprintf(&b, "Check %s: %s\n", ch.ID, ch.Command)
	}
	if len(p.ProtectedPaths) > 0 {
		fmt.Fprintf(&b, "Không được ghi (protected_paths): %s\n", strings.Join(p.ProtectedPaths, ", "))
	}
	return AgentReply{Text: b.String(), Data: map[string]any{"required_checks": p.RequiredChecks, "protected_paths": p.ProtectedPaths}}, nil
}
