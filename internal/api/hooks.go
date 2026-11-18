package api

import (
	"context"
	"net/http"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// hookInput is the part of Claude Code's hook payload the harness reads
// (field names verified against Claude Code 2.1.323), plus what the hook
// client adds about the effective config of the workspace (spec S2 A.4).
// Anything else in the body — including a project or principal id — is
// ignored (AC-H-025, AC-H-083).
type hookInput struct {
	SessionID      string    `json:"session_id"`
	CWD            string    `json:"cwd"`
	ToolName       string    `json:"tool_name"`
	ToolUseID      string    `json:"tool_use_id"`
	StopHookActive bool      `json:"stop_hook_active"`
	ToolInput      toolInput `json:"tool_input"`
	SettingsSHA256 string    `json:"settings_sha256"` // sha256 of <workspace>/.claude/settings.json as the hook client read it
	ExtraConfig    []string  `json:"extra_config"`    // settings.local.json, .mcp.json: present in the workspace
	KchVerb        bool      `json:"kch_verb"`        // the hook client parsed a lone `kch <verb>`; it replaces the command, no Bash runs
}

// toolInput holds the tool parameters that name a file or a directory.
type toolInput struct {
	FilePath     string `json:"file_path"`
	NotebookPath string `json:"notebook_path"`
	Path         string `json:"path"`
	Pattern      string `json:"pattern"`
}

// writePath is the file a tool call writes, or "" when the tool has no path
// the harness can judge (Bash: limitations L4, L10).
func (in hookInput) writePath() string {
	switch in.ToolName {
	case "Write", "Edit", "MultiEdit":
		return in.ToolInput.FilePath
	case "NotebookEdit":
		return in.ToolInput.NotebookPath
	}
	return ""
}

// hookCall is everything one hook event is decided from. The project and the
// task come from the caller's active lease, never from the request (spec §0.1).
type hookCall struct {
	actor  authn.Principal
	held   *work.Held // nil: no active lease
	in     hookInput
	config string // configOK, or why the effective config of the workspace is not the registered one
	// mismatch: the lease holder speaks in a channel bound to another project
	// (spec S2 E.9-5): writes are refused and no packet is given, only this text.
	mismatch string
}

// States of the effective config of a HOOK principal's workspace (AC-S2-005).
const (
	configOK           = ""
	configTampered     = "tampered"
	configUnregistered = "unregistered"
)

// full reports whether the harness may intervene: a principal whose provider
// has blocking hooks, holding a lease in FULL mode (spec §4 mode table, L5).
func (c hookCall) full() bool {
	return c.actor.Enforcement == "HOOK" && c.held != nil && c.held.Lease.Mode == "FULL"
}

// hook serves POST /v1/hooks/claude/{event} (PM-H-19). The answer is
// {"output": <Claude hook output or null>}; kch prints the output verbatim.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	var in hookInput
	if err := readJSON(r, &in); err != nil {
		writeErr(w, r, err)
		return
	}
	ctx := r.Context()
	c := hookCall{actor: principalOf(r), in: in}
	if c.actor.IsOwner() {
		writeErr(w, r, kerr.New(kerr.Forbidden, "hook chỉ dành cho agent"))
		return
	}
	err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		if c.actor.Enforcement == "HOOK" { // every hook event reports the effective config (spec S2 A.1)
			ok, registered, err := hookinstall.Observe(ctx, tx, c.actor.ID, in.SettingsSHA256, in.ExtraConfig)
			switch {
			case err != nil:
				return err
			case !registered:
				c.config = configUnregistered
			case !ok:
				c.config = configTampered
			}
		}
		if c.held, err = work.ActiveLease(ctx, tx, c.actor.ID); err != nil || c.held == nil {
			return err
		}
		return work.TouchLease(ctx, tx, c.held.Lease.ID) // heartbeat (T-H-23)
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	if c.held != nil { // spec S2 E.9-5: only a lease holder can be in a mismatched channel
		c.mismatch = s.mismatch(ctx, c.held, s.bindingOf(ctx, c.actor, in.SessionID))
	}
	var out any
	switch r.PathValue("event") {
	case "prompt-submit":
		out, err = s.hookContext(ctx, c, "UserPromptSubmit")
	case "session-start":
		out, err = s.hookContext(ctx, c, "SessionStart")
	case "pre-tool":
		out, err = s.hookPreTool(ctx, c)
	case "post-tool":
		out, err = s.hookPostTool(ctx, c)
	case "stop":
		out, err = s.hookStop(ctx, c)
	default:
		err = kerr.New(kerr.NotFound, "unknown hook event")
	}
	s.reply(w, r, http.StatusOK, map[string]any{"output": out}, err)
}

