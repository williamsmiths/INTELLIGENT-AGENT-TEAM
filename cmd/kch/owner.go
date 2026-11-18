package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/attest"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
)

const ownerTimeout = 30 * time.Minute // an Owner verify can run long checks

// ownerRequest sends one request over the owner socket; a non-2xx status is an error.
func ownerRequest(method, path string, body []byte) ([]byte, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	status, out, err := call(hookinstall.OwnerSocket(home), method, path, body, ownerTimeout)
	if err != nil {
		return nil, err
	}
	if status < 200 || status > 299 {
		return nil, fmt.Errorf("%d %s", status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func printPretty(out []byte) {
	var pretty bytes.Buffer
	if json.Indent(&pretty, out, "", "  ") == nil {
		out = pretty.Bytes()
	}
	fmt.Println(strings.TrimSpace(string(out)))
}

// ownerCall is "kch owner METHOD path [json]" (PM-S2-01).
func ownerCall(args []string) error {
	if len(args) < 2 || len(args) > 3 {
		return fmt.Errorf("usage: kch owner <METHOD> <path> [json]")
	}
	var body []byte
	if len(args) == 3 {
		body = []byte(args[2])
	}
	out, err := ownerRequest(strings.ToUpper(args[0]), args[1], body)
	if err != nil {
		return err
	}
	printPretty(out)
	return nil
}

// whoami prints what this process attests as: on the owner socket, or with
// "agent" on the agent socket (spec S2 A.4).
func whoami(args []string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	socket := hookinstall.OwnerSocket(home)
	if len(args) == 1 && args[0] == "agent" {
		socket = hookinstall.AgentSocket(home)
	} else if len(args) != 0 {
		return fmt.Errorf("usage: kch whoami [agent]")
	}
	status, out, err := call(socket, http.MethodGet, "/v1/whoami", nil, hookTimeout)
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("%d %s", status, strings.TrimSpace(string(out)))
	}
	printPretty(out)
	return nil
}

// enroll is "kch enroll <principal-id> <workspace-dir>" (AC-S2-011): it
// generates the hooks and the sandbox of the agent from the machine config
// and the principal's memberships, installs them in the workspace and
// registers the hash of the file. Running it again refreshes all of it.
func enroll(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: kch enroll <principal-id> <workspace-dir>")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	machine, err := hookinstall.LoadMachine(hookinstall.MachinePath(home), home)
	if err != nil {
		return err
	}
	out, err := ownerRequest(http.MethodGet, "/v1/principals/"+args[0], nil)
	if err != nil {
		return err
	}
	var who struct {
		ID             string   `json:"id"`
		AgentID        string   `json:"openbot_agent_id"`
		RepoRoots      []string `json:"repo_roots"`
		ReferenceRoots []string `json:"reference_roots"`
	}
	if err := json.Unmarshal(out, &who); err != nil || who.ID == "" {
		return fmt.Errorf("harnessd trả lời không đọc được cho principal %s", args[0])
	}
	if who.AgentID == "" {
		return fmt.Errorf("principal %s không gắn với agent OpenBot nào", args[0])
	}
	workspace := attest.Canonical(args[1])
	if want := filepath.Join(machine.AgentsRoot, who.AgentID); workspace != want {
		return fmt.Errorf("workspace phải là %s (agents_root + openbot_agent_id của principal), không phải %s", want, workspace)
	}
	tmp := filepath.Join(machine.PilotTmpRoot, who.AgentID)
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		return fmt.Errorf("thư mục tạm riêng: %w", err)
	}
	env := map[string]string{"TMPDIR": tmp, "GOCACHE": filepath.Join(tmp, "go-build"), "GOPROXY": "off"}
	for k, v := range machine.SandboxEnv {
		env[k] = v
	}
	path, sum, err := hookinstall.Install(hookinstall.Generate(hookinstall.Input{
		Workspace: workspace, Binary: attest.Self(), Socket: hookinstall.AgentSocket(home), Enrolled: true, Home: attest.Canonical(home),
		RepoRoots: who.RepoRoots, ReferenceRoots: who.ReferenceRoots, ReadRoots: machine.SandboxReadRoots, TmpRoot: tmp,
		HiddenTmp: hookinstall.OtherSessionTmp(os.Getuid(), workspace), Env: env,
		SharedRoot: attest.Canonical(filepath.Join(filepath.Dir(machine.AgentsRoot), "Shared"))}))
	if err != nil {
		return err
	}
	reg, _ := json.Marshal(map[string]string{"settings_path": path, "sha256": sum})
	if _, err := ownerRequest(http.MethodPost, "/v1/principals/"+who.ID+"/hook-install", reg); err != nil {
		return fmt.Errorf("đã ghi %s nhưng chưa đăng ký được hash: %w", path, err)
	}
	fmt.Fprintf(os.Stderr, "đã ghi danh %s: %s (sha256 %s)\n", who.AgentID, path, sum)
	return nil
}
