package attest_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/attest"
	"github.com/williamsmiths/kongcowork-harness/internal/attest/attesttest"
)

func TestMain(m *testing.M) { attesttest.Main(m) }

type connKey struct{}

// fixture is a real unix socket server that answers every request with what
// the peer attests as, plus the binaries and workspaces the trees are built of.
type fixture struct {
	t                         *testing.T
	cfg                       attest.Config
	socket                    string
	host, provider, bwrap     string // distinct copies of the test binary
	agentX, agentY, elsewhere string // working directories
}

func newFixture(t *testing.T) *fixture { return newFixtureAs(t, "") }

// newFixtureAs sets the executable that counts as harnessd; "" = one that runs nowhere.
func newFixtureAs(t *testing.T, self string) *fixture {
	t.Helper()
	dir := attesttest.ShortDir(t)
	bin := filepath.Join(dir, "sbx")
	f := &fixture{t: t, socket: filepath.Join(dir, "a.sock"),
		agentX: filepath.Join(dir, "Agents", "agent-X"), agentY: filepath.Join(dir, "Agents", "agent-Y"), elsewhere: filepath.Join(dir, "else")}
	for _, d := range []string{f.agentX, filepath.Join(f.agentX, "sub"), f.agentY, f.elsewhere, bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.host, f.provider, f.bwrap = attesttest.Copy(t, dir, "openbot"), attesttest.Copy(t, dir, "claude"), attesttest.Copy(t, bin, "bwrap")
	if self == "" {
		self = filepath.Join(dir, "no-harnessd")
	}
	f.cfg = attest.Config{OpenBotExe: f.host, AgentsRoot: filepath.Join(dir, "Agents"), SelfExe: self}
	ln, err := net.Listen("unix", f.socket)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{ReadHeaderTimeout: 5 * time.Second,
		ConnContext: func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) },
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, _ := r.Context().Value(connKey{}).(*net.UnixConn)
			out := map[string]any{}
			pid, err := attest.PeerPID(conn)
			if err == nil {
				var id attest.Identity
				if id, err = f.cfg.Resolve(pid); err == nil {
					out["identity"] = id
				}
			}
			if err != nil {
				out["error"] = err.Error()
			}
			_ = json.NewEncoder(w).Encode(out)
		})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

// attested runs a real tree whose last level connects to the socket.
func (f *fixture) attested(steps ...attesttest.Step) attest.Identity {
	f.t.Helper()
	a := attesttest.Run(f.t, attesttest.Plan{Steps: steps, Socket: f.socket, Path: "/"})
	var out struct {
		Identity attest.Identity `json:"identity"`
		Error    string          `json:"error"`
	}
	if a.Status != http.StatusOK || json.Unmarshal([]byte(a.Body), &out) != nil || out.Error != "" {
		f.t.Fatalf("no identity: %+v", a)
	}
	return out.Identity
}

func (f *fixture) tree(extra ...attesttest.Step) []attesttest.Step {
	return append([]attesttest.Step{{Exe: f.host, Dir: f.elsewhere}, {Exe: f.provider, Dir: f.agentX}}, extra...)
}

func TestAC_S2_001_AttestsTheAgentOfARealProcessTree(t *testing.T) {
	f := newFixture(t)
	id := f.attested(f.tree(attesttest.Step{})...) // host -> provider (cwd agent-X) -> client
	if id.AgentID != "agent-X" || id.HostPID == 0 || id.ProviderPID == 0 || id.Sandboxed || id.HasHarnessAncestor {
		t.Fatalf("identity: %+v", id)
	}
	if len(id.Chain) < 3 || id.Chain[0].Exe != f.provider || id.Chain[1].PID != id.ProviderPID || id.Chain[1].CWD != f.agentX ||
		id.Chain[2].PID != id.HostPID || id.Chain[2].Exe != f.host || id.Chain[0].StartTime == 0 {
		t.Fatalf("chain: %+v", id.Chain)
	}
	t.Run("the provider itself and a client deeper in the workspace", func(t *testing.T) {
		if id := f.attested(f.tree()...); id.AgentID != "agent-X" || id.ProviderPID != id.Chain[0].PID {
			t.Fatalf("provider as peer: %+v", id)
		}
		deep := []attesttest.Step{{Exe: f.host, Dir: f.elsewhere}, {Exe: f.provider, Dir: filepath.Join(f.agentX, "sub")}, {}}
		if id := f.attested(deep...); id.AgentID != "agent-X" {
			t.Fatalf("first component below AgentsRoot: %+v", id)
		}
	})
	t.Run("AC-S2-003 a child of harnessd is marked as a runner", func(t *testing.T) {
		f := newFixtureAs(t, attest.Self()) // the test process is the ancestor of every tree it starts
		if id := f.attested(attesttest.Step{Exe: f.provider, Dir: f.elsewhere}); !id.HasHarnessAncestor || id.AgentID != "" {
			t.Fatalf("runner child: %+v", id)
		}
	})
	t.Run("a bwrap process between the peer and the provider marks the peer as sandboxed", func(t *testing.T) {
		if id := f.attested(f.tree(attesttest.Step{Exe: f.bwrap}, attesttest.Step{Exe: f.provider})...); !id.Sandboxed || id.AgentID != "agent-X" {
			t.Fatalf("sandboxed: %+v", id)
		}
	})
}

