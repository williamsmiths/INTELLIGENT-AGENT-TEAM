package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/guard"
	"github.com/williamsmiths/kongcowork-harness/internal/hookinstall"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// The tools of the provider that run OUTSIDE its sandbox are judged here, for
// every HOOK principal, in every lease mode and without a lease (spec S2 A.1
// "Tool chạy ngoài sandbox…", AC-S2-007, REV-004 #4). It is an allowlist:
//
//   - Read, Glob, Grep: only inside the agent's read roots.
//   - Write, Edit, MultiEdit, NotebookEdit: judged by the write rules
//     (hookPreTool, guardLooseWrite) — the task repo and the private temp root.
//   - mcp__*, WebFetch, WebSearch: refused.
//   - any other tool that carries a path parameter: refused.
//   - Bash: allowed — the provider's sandbox confines it (limitation L10).

var (
	readTools  = map[string]bool{"Read": true, "Glob": true, "Grep": true}
	writeTools = map[string]bool{"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true}
)

// toolGuard returns a deny for a tool call outside the allowlist, or nil.
func (s *Server) toolGuard(ctx context.Context, c hookCall) (any, error) {
	name, in := c.in.ToolName, c.in.ToolInput
	switch {
	case strings.HasPrefix(name, "mcp__") || name == "WebFetch" || name == "WebSearch":
		s.journalDenied(ctx, c, "TOOL_DENIED", "tool_denied")
		return deny("tool " + toolLabel(name) + " không được dùng trong workspace đã ghi danh (MCP và truy cập web bị tắt)."), nil
	case readTools[name]:
		targets, err := readTargets(name, in, c.in.CWD)
		if err != nil {
			s.journalDenied(ctx, c, "TOOL_DENIED", "bad_path")
			return deny("không xác định được đường dẫn đọc (" + err.Error() + "); dùng đường dẫn tuyệt đối."), nil // fail closed
		}
		roots, err := s.readRoots(ctx, c.actor)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			if !insideAny(roots, t) {
				s.journalDenied(ctx, c, "TOOL_DENIED", "read_outside")
				return deny("đường dẫn đọc nằm ngoài vùng được phép (workspace của bạn, repo và reference root của project bạn là thành viên, toolchain, thư mục tạm riêng)."), nil
			}
		}
	case writeTools[name], name == "Bash":
	case in.FilePath != "" || in.Path != "" || in.NotebookPath != "":
		s.journalDenied(ctx, c, "TOOL_DENIED", "unknown_path_tool")
		return deny("tool " + toolLabel(name) + " có tham số đường dẫn và không nằm trong danh sách tool được phép."), nil
	}
	return nil, nil
}

// readTargets returns the canonical locations a read tool call reaches: the
// file, or the directory searched plus the fixed part of a glob pattern (an
// absolute pattern, or one with "..", searches somewhere else than its base).
func readTargets(name string, in toolInput, cwd string) ([]string, error) {
	if name == "Read" {
		p, err := guard.Canonical(in.FilePath, cwd)
		return []string{p}, err
	}
	base := in.Path
	if base == "" {
		base = cwd
	}
	p, err := guard.Canonical(base, cwd)
	if err != nil {
		return nil, err
	}
	targets := []string{p}
	if name == "Glob" && in.Pattern != "" {
		fixed := globPrefix(in.Pattern)
		if !filepath.IsAbs(fixed) {
			fixed = p + "/" + fixed
		}
		q, err := guard.Canonical(fixed, "")
		if err != nil {
			return nil, err
		}
		targets = append(targets, q)
	}
	return targets, nil
}

// globPrefix is the leading part of a pattern made of plain path components.
func globPrefix(pattern string) string {
	parts := strings.Split(pattern, "/")
	for i, part := range parts {
		if strings.ContainsAny(part, "*?[{") {
			parts = parts[:i]
			break
		}
	}
	if len(parts) == 0 {
		return "."
	}
	if prefix := strings.Join(parts, "/"); prefix != "" {
		return prefix
	}
	return "/" // the pattern started with "/" and a wildcard followed at once
}

// readRoots are the directories a HOOK principal may read with file tools:
// its workspace, the repo and reference roots of its projects, the machine's
// toolchain roots, its own session temp directory and its write roots. The
// shared /tmp/claude-<uid> of other sessions is not among them (L11).
func (s *Server) readRoots(ctx context.Context, actor authn.Principal) ([]string, error) {
	var member work.Roots
	if err := s.runtime(ctx, func(tx pgx.Tx) (err error) {
		member, err = work.PrincipalRoots(ctx, tx, actor.ID)
		return err
	}); err != nil {
		return nil, err
	}
	roots := append(append(append([]string{}, member.Repos...), member.References...), s.ReadRoots...)
	roots = append(roots, s.writeRoots(actor)...)
	if ws := s.workspaceOf(actor); ws != "" {
		roots = append(roots, ws, hookinstall.SessionTmp(os.Getuid(), ws))
	}
	return roots, nil
}

// insideAny reports whether a canonical target lies in one of the roots.
func insideAny(roots []string, target string) bool {
	for _, r := range roots {
		if c, err := guard.Canonical(r, ""); err == nil && guard.Inside(c, target) {
			return true
		}
	}
	return false
}

// guardLooseWrite judges a file write of a principal that is not under a FULL
// lease: with an OBSERVE or CONTEXT lease the whole task repo is open except
// the always-protected paths; without a lease only the private temp root is
// (spec S2 A.1). path is "" for tools that write no file.
func (s *Server) guardLooseWrite(ctx context.Context, c hookCall, path string) any {
	if path == "" {
		return nil
	}
	var d guard.Decision
	if c.held != nil {
		d = guard.Rules{Repo: c.held.Task.RepoPath, Allowed: []string{"**"}, ExtraRoots: s.writeRoots(c.actor)}.CheckWrite(path, c.in.CWD)
	} else {
		res, err := guard.Resolve(path, c.in.CWD)
		switch {
		case err != nil:
			d = guard.Decision{Rule: "bad_path", Reason: "không xác định được đường dẫn ghi: " + err.Error()}
		case !insideAny(s.writeRoots(c.actor), res.Path):
			d = guard.Decision{Rule: "outside_scope", Reason: "chưa giữ task nào: chỉ ghi được vào thư mục tạm riêng của bạn"}
		case res.Dangling:
			d = guard.Decision{Rule: "dangling_symlink", Reason: "ghi qua symlink treo (đích chưa tồn tại) không được phép"}
		default:
			d = guard.Decision{Allow: true}
		}
	}
	if d.Allow {
		return nil
	}
	s.journalDenied(ctx, c, "WRITE_DENIED", d.Rule)
	return deny(d.Reason + ".")
}
