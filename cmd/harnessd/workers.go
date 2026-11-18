package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/memory"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/openbot"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// bootstrapOwner creates the owner principal on first start. No token is
// written anywhere (AC-S2-006): the Owner is whoever reaches the owner socket.
// The token_hash column is still NOT NULL from S1, so it gets the hash of a
// random value that is thrown away at once.
func bootstrapOwner(ctx context.Context, pool *pgxpool.Pool) error {
	return store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM principal WHERE kind = 'owner'`).Scan(&n); err != nil || n > 0 {
			return err
		}
		_, hash, err := authn.NewToken()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO principal (kind, name, token_hash) VALUES ('owner', 'owner', $1)`, hash); err != nil {
			return err
		}
		slog.Info("owner principal created")
		return nil
	})
}

// startWorkers runs the bounded timers of the runtime. They react to state
// that already exists (expired leases, changed hook files, new host usage);
// none of them starts an agent or calls a model.
func startWorkers(ctx context.Context, pool *pgxpool.Pool, cfg config) {
	every(ctx, time.Minute, "lease-expiry", func() error {
		return store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			_, err := work.ExpireLeases(ctx, tx)
			return err
		})
	})
	every(ctx, time.Minute, "hook-watch", func() error {
		return store.InScope(ctx, pool, store.Runtime, func(tx pgx.Tx) error {
			if _, err := hookinstall.Scan(ctx, tx); err != nil {
				return err
			}
			_, err := hookinstall.IngestFailOpen(ctx, tx, cfg.HookLog)
			return err
		})
	})
	// Spec S2 part C: QUESTION_WAITING for blocking questions the owner may have
	// answered in the host's chat, consult expiry, and the host turn of each
	// consult message (COLLAB cost). It talks to the host only when delivery is on.
	var host api.Notifier
	if cfg.Notify {
		host = openbot.NewNotifier(filepath.Join(cfg.AutomationDir, "url"), filepath.Join(cfg.AutomationDir, "token"))
	}
	every(ctx, time.Minute, "intel", func() error { return api.SweepIntel(ctx, pool, host) })
	// Semantic retrieval (spec S2 part B): the stage is installed only with a
	// configured local sidecar; the worker embeds new or changed knowledge only.
	if cfg.Embed.URL != "" {
		if w := memory.EnableSemantic(pool, cfg.Embed.Client(), cfg.Embed.MinSim); w != nil {
			w.Logf = func(format string, args ...any) { slog.Warn(fmt.Sprintf(format, args...), "worker", "embed") }
			go w.Run(ctx)
		}
	}
	if cfg.OpenBotDB != "" {
		tailer := openbot.NewTailer(pool, cfg.OpenBotDB)
		go func() {
			defer tailer.Close()
			if err := tailer.Run(ctx, 5*time.Second); err != nil && ctx.Err() == nil {
				slog.Error("tailer stopped", "err", err)
			}
		}()
	}
}

// every runs fn on a timer until ctx ends. Errors are logged, not fatal.
func every(ctx context.Context, d time.Duration, name string, fn func() error) {
	go func() {
		t := time.NewTicker(d)
		defer t.Stop()
		for {
			if err := fn(); err != nil && ctx.Err() == nil {
				slog.Warn("worker failed", "worker", name, "err", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}