func TestAC_S2_002_AnAgentCannotBecomeAnotherAgent(t *testing.T) {
	f := newFixture(t)
	t.Run("the client moves into the workspace of agent-Y", func(t *testing.T) {
		if id := f.attested(f.tree(attesttest.Step{Chdir: f.agentY})...); id.AgentID != "agent-X" {
			t.Fatalf("chdir changed the agent: %+v", id)
		}
		if id := f.attested(f.tree(attesttest.Step{Dir: f.agentY})...); id.AgentID != "agent-X" {
			t.Fatalf("a child started in agent-Y changed the agent: %+v", id)
		}
	})
	t.Run("KCH_AGENT in the environment", func(t *testing.T) {
		if id := f.attested(f.tree(attesttest.Step{Env: []string{"KCH_AGENT=agent-Y", "KCH_SOCKET=/nowhere"}})...); id.AgentID != "agent-X" {
			t.Fatalf("environment changed the agent: %+v", id)
		}
	})
	t.Run("an intermediate process that runs the host binary, with a child in agent-Y", func(t *testing.T) {
		id := f.attested(f.tree(attesttest.Step{Exe: f.host, Dir: f.elsewhere}, attesttest.Step{Exe: f.provider, Dir: f.agentY}, attesttest.Step{})...)
		if id.AgentID != "agent-X" || id.HostPID != id.Chain[4].PID || id.Chain[2].Exe != f.host {
			t.Fatalf("the fake host re-rooted the chain: %+v", id)
		}
	})
	t.Run("no host in the ancestors", func(t *testing.T) {
		if id := f.attested(attesttest.Step{Exe: f.provider, Dir: f.agentX}, attesttest.Step{}); id.HostPID != 0 || id.AgentID != "" {
			t.Fatalf("attested without a host: %+v", id)
		}
	})
	t.Run("a client detached from the tree (double fork)", func(t *testing.T) {
		id := f.attested(f.tree(attesttest.Step{}, attesttest.Step{Orphan: true})...)
		if id.HostPID != 0 || id.AgentID != "" {
			t.Fatalf("a detached process still attests: %+v", id)
		}
		for _, p := range id.Chain[1:] {
			if p.Exe == f.host || p.Exe == f.provider {
				t.Fatalf("the detached process still has the tree above it: %+v", id.Chain)
			}
		}
	})
}

func TestResolveRefusesWhatItCannotRead(t *testing.T) {
	cfg := attest.Config{OpenBotExe: "/x/openbot", AgentsRoot: "/x/Agents"}
	for _, pid := range []int{0, -1, 1 << 30} {
		if _, err := cfg.Resolve(pid); !errors.Is(err, attest.ErrNotAttested) {
			t.Fatalf("pid %d: %v", pid, err)
		}
	}
	if _, err := attest.PeerPID(nil); !errors.Is(err, attest.ErrNotAttested) {
		t.Fatalf("nil connection: %v", err)
	}
	// The test process itself resolves, up to pid 1, without a host.
	id, err := cfg.Resolve(os.Getpid())
	if err != nil || len(id.Chain) < 2 || id.Chain[0].PID != os.Getpid() || id.HostPID != 0 {
		t.Fatalf("self: %+v %v", id, err)
	}
}
