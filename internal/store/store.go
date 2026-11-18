// Package store owns the PostgreSQL connection pool and schema migrations.
// PostgreSQL is the authoritative store of the harness (docs/specs/_context.md).
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver for goose
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Open connects a bounded pool. Callers own Close.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	if cfg.MaxConns > 16 || cfg.MaxConns == 0 {
		cfg.MaxConns = 16
	}
	cfg.MaxConnIdleTime = 5 * time.Minute
	// Default deny: every pooled connection runs as the unprivileged role, so a
	// query that forgets InScope sees no rows at all (spec §0.3, AC-H-087).
	cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "SET ROLE harness_app")
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate applies every embedded migration that has not run yet.
func Migrate(ctx context.Context, dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return fmt.Errorf("open for migrate: %w", err)
	}
	defer db.Close()
	goose.SetBaseFS(migrationFS)
	goose.SetLogger(goose.NopLogger())
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	if err := goose.UpContext(ctx, db, "migrations"); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// InTx runs fn in one transaction and retries serialization failures a
// bounded number of times. fn must be safe to run more than once.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	const attempts = 5
	var err error
	for i := 0; i < attempts; i++ {
		err = pgx.BeginFunc(ctx, pool, fn)
		if !IsRetryable(err) {
			return err
		}
	}
	return err
}

// Scope is the authenticated execution context of one request. It is set by
// the server from the bearer token and the lease, never from request bodies
// or model output, and it is enforced again by row-level security.
type Scope struct {
	Role      string   // "owner", "agent" or "runtime"
	Principal string   // principal uuid; empty for runtime
	Projects  []string // project uuids the principal is a member of
	LeadOf    []string // subset of Projects where the principal is a lead
}

// Runtime is the scope of harnessd's own workers (tailer, runner, auth lookup).
var Runtime = Scope{Role: "runtime"}

// InScope runs fn in one transaction as the unprivileged role harness_app with
// the scope applied, so PostgreSQL row-level security filters every statement.
func InScope(ctx context.Context, pool *pgxpool.Pool, sc Scope, fn func(tx pgx.Tx) error) error {
	if sc.Role != "owner" && sc.Role != "agent" && sc.Role != "runtime" {
		return fmt.Errorf("store: invalid scope role %q", sc.Role)
	}
	return InTx(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE harness_app"); err != nil {
			return fmt.Errorf("set role: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`SELECT set_config('kch.role', $1, true), set_config('kch.principal', $2, true),
			        set_config('kch.projects', $3, true), set_config('kch.lead_projects', $4, true)`,
			sc.Role, sc.Principal, strings.Join(sc.Projects, ","), strings.Join(sc.LeadOf, ",")); err != nil {
			return fmt.Errorf("set scope: %w", err)
		}
		return fn(tx)
	})
}

// IsRetryable reports serialization failures and deadlocks.
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "40001" || pgErr.Code == "40P01"
	}
	return false
}

// IsUniqueViolation reports a unique-constraint conflict, optionally on one constraint.
func IsUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return constraint == "" || pgErr.ConstraintName == constraint
	}
	return false
}
