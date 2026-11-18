package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"

	"github.com/williamsmiths/kongcowork-harness/internal/attest"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
)

// config is everything harnessd runs with. The locations are fixed below the
// home directory of the OS user (spec S2 A.1); the rest comes from two files
// in the config directory, which is hidden from every sandbox:
//
//	harnessd.env  (mode 0600) HARNESS_DSN and optional switches
//	machine.json  openbot_exe, agents_root, sandbox_read_roots, pilot_tmp_root
type config struct {
	DSN           string // PostgreSQL admin DSN (migrations run as this role; requests run as harness_app)
	Home          string
	ConfigDir     string
	StateDir      string // evidence logs, base store, hook log, agent socket
	LogDir        string
	HookLog       string
	OwnerSocket   string
	AgentSocket   string
	Machine       hookinstall.Machine
	Attest        attest.Config
	OpenBotDB     string       // host SQLite file to tail read-only; empty disables the tailer
	AutomationDir string       // host automation API url/token files
	Embed         embed.Config // HARNESS_EMBED_URL etc.; empty URL = semantic retrieval off
	Notify        bool         // HARNESS_NOTIFY=on: deliver assignments and feedback into agents through the host
	PacketChars   int
}

const dsnKey = "HARNESS_DSN"

// loadConfig reads the two files. The DSN is taken from harnessd.env only: it
// is never read from the process environment, so it does not have to be
// there and cannot be read back from /proc/<pid>/environ (AC-S2-006).
func loadConfig() (config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return config{}, err
	}
	home = attest.Canonical(home)
	c := config{Home: home, ConfigDir: filepath.Dir(hookinstall.EnvFile(home)), StateDir: hookinstall.StateDir(home),
		OwnerSocket: hookinstall.OwnerSocket(home), AgentSocket: hookinstall.AgentSocket(home), HookLog: hookinstall.HookLog(home),
		PacketChars: 6000}
	c.LogDir = filepath.Join(c.StateDir, "evidence")
	file, err := readEnvFile(hookinstall.EnvFile(home))
	if err != nil {
		return c, err
	}
	get := func(name, fallback string) string {
		if v, ok := file[name]; ok && v != "" {
			return v
		}
		if v := os.Getenv(name); v != "" && name != dsnKey {
			return v
		}
		return fallback
	}
	if c.DSN = file[dsnKey]; c.DSN == "" {
		return c, fmt.Errorf("%s is missing in %s", dsnKey, hookinstall.EnvFile(home))
	}
	if c.Machine, err = hookinstall.LoadMachine(hookinstall.MachinePath(home), home); err != nil {
		return c, err
	}
	if st, err := os.Stat(c.Machine.OpenBotExe); err != nil || st.IsDir() {
		return c, fmt.Errorf("machine config: openbot_exe %s is not a file", c.Machine.OpenBotExe)
	}
	c.Attest = attest.Config{OpenBotExe: c.Machine.OpenBotExe, AgentsRoot: c.Machine.AgentsRoot, SelfExe: attest.Self()}
	if c.Attest.SelfExe == "" {
		return c, errors.New("cannot resolve the executable of harnessd")
	}
	c.OpenBotDB = get("HARNESS_OPENBOT_DB", filepath.Join(home, ".config", "OpenBot", "openbot.db"))
	c.AutomationDir = get("HARNESS_AUTOMATION_DIR", filepath.Join(home, ".config", "OpenBot", "automation"))
	c.Notify = strings.EqualFold(get("HARNESS_NOTIFY", ""), "on")
	if c.Embed, err = embed.FromLookup(func(k string) string { return get(k, "") }); err != nil {
		return c, err
	}
	if n, err := strconv.Atoi(get("HARNESS_PACKET_CHARS", "")); err == nil && n > 0 {
		c.PacketChars = n
	}
	if strings.EqualFold(get("HARNESS_TAIL", ""), "off") {
		c.OpenBotDB = ""
	} else if _, err := os.Stat(c.OpenBotDB); err != nil {
		c.OpenBotDB = "" // no host database on this machine: nothing to observe
	}
	return c, nil
}

// readEnvFile parses KEY=VALUE lines ("#" comments, optional quotes). The
// file holds the database password, so anything but mode 0600 is refused.
func readEnvFile(path string) (map[string]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("harnessd env file: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("harnessd env file %s must have mode 0600 (it is %04o)", path, st.Mode().Perm())
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("harnessd env file: %w", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		out[strings.TrimSpace(key)] = value
	}
	return out, sc.Err()
}
