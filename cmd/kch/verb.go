package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

var verbRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// sessionHeader names the Claude session of a verb call (spec S2 E.9-2; api.SessionHeader).
const sessionHeader = "X-KCH-Session"

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,200}$`)

// verb answers the agent's `kch <verb> …` (AC-S2-004): it posts the verb to
// harnessd and replaces the Bash command by one that prints the answer. The
// command the agent typed never runs.
//
// session is the session_id of the hook input (set by Claude Code, not by the
// agent): it travels in X-KCH-Session so harnessd can tell which OpenBot
// conversation the call comes from (spec S2 E.9-2). Nothing the agent typed
// reaches a header: its words go into the JSON body only (AC-S2-086).
func (c hookCall) verb(stdout io.Writer, cmd kchCommand, session string) int {
	if !verbRe.MatchString(cmd.Verb) {
		printJSON(stdout, rewrite("cách dùng: kch <status|context|verify|release|search|note|propose|remember|delegate|…> [tham số]", true))
		return 0
	}
	var body []byte
	switch {
	case cmd.HasBody && len(cmd.Args) == 0:
		body = []byte(cmd.Body) // the heredoc is the JSON body, byte for byte
	default:
		args := cmd.Args
		if args == nil {
			args = []string{}
		}
		body, _ = json.Marshal(map[string]any{"args": args, "stdin": cmd.Body})
	}
	var headers []string
	if sessionRe.MatchString(session) {
		headers = []string{sessionHeader, session}
	}
	status, answer, err := call(c.socket, http.MethodPost, "/v1/agent/"+cmd.Verb, body, verbTimeout, headers...)
	if err != nil || status >= 500 {
		if c.enrolled {
			return c.unavailable(stdout, status)
		}
		printJSON(stdout, rewrite("kch "+cmd.Verb+": harness không sẵn sàng.", true))
		return 0
	}
	var out struct {
		Text  *string `json:"text"`
		Error string  `json:"error"`
	}
	_ = json.Unmarshal(answer, &out)
	switch {
	case status < 300 && out.Text != nil:
		printJSON(stdout, rewrite(*out.Text, false))
	case out.Error != "":
		printJSON(stdout, rewrite(fmt.Sprintf("kch %s: %s (HTTP %d)", cmd.Verb, out.Error, status), true))
	default:
		printJSON(stdout, rewrite(fmt.Sprintf("kch %s: harness trả HTTP %d", cmd.Verb, status), true))
	}
	return 0
}

// rewrite is the PreToolUse answer that makes Bash print text instead of
// running what the agent typed. The text travels in a quoted heredoc, so the
// shell interprets nothing in it; the delimiter is random and does not occur
// in the text. failed makes the command exit 1 so the model sees a failure.
func rewrite(text string, failed bool) map[string]any {
	text = strings.ReplaceAll(text, "\x00", "")
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	d := delimiter(text)
	command := "cat <<'" + d + "'\n" + text + d + "\n"
	if failed {
		command += "exit 1\n"
	}
	return map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": "PreToolUse", "permissionDecision": "allow",
		"permissionDecisionReason": "kc-harness trả lời lệnh kch", "updatedInput": map[string]any{"command": command}}}
}

// delimiter returns a heredoc terminator that is no line of text.
func delimiter(text string) string {
	for {
		buf := make([]byte, 8)
		if _, err := rand.Read(buf); err != nil {
			panic(err) // crypto/rand does not fail on Linux
		}
		d := "KCH_" + hex.EncodeToString(buf)
		if !strings.Contains(text, d) {
			return d
		}
	}
}
