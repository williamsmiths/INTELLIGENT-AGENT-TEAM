package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
)

const (
	hookTimeout  = 10 * time.Second  // the installer gives a hook 15 s
	verbTimeout  = 880 * time.Second // `kch verify` runs inside the pre-tool hook, which gets 900 s
	maxHookInput = 8 << 20
)

// hookCall is what the installer wrote on the hook's command line. Nothing
// here comes from the environment: every KCH_* variable is ignored, so a
// process that controls the provider's environment cannot point the hook at
// another socket or switch its fail-closed mode off (AC-S2-005, REV-004 #5).
type hookCall struct {
	event     string
	socket    string
	workspace string // directory of the settings file the installer wrote; NOT the cwd of the hook input
	enrolled  bool
}

// hookMain parses "kch hook <event> --socket <path> --workspace <dir> [--enrolled]".
// A hook never fails on its own usage error: it prints nothing and exits 0.
func hookMain(args []string, stdin io.Reader, stdout io.Writer) int {
	var c hookCall
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--enrolled":
			c.enrolled = true
		case (a == "--socket" || a == "--workspace") && i+1 < len(args):
			if a == "--socket" {
				c.socket = args[i+1]
			} else {
				c.workspace = args[i+1]
			}
			i++
		case c.event == "":
			c.event = a
		}
	}
	if c.event == "" || c.socket == "" {
		fmt.Fprintln(os.Stderr, "usage: kch hook <event> --socket <path> --workspace <dir> [--enrolled]")
		if c.enrolled && c.event == "pre-tool" {
			printJSON(stdout, denyOutput("hook của harness được cấu hình sai (thiếu --socket)."))
		}
		return 0
	}
	return c.run(stdin, stdout)
}

// hookInput is the part of Claude Code's payload the client reads or forwards.
// Tool outputs and file contents can be megabytes; the server needs none of
// them (REV-003 #9).
type hookInput struct {
	SessionID      string `json:"session_id"`
	CWD            string `json:"cwd"`
	ToolName       string `json:"tool_name"`
	ToolUseID      string `json:"tool_use_id"`
	StopHookActive bool   `json:"stop_hook_active"`
	ToolInput      struct {
		Command      string `json:"command,omitempty"`
		FilePath     string `json:"file_path,omitempty"`
		NotebookPath string `json:"notebook_path,omitempty"`
		Path         string `json:"path,omitempty"`
		Pattern      string `json:"pattern,omitempty"`
	} `json:"tool_input"`
	// Added by the client: the effective config of the workspace (spec S2 A.4).
	SettingsSHA256 string   `json:"settings_sha256"`
	ExtraConfig    []string `json:"extra_config"`
	KchVerb        bool     `json:"kch_verb,omitempty"` // a lone `kch <verb>`: replaced by the verb call, never run as Bash
}

// run forwards one hook event and prints the answer in Claude's hook output format.
func (c hookCall) run(stdin io.Reader, stdout io.Writer) int {
	raw, _ := io.ReadAll(io.LimitReader(stdin, maxHookInput))
	var in hookInput
	_ = json.Unmarshal(raw, &in)
	command := in.ToolInput.Command
	in.ToolInput.Command = "" // the server judges Bash by its effects, not by its text
	if c.event == "pre-tool" && in.ToolName == "Bash" {
		_, kind, _ := parseKch(command)
		in.KchVerb = kind == kchCall
	}
	if c.workspace != "" {
		in.SettingsSHA256 = hookinstall.Current(hookinstall.SettingsPath(c.workspace))
		in.ExtraConfig = hookinstall.ExtraConfig(c.workspace)
	}
	body, _ := json.Marshal(in)
	status, answer, err := call(c.socket, http.MethodPost, "/v1/hooks/claude/"+c.event, body, hookTimeout)
	var env struct {
		Output json.RawMessage `json:"output"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(answer, &env) != nil {
		return c.unavailable(stdout, status)
	}
	if len(env.Output) > 0 && string(env.Output) != "null" {
		fmt.Fprintln(stdout, string(env.Output))
		return 0
	}
	if c.event == "pre-tool" && in.ToolName == "Bash" {
		switch cmd, kind, reason := parseKch(command); kind {
		case kchCall:
			return c.verb(stdout, cmd, in.SessionID)
		case kchCompound:
			printJSON(stdout, denyOutput("lệnh kch phải là MỘT lệnh Bash riêng: chạy đúng một lệnh `kch <verb> …` riêng, không `cd … &&`, không ghép; cwd không cần. "+
				"Tham số đặt trong nháy đơn, không chuyển hướng ("+reason+"). Gõ lại, ví dụ: kch verify"))
		}
	}
	return 0
}

// unavailable is the answer when harnessd gave no decision. An enrolled
// workspace refuses the tool call (fail-closed); every other event, and a
// workspace that is not enrolled, lets it through. Both are logged locally and
// become HOOK_FAILCLOSED / HOOK_FAILOPEN when harnessd is back (L3, AC-S2-005).
func (c hookCall) unavailable(stdout io.Writer, status int) int {
	closed := c.enrolled && c.event == "pre-tool"
	if home, err := os.UserHomeDir(); err == nil {
		agent := filepath.Base(c.workspace)
		if c.workspace == "" {
			agent = "unknown"
		}
		_ = hookinstall.AppendFailOpen(hookinstall.HookLog(home), hookinstall.FailOpen{At: time.Now().UTC(), Agent: agent, Event: c.event, Closed: closed})
	}
	if closed {
		why := "không kết nối được"
		if status != 0 {
			why = fmt.Sprintf("trả HTTP %d", status)
		}
		printJSON(stdout, denyOutput("harness không sẵn sàng ("+why+"): tool bị từ chối cho tới khi harnessd chạy lại. Dừng lại và báo cho chủ sản phẩm."))
	}
	return 0
}

func denyOutput(reason string) map[string]any {
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse",
		"permissionDecision": "deny", "permissionDecisionReason": "[kc-harness] " + reason}}
}

func printJSON(w io.Writer, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if enc.Encode(v) == nil {
		_, _ = w.Write(buf.Bytes())
	}
}
