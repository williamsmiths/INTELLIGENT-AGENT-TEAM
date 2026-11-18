// Command kch is the client of the kongCowork Intelligence Harness (spec S2
// part A). It holds no token and opens no TCP connection: harnessd knows the
// caller from the process tree on the other end of a unix socket.
//
//   - `kch hook …` is run by Claude Code for every hook event. It also carries
//     the agent's own commands: when the Bash command of a tool call is exactly
//     one `kch <verb> …`, the pre-tool hook asks harnessd and replaces the
//     command with one that prints the answer (AC-S2-004).
//   - `kch owner …`, `kch enroll …` and `kch whoami` use the owner socket.
package main

import (
	"fmt"
	"os"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		usage()
		return 2
	}
	var err error
	switch args[0] {
	case "hook":
		return hookMain(args[1:], os.Stdin, os.Stdout)
	case "owner":
		err = ownerCall(args[1:])
	case "enroll":
		err = enroll(args[1:])
	case "whoami":
		err = whoami(args[1:])
	case "-h", "--help", "help":
		usage()
		return 0
	default:
		// An agent verb typed where no hook could answer it: inside a compound
		// command, or in a shell that is not a Claude Code tool call.
		err = fmt.Errorf("lệnh `kch %s` chỉ chạy được khi là MỘT lệnh Bash riêng của agent (không ghép với &&, ;, |, không chuyển hướng): "+
			"hook của harness trả lời thay cho lệnh. Trong sandbox không có kết nối nào tới harness", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "kch:", err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, `kch — kongCowork Intelligence Harness client

Agent (typed as ONE Bash command; the pre-tool hook answers it — project and task come from the lease):
  kch status | context | verify | release
  kch search <words>
  kch note <kind> <text>                    finding, evidence, proposal, conflict, decision, impact, next_action
  kch propose <key> <kind> <title> [body]   project knowledge (candidate)
  kch remember <key> <title> [body]         agent-level note (candidate)
  kch <verb> <<'EOF' … EOF                  a JSON or text body through a quoted heredoc
  kch delegate <<'EOF' {"to":…} EOF         Lead: give one task to the Worker of a specialty
  kch delegate-policy                       Lead: the Owner's checks and protected paths

Owner (owner socket; any unsandboxed process of this OS user):
  kch owner <METHOD> <path> [json]          call an Owner API endpoint
  kch enroll <principal-id> <workspace-dir> generate and install hooks + sandbox for an agent, register the hash
  kch whoami [agent]                        what this process attests as (owner socket, or the agent socket)

Hook (written into .claude/settings.json by kch enroll):
  kch hook <event> --socket <path> --workspace <dir> [--enrolled]`)
}
