package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/testdb"
)

// tokenRe matches a bearer token of S1: "kch_" and 32 random bytes in base64url.
var tokenRe = regexp.MustCompile(`kch_[A-Za-z0-9_-]{43}`)

// daemon is the real harnessd binary with a temp HOME and the test database.
type daemon struct {
	t    *testing.T
	bin  string
	home string
	dsn  string
	log  *syncBuffer
}

// syncBuffer collects the output of several harnessd processes at once.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func newDaemon(t *testing.T) *daemon {
	t.Helper()
	pool := testdb.New(t)
	dir, err := os.MkdirTemp("", "kcd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	d := &daemon{t: t, bin: filepath.Join(dir, "harnessd"), home: filepath.Join(dir, "h"), dsn: pool.Config().ConnString(), log: &syncBuffer{}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", d.bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build harnessd: %v\n%s", err, out)
	}
	d.write(hookinstall.EnvFile(d.home), "# harness\nHARNESS_DSN="+d.dsn+"\nHARNESS_TAIL=off\n", 0o600)
	d.write(hookinstall.MachinePath(d.home), fmt.Sprintf(`{"openbot_exe": %q, "agents_root": %q}`, d.bin, filepath.Join(d.home, "Agents")), 0o644)
	if err := os.MkdirAll(filepath.Join(d.home, "Agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

func (d *daemon) write(path, content string, mode os.FileMode) {
	d.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		d.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		d.t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		d.t.Fatal(err)
	}
}

// cmd prepares harnessd with a clean environment plus extra variables.
func (d *daemon) cmd(ctx context.Context, extraEnv []string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.bin, args...)
	cmd.Env = append([]string{"HOME=" + d.home, "PATH=" + os.Getenv("PATH")}, extraEnv...)
	cmd.Stdout, cmd.Stderr = d.log, d.log
	return cmd
}

// get sends one request over a socket of the daemon from this test process.
func get(socket, path string, header ...string) (int, string, error) {
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		}}}
	req, _ := http.NewRequest(http.MethodGet, "http://harness"+path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// AC-S2-006: the running daemon holds no secret an agent could pick up: no
// token file, no TCP listener, no DSN in its environment, and it does not
// read the Authorization header.
func TestAC_S2_006_NoSecretsNoTCP(t *testing.T) {
	d := newDaemon(t)
	owner, agent := hookinstall.OwnerSocket(d.home), hookinstall.AgentSocket(d.home)
	// A socket file left behind by a harnessd that died is replaced.
	if err := os.MkdirAll(filepath.Dir(agent), 0o755); err != nil {
		t.Fatal(err)
	}
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: agent, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	stale.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const marker = "postgres://leaked:SECRET-IN-ENV@127.0.0.1:1/none"
	cmd := d.cmd(ctx, []string{"HARNESS_DSN=" + marker})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	defer func() { _ = cmd.Process.Kill() }()
	for deadline := time.Now().Add(60 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if status, _, err := get(owner, "/healthz"); err == nil && status == http.StatusOK {
			break
		}
		select {
		case err := <-exited:
			t.Fatalf("harnessd exited: %v\n%s", err, d.log)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("harnessd did not start:\n%s", d.log)
		}
	}
	pid := cmd.Process.Pid

	t.Run("AC-S2-006 the DSN is not in /proc/<pid>/environ", func(t *testing.T) {
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil || len(environ) == 0 {
			t.Fatalf("environ: %v", err)
		}
		for _, secret := range []string{"HARNESS_DSN", "SECRET-IN-ENV", d.dsn, "postgres://"} {
			if bytes.Contains(environ, []byte(secret)) {
				t.Fatalf("%q is in the environment of harnessd", secret)
			}
		}
		if exe, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); exe != d.bin {
			t.Fatalf("pid %d runs %s", pid, exe)
		}
		if cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); bytes.Contains(cmdline, []byte("postgres")) {
			t.Fatal("the DSN is on the command line")
		}
	})
	t.Run("AC-S2-006 no token file, sockets 0600 in directories 0700", func(t *testing.T) {
		_ = filepath.WalkDir(d.home, func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !e.Type().IsRegular() {
				return nil
			}
			raw, _ := os.ReadFile(path)
			if strings.Contains(e.Name(), "token") || tokenRe.Match(raw) {
				t.Errorf("token material in %s", path)
			}
			return nil
		})
		for _, sock := range []string{owner, agent} {
			st, err := os.Lstat(sock)
			if err != nil || st.Mode()&os.ModeSocket == 0 || st.Mode().Perm() != 0o600 {
				t.Errorf("%s: %v %v, want a socket with mode 0600", sock, st.Mode(), err)
			}
			if dir, err := os.Stat(filepath.Dir(sock)); err != nil || dir.Mode().Perm() != 0o700 {
				t.Errorf("%s: directory mode %v, want 0700", filepath.Dir(sock), dir.Mode().Perm())
			}
		}
	})
	t.Run("AC-S2-006 no TCP listener", func(t *testing.T) {
		inodes := map[string]bool{}
		fds, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
		if err != nil {
			t.Fatal(err)
		}
		for _, fd := range fds {
			if target, _ := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, fd.Name())); strings.HasPrefix(target, "socket:[") {
				inodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = true
			}
		}
		if len(inodes) < 2 {
			t.Fatalf("harnessd holds %d sockets, want at least its two listeners", len(inodes))
		}
		for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
			raw, _ := os.ReadFile(table)
			for _, line := range strings.Split(string(raw), "\n")[1:] {
				// fields: sl local rem st tx_queue rx_queue tr tm->when retrnsmt uid timeout inode ...; st 0A = LISTEN
				if f := strings.Fields(line); len(f) > 9 && f[3] == "0A" && inodes[f[9]] {
					t.Fatalf("harnessd listens on TCP %s", f[1])
				}
			}
		}
	})
	t.Run("AC-S2-006 Authorization is ignored; identity is the peer process", func(t *testing.T) {
		for _, header := range [][]string{nil, {"Authorization", "Bearer kch_forged"}, {"Authorization", "garbage"}} {
			status, body, err := get(owner, "/v1/system/events", header...)
			if err != nil || status != http.StatusOK {
				t.Fatalf("owner socket with %v: %d %s %v", header, status, body, err)
			}
			if status, _, _ := get(agent, "/v1/me", header...); status != http.StatusUnauthorized {
				t.Fatalf("agent socket with %v: %d, want 401 (this process is no enrolled agent)", header, status)
			}
		}
		_, body, _ := get(owner, "/v1/whoami")
		var who map[string]any
		if json.Unmarshal([]byte(body), &who) != nil || who["role"] != "owner" || who["socket"] != "owner" {
			t.Fatalf("whoami: %s", body)
		}
	})
	t.Run("a second harnessd refuses to take over the sockets", func(t *testing.T) {
		second := d.cmd(ctx, nil)
		if err := second.Run(); err == nil || !strings.Contains(d.log.String(), "another harnessd is serving") {
			t.Fatalf("second instance: %v\n%s", err, d.log)
		}
		if status, _, err := get(owner, "/healthz"); err != nil || status != http.StatusOK {
			t.Fatal("the first instance lost its socket")
		}
	})
	t.Run("graceful shutdown removes the sockets", func(t *testing.T) {
		if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-exited:
			if err != nil {
				t.Fatalf("exit after SIGTERM: %v\n%s", err, d.log)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("harnessd did not stop")
		}
		for _, sock := range []string{owner, agent} {
			if _, err := os.Lstat(sock); !os.IsNotExist(err) {
				t.Errorf("%s is still there", sock)
			}
		}
		if strings.Contains(d.log.String(), "SECRET-IN-ENV") || strings.Contains(d.log.String(), d.dsn) {
			t.Fatal("the DSN was logged")
		}
	})
}

