package work

import (
	"context"
	"errors"
	"regexp"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// validID keeps malformed ids away from uuid columns: they are simply not found.
func validID(id string) bool { return uuidRe.MatchString(id) }

func notFound(what string) error { return kerr.New(kerr.NotFound, "%s not found", what) }

// requireOwner guards the owner-only operations (PM-H-01, 02, 14, 15, 22): everyone else gets 403.
func requireOwner(actor authn.Principal) error {
	if !actor.IsOwner() {
		return kerr.New(kerr.Forbidden, "owner only")
	}
	return nil
}

// requireManager lets the owner and the leads of the project through (PM-H-11).
// A worker of the project gets 403; anyone else must not learn the project exists (404).
func requireManager(actor authn.Principal, projectID, what string) error {
	switch actor.RoleIn(projectID) {
	case "owner", "lead":
		return nil
	case "worker":
		return kerr.New(kerr.Forbidden, "owner or lead of the project only")
	}
	return notFound(what)
}

// requireMember lets the owner and the members of the project through (PM-H-03, PM-H-20).
func requireMember(actor authn.Principal, projectID string) error {
	if !validID(projectID) || actor.RoleIn(projectID) == "" {
		return notFound("project")
	}
	return nil
}

// manageTask loads a task for an Owner/Lead command and locks its row.
func manageTask(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (Task, error) {
	t, err := loadTask(ctx, tx, taskID, false)
	if err != nil {
		return Task{}, err
	}
	if err := requireManager(actor, t.ProjectID, "task"); err != nil {
		return Task{}, err
	}
	return loadTask(ctx, tx, taskID, true)
}

// viewTask loads a task the actor may see: Owner all, Lead its projects,
// Worker only the task whose lease it holds (PM-H-12, PM-H-13, PM-H-16).
func viewTask(ctx context.Context, tx pgx.Tx, actor authn.Principal, taskID string) (Task, *Lease, error) {
	t, err := loadTask(ctx, tx, taskID, false)
	if err != nil {
		return Task{}, nil, err
	}
	l, err := taskLease(ctx, tx, taskID, false)
	if err != nil {
		return Task{}, nil, err
	}
	if l != nil && l.due {
		l = nil // T-H-23: an overdue lease is not shown as live; the runtime ends it
	}
	switch actor.RoleIn(t.ProjectID) {
	case "owner", "lead":
		return t, l, nil
	case "worker":
		if l != nil && l.PrincipalID == actor.ID {
			return t, l, nil
		}
	}
	return Task{}, nil, notFound("task")
}

// conflict turns a unique violation into kerr.Conflict; other errors pass through.
func conflict(err error, msg string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return kerr.New(kerr.Conflict, "%s", msg)
	}
	return err
}

// negative reports a limit below zero.
func (l Limits) negative() bool {
	for _, v := range []*int64{l.MaxCost, l.MaxInputTokens, l.MaxOutputTokens, l.MaxToolCalls, l.MaxTurns} {
		if v != nil && *v < 0 {
			return true
		}
	}
	return false
}
