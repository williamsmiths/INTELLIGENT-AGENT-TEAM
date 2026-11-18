package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// The S1 agent commands as verbs (spec S2 C.9 "Bí danh theo lease của các
// endpoint S1"). Each one runs the S1 handler itself as the caller, with the
// task or project taken from the lease, so every rule of the S1 endpoint
// applies unchanged; only the answer is turned into text for the model.
func init() {
	RegisterAgentVerb("status", verbStatus)
	RegisterAgentVerb("context", verbContext)
	RegisterAgentVerb("search", verbSearch)
	RegisterAgentVerb("note", verbNote)
	RegisterAgentVerb("propose", verbPropose)
	RegisterAgentVerb("remember", verbRemember)
	RegisterAgentVerb("verify", verbVerify)
	RegisterAgentVerb("release", verbRelease)
}

// capture is the response writer of an in-process handler call.
type capture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (c *capture) Header() http.Header         { return c.header }
func (c *capture) WriteHeader(status int)      { c.status = status }
func (c *capture) Write(b []byte) (int, error) { return c.body.Write(b) }

// alias runs an S1 handler as the caller of a verb and decodes its JSON
// answer. A refusal of the handler comes back as the kerr error of its status.
func (s *Server) alias(ctx context.Context, c *AgentCall, h http.HandlerFunc, method string, pathValues map[string]string, query url.Values, body any) (map[string]any, error) {
	var rd *bytes.Reader
	if raw, err := json.Marshal(body); err == nil && body != nil {
		rd = bytes.NewReader(raw)
	} else {
		rd = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, principalKey, c.Actor), method, "/?"+query.Encode(), rd)
	if err != nil {
		return nil, err
	}
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	rec := &capture{header: http.Header{}, status: http.StatusOK}
	h(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.body.Bytes(), &out)
	if rec.status < 400 {
		return out, nil
	}
	msg, _ := out["error"].(string)
	switch rec.status {
	case http.StatusForbidden:
		return nil, kerr.New(kerr.Forbidden, "%s", msg)
	case http.StatusNotFound:
		return nil, kerr.New(kerr.NotFound, "%s", msg)
	case http.StatusConflict:
		return nil, kerr.New(kerr.Conflict, "%s", msg)
	case http.StatusUnprocessableEntity:
		return nil, kerr.New(kerr.Invalid, "%s", msg)
	}
	return nil, errors.New("agent verb: the aliased handler failed")
}