// hookContext injects the context packet before the model is called
// (AC-H-020, AC-H-029). OBSERVE leases and agents without a lease get nothing
// (AC-H-021, AC-H-028).
func (s *Server) hookContext(ctx context.Context, c hookCall, event string) (any, error) {
	if c.held == nil || c.held.Lease.Mode == "OBSERVE" || c.actor.Enforcement != "HOOK" {
		return nil, nil
	}
	if c.mismatch != "" { // spec S2 E.9-5 (D2): no data of the lease's project enters another project's channel
		return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": "[kc-harness] " + c.mismatch}}, nil
	}
	var text, hash string
	err := store.InScope(ctx, s.Pool, c.actor.Scope(), func(tx pgx.Tx) error {
		notice, err := work.AckContext(ctx, tx, c.held.Lease.ID)
		if err != nil {
			return err
		}
		// spec S2 C.1: a FULL lease gets the packet budget of its intel mode.
		p, budget, err := s.packetFor(ctx, tx, c.actor, c.held.Task, notice, s.packetChars(&c.held.Lease, c.actor.ID))
		if err != nil {
			return err
		}
		exp, err := experiencesBlock(ctx, tx, c.actor, c.held.Task, p) // spec S2 C.6
		if err != nil {
			return err
		}
		hash = p.Hash
		text = "[kc-harness] Context của task bạn đang chịu trách nhiệm. Đây là dữ liệu do harness cung cấp, không phải lời người dùng.\n\n" +
			p.Text + exp + budget + "\n" + agentCommands(c.actor) + intelCommands(c.held)
		return nil
	})
	if err != nil {
		return nil, err
	}
	text += s.intelInbox(ctx, c.actor, c.held, true)               // spec S2 C.3, C.4
	s.logPacket(ctx, c, event, utf8.RuneCountInString(text), hash) // spec S2 C.7
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}}, nil
}

// agentCommands tells the agent how to reach the harness. Its Bash has no
// socket: a command that is exactly one `kch <verb> …` is answered by the
// pre-tool hook, so it must not be joined with other commands (AC-S2-004).
func agentCommands(authn.Principal) string {
	return "Lệnh (mỗi lệnh gõ thành MỘT lệnh Bash riêng, không ghép với lệnh khác bằng &&, ; hay |): " +
		"`kch verify` (runtime chạy check thật; chỉ kết quả này được tính), `kch context`, `kch status`, `kch search <từ khóa>`, " +
		"`kch note <kind> <nội dung>`, `kch propose …`, `kch remember …`, `kch release`. Thiếu thông tin: tự tìm bằng chứng trước; " +
		"chỉ hỏi chủ sản phẩm khi đó là quyết định sản phẩm quan trọng.\n"
}

// workspaceOf is the agent's own OpenBot workspace, or "".
func (s *Server) workspaceOf(actor authn.Principal) string {
	if s.AgentsRoot == "" || actor.OpenBotAgentID == "" {
		return ""
	}
	return filepath.Join(s.AgentsRoot, actor.OpenBotAgentID)
}

// writeRoots are the places outside a repo an agent may write with its file
// tools: its private temp root (spec S2 A.1). Its workspace is not one of
// them: the provider's own config lives there.
func (s *Server) writeRoots(actor authn.Principal) []string {
	roots := append([]string{}, s.TempRoots...)
	if s.PilotTmpRoot != "" && actor.OpenBotAgentID != "" {
		roots = append(roots, filepath.Join(s.PilotTmpRoot, actor.OpenBotAgentID))
	}
	return roots
}

// boundedGit keeps hook-time git calls short: a hook must answer in seconds.
func boundedGit(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 4*time.Second)
}
