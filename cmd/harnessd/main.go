// Command harnessd is the kongCowork Intelligence Harness service: one
// process, four responsibility groups (Memory & Context, Adaptive Runtime,
// Engineering & Verification, Intelligence & Collaboration), PostgreSQL as the
// authoritative store. It does not run an agent loop or a scheduler of its
// own: OpenBot owns agents, turns and schedules (docs/specs/_context.md).
//
// It listens on two unix sockets and on no TCP port, and it holds no token:
// callers are identified by process attestation (spec S2 part A).
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/api"
	"github.com/williamsmiths/kongcowork-harness/internal/openbot"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/verify"
)

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	check := flag.Bool("check", false, "validate the configuration and the database connection, then exit")
	flag.Parse()
	scrubEnvironment()
	if err := run(*check); err != nil {
		slog.Error("harnessd stopped", "err", err)
		os.Exit(1)
	}
}

// scrubEnvironment makes sure the DSN is not in the environment of this
// process: /proc/<pid>/environ shows the environment a process was started
// with, whatever it unsets later, so harnessd starts itself again without the
// variable (AC-S2-006). The value is not used: the DSN comes from harnessd.env.
func scrubEnvironment() {
	if _, present := os.LookupEnv(dsnKey); !present {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		slog.Error("cannot restart without "+dsnKey+" in the environment", "err", err)
		os.Exit(1)
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, dsnKey+"=") {
			env = append(env, kv)
		}
	}
	slog.Warn(dsnKey + " was in the environment and is ignored; restarting without it (the DSN is read from harnessd.env)")
	if err := syscall.Exec(exe, os.Args, env); err != nil {
		slog.Error("cannot restart without "+dsnKey+" in the environment", "err", err)
		os.Exit(1)
	}
}

func run(checkOnly bool) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if checkOnly {
		return check(ctx, cfg)
	}
	if err := store.Migrate(ctx, cfg.DSN); err != nil {
		return err
	}
	pool, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		return err
	}
	defer pool.Close()
	if err := bootstrapOwner(ctx, pool); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.LogDir, 0o700); err != nil {
		return err
	}
	srv := &api.Server{Pool: pool, Runner: verify.NewRunner(), LogDir: cfg.LogDir, AgentsRoot: cfg.Machine.AgentsRoot,
		PacketChars: cfg.PacketChars, ReadRoots: cfg.Machine.SandboxReadRoots, PilotTmpRoot: cfg.Machine.PilotTmpRoot,
		// A check never sees the sockets, the DSN file or the harness state (AC-S2-010).
		Isolation: verify.Isolation{HideDirs: []string{cfg.ConfigDir, cfg.StateDir}}}
	if cfg.Notify {
		srv.Notifier = openbot.NewNotifier(filepath.Join(cfg.AutomationDir, "url"), filepath.Join(cfg.AutomationDir, "token"))
	}
	if cfg.OpenBotDB != "" { // spec S2 E.9: read-only lookup of the conversation of a session
		conv := openbot.NewConversations(cfg.OpenBotDB)
		defer conv.Close()
		srv.Conversations = conv
	}
	ownerLn, err := listenUnix(cfg.OwnerSocket)
	if err != nil {
		return err
	}
	agentLn, err := listenUnix(cfg.AgentSocket)
	if err != nil {
		ownerLn.Close()
		return err
	}
	startWorkers(ctx, pool, cfg)
	slog.Info("harnessd listening", "owner_socket", cfg.OwnerSocket, "agent_socket", cfg.AgentSocket,
		"openbot_exe", cfg.Attest.OpenBotExe, "tail", cfg.OpenBotDB != "", "notify", cfg.Notify, "semantic", cfg.Embed.URL != "")
	return serve(ctx, map[net.Listener]http.Handler{
		ownerLn: srv.OwnerRoutes(&api.PeerAuth{Pool: pool, Attest: cfg.Attest, Socket: api.SocketOwner}),
		agentLn: srv.AgentRoutes(&api.PeerAuth{Pool: pool, Attest: cfg.Attest, Socket: api.SocketAgent}),
	})
}

// check is `harnessd -check`: the configuration was read and validated by
// loadConfig; this adds the things only the machine can tell.
func check(ctx context.Context, cfg config) error {
	if st, err := os.Stat(cfg.Machine.AgentsRoot); err != nil || !st.IsDir() {
		return fmt.Errorf("machine config: agents_root %s is not a directory", cfg.Machine.AgentsRoot)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	pool, err := store.Open(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	pool.Close()
	fmt.Printf("ok: openbot_exe=%s agents_root=%s owner_socket=%s agent_socket=%s tail=%v notify=%v\n",
		cfg.Attest.OpenBotExe, cfg.Machine.AgentsRoot, cfg.OwnerSocket, cfg.AgentSocket, cfg.OpenBotDB != "", cfg.Notify)
	return nil
}