func verbStatus(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent: %s (%s)\n", c.Actor.Name, c.Actor.OpenBotAgentID)
	data := map[string]any{"principal_id": c.Actor.ID, "agent_id": c.Actor.OpenBotAgentID}
	if c.Held == nil {
		fmt.Fprintf(&b, "Chưa giữ task nào. Thành viên của %d project.\n", len(c.Actor.Roles))
		// Say which project the verbs will use here (spec E.9-4), so an agent in a bound
		// conversation knows it can search and delegate without a lease (team pilot, design step).
		if pid, err := c.Project(); err == nil {
			var slug string
			_ = c.scoped(ctx, s, func(tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT slug FROM project WHERE id = $1`, pid).Scan(&slug)
			})
			role := c.Actor.RoleIn(pid)
			data["project_id"], data["role"] = pid, role
			fmt.Fprintf(&b, "Project của cuộc hội thoại này: %s (vai trò %s). Dùng được: kch search <từ khóa>", slug, role)
			if role == "lead" {
				b.WriteString("; kch delegate <<'END' {\"to\":\"design|engineering|quality\",\"title\":\"…\",\"outcome\":\"…\",\"allowed_paths\":[\"…\"]} END để giao việc")
			}
			b.WriteString(".\n")
		} else {
			fmt.Fprintf(&b, "Chưa xác định được project cho cuộc hội thoại này (%s).\n", err.Error())
		}
		return AgentReply{Text: b.String(), Data: data}, nil
	}
	t, l := c.Held.Task, c.Held.Lease
	data["task_id"], data["project_id"], data["mode"] = t.ID, t.ProjectID, l.Mode
	fmt.Fprintf(&b, "Task: %s — %s\nTrạng thái: %s, rev %d, mode %s, lease hết hạn %s\n", t.Key, t.Title, t.State, t.Revision, l.Mode,
		l.ExpiresAt.Local().Format("15:04 02/01"))
	if len(t.AllowedPaths) > 0 {
		fmt.Fprintf(&b, "Phạm vi ghi: %s\n", strings.Join(t.AllowedPaths, ", "))
	}
	var budget work.BudgetView
	if err := c.scoped(ctx, s, func(tx pgx.Tx) (err error) {
		budget, err = work.GetBudget(ctx, tx, c.Actor, t.ID)
		return err
	}); err != nil {
		return AgentReply{}, err
	}
	b.WriteString(budgetLine(budget) + "\n")
	return AgentReply{Text: b.String(), Data: data}, nil
}

func verbContext(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	t, err := c.Task()
	if err != nil {
		return AgentReply{}, err
	}
	out, err := s.alias(ctx, c, s.getContext, http.MethodGet, map[string]string{"id": t.ID}, nil, nil)
	if err != nil {
		return AgentReply{}, err
	}
	text, _ := out["text"].(string)
	return AgentReply{Text: text, Data: map[string]any{"context_hash": out["context_hash"]}}, nil
}

func verbSearch(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	project, err := c.Project()
	if err != nil {
		return AgentReply{}, err
	}
	q := strings.TrimSpace(strings.Join(c.Args, " ") + " " + c.Stdin)
	if q == "" {
		return AgentReply{}, kerr.New(kerr.Invalid, "cách dùng: kch search <từ khóa>")
	}
	out, err := s.alias(ctx, c, s.searchKnowledge, http.MethodGet, map[string]string{"p": project}, url.Values{"q": {q}, "limit": {"8"}}, nil)
	if err != nil {
		return AgentReply{}, err
	}
	hits, _ := out["hits"].([]any)
	if len(hits) == 0 {
		return AgentReply{Text: "Không tìm thấy mục nào cho: " + q + "\n", Data: map[string]any{"count": 0}}, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d mục (dữ liệu của harness; mục không phải AUTHORITATIVE là chưa kiểm chứng):\n", len(hits))
	for _, h := range hits {
		hit, _ := h.(map[string]any)
		item, _ := hit["Item"].(map[string]any)
		fmt.Fprintf(&b, "- %v v%v [%v, %v, %v] %v: %s\n", item["Key"], item["Version"], item["Kind"], item["Aspect"], item["Trust"],
			item["Title"], clipText(fmt.Sprint(item["Body"]), 400))
	}
	return AgentReply{Text: b.String(), Data: map[string]any{"count": len(hits)}}, nil
}

func verbNote(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	project, err := c.Project()
	if err != nil {
		return AgentReply{}, err
	}
	text := strings.TrimSpace(strings.Join(c.rest(1), " ") + "\n" + c.Stdin)
	if len(c.Args) < 1 || text == "" {
		return AgentReply{}, kerr.New(kerr.Invalid, "cách dùng: kch note <finding|evidence|proposal|conflict|decision|impact|next_action> <nội dung>")
	}
	body := map[string]any{"kind": c.Args[0], "body": text}
	if c.Held != nil {
		body["taskid"] = c.Held.Task.ID
	}
	out, err := s.alias(ctx, c, s.postBlackboard, http.MethodPost, map[string]string{"p": project}, nil, body)
	if err != nil {
		return AgentReply{}, err
	}
	return AgentReply{Status: http.StatusCreated, Text: fmt.Sprintf("Đã ghi %s #%v lên blackboard của project.\n", c.Args[0], out["ID"]),
		Data: map[string]any{"id": out["ID"]}}, nil
}

// verbPropose: kch propose <key> <kind> <title> [body] — a PROJECT candidate (PM-H-05).
func verbPropose(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	if len(c.Args) < 3 {
		return AgentReply{}, kerr.New(kerr.Invalid, "cách dùng: kch propose <key> <kind> <tiêu đề> [nội dung]")
	}
	return s.candidate(ctx, c, map[string]any{"level": "PROJECT", "key": c.Args[0], "kind": c.Args[1], "aspect": "TO_BE",
		"trust": "CANDIDATE", "basis": "proposed", "title": c.Args[2], "body": c.text(3)}, "đề xuất (CANDIDATE, chờ chủ sản phẩm xác nhận)")
}

// verbRemember: kch remember <key> <title> [body] — an AGENT-level note of the caller.
func verbRemember(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	if len(c.Args) < 2 {
		return AgentReply{}, kerr.New(kerr.Invalid, "cách dùng: kch remember <key> <tiêu đề> [nội dung]")
	}
	return s.candidate(ctx, c, map[string]any{"level": "AGENT", "key": c.Args[0], "kind": "experience", "aspect": "AS_IS",
		"trust": "CANDIDATE", "basis": "inferred", "title": c.Args[1], "body": c.text(2)}, "ghi nhớ riêng của bạn trong project này")
}

func (s *Server) candidate(ctx context.Context, c *AgentCall, body map[string]any, what string) (AgentReply, error) {
	project, err := c.Project()
	if err != nil {
		return AgentReply{}, err
	}
	out, err := s.alias(ctx, c, s.createKnowledge, http.MethodPost, map[string]string{"p": project}, nil, body)
	if err != nil {
		return AgentReply{}, err
	}
	return AgentReply{Status: http.StatusCreated, Text: fmt.Sprintf("Đã lưu %v v%v — %s.\n", out["Key"], out["Version"], what),
		Data: map[string]any{"id": out["ID"], "key": out["Key"], "version": out["Version"]}}, nil
}

func verbRelease(ctx context.Context, s *Server, c *AgentCall) (AgentReply, error) {
	t, err := c.Task()
	if err != nil {
		return AgentReply{}, err
	}
	out, err := s.alias(ctx, c, s.releaseTask, http.MethodPost, map[string]string{"id": t.ID}, nil, nil)
	if err != nil {
		return AgentReply{}, err
	}
	task, _ := out["task"].(map[string]any)
	state, _ := task["state"].(string)
	return AgentReply{Text: fmt.Sprintf("Đã nhả task %s (trạng thái task: %s). Bạn không còn giữ lease.\n", t.Key, state),
		Data: map[string]any{"task_state": state}}, nil
}

// rest returns the arguments from index i on.
func (c *AgentCall) rest(i int) []string {
	if len(c.Args) <= i {
		return nil
	}
	return c.Args[i:]
}

// text joins the arguments from index i on with the heredoc, if any.
func (c *AgentCall) text(i int) string {
	return strings.TrimSpace(strings.Join(c.rest(i), " ") + "\n" + c.Stdin)
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