// `harnessd -check` validates the configuration and exits.
func TestCheckMode(t *testing.T) {
	d := newDaemon(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(env ...string) (string, error) {
		d.log.Reset()
		err := d.cmd(ctx, env, "-check").Run()
		return d.log.String(), err
	}
	if out, err := run(); err != nil || !strings.HasPrefix(out, "ok: ") || strings.Contains(out, d.dsn) {
		t.Fatalf("-check: %v\n%s", err, out)
	}
	if _, err := os.Stat(hookinstall.OwnerSocket(d.home)); !os.IsNotExist(err) {
		t.Fatal("-check opened a socket")
	}
	machine, _ := os.ReadFile(hookinstall.MachinePath(d.home))
	envFile, _ := os.ReadFile(hookinstall.EnvFile(d.home))
	for name, breakIt := range map[string]func(){
		"openbot_exe is required":   func() { d.write(hookinstall.MachinePath(d.home), `{"agents_root": "/tmp"}`, 0o644) },
		"openbot_exe must exist":    func() { d.write(hookinstall.MachinePath(d.home), `{"openbot_exe": "/nonexistent/openbot"}`, 0o644) },
		"env file must be 0600":     func() { d.write(hookinstall.EnvFile(d.home), string(envFile), 0o644) },
		"the DSN must be in a file": func() { d.write(hookinstall.EnvFile(d.home), "HARNESS_TAIL=off\n", 0o600) },
		"a wrong DSN is reported": func() {
			d.write(hookinstall.EnvFile(d.home), "HARNESS_DSN=postgres://u:p@127.0.0.1:1/none?connect_timeout=2\n", 0o600)
		},
	} {
		breakIt()
		// The DSN in the environment never replaces the file (AC-S2-006).
		if out, err := run("HARNESS_DSN=" + d.dsn); err == nil {
			t.Errorf("%s: -check passed\n%s", name, out)
		} else if strings.Contains(out, "u:p@") {
			t.Errorf("%s: the DSN is in the output: %s", name, out)
		}
		d.write(hookinstall.MachinePath(d.home), string(machine), 0o644)
		d.write(hookinstall.EnvFile(d.home), string(envFile), 0o600)
	}
	if out, err := run(); err != nil {
		t.Fatalf("-check after the restore: %v\n%s", err, out)
	}
}
