package api_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// AC-S2-004: the agent's kch commands travel through the pre-tool hook. The
// hook client is the real kch binary, run from the command line written by
// `kch enroll`, as a child of the provider of a real process tree.
func TestAC_S2_004_AgentCommandsGoThroughTheHook(t *testing.T) {
	f := enrolled(t)
	f.o(http.StatusCreated, "POST", "/v1/projects/"+f.id["alpha"]+"/knowledge", map[string]any{"key": "REQ-1", "kind": "requirement",
		"aspect": "TO_BE", "trust": "AUTHORITATIVE", "scope": "feature:x", "title": "Xóa nhóm câu hỏi", "body": "ALPHA-RULE: phải xác nhận trước khi xóa."})
	task := f.task("T-1", "REQ-1")

	t.Run("AC-S2-004 kch status without a lease", func(t *testing.T) {
		out := f.bash("X", "kch status")
		lacks(t, out, "kch status") // the command the agent typed does not run: it was replaced
		has(t, f.printed(out, 0), "Agent: X (agent-X)", "Chưa giữ task nào")
	})
	t.Run("AC-S2-004 verbs that need a lease answer 409 as text with exit 1", func(t *testing.T) {
		has(t, f.printed(f.bash("X", "kch verify"), 1), "kch verify:", "chưa được giao task", "HTTP 409")
		has(t, f.printed(f.bash("X", "kch nonexistent-verb"), 1), "không có lệnh này", "verify")
		has(t, f.printed(f.bash("X", "kch"), 1), "cách dùng")
	})
	f.assign(http.StatusOK, task, "X", "FULL")
	t.Run("AC-S2-004 kch verify runs the checks of the lease's task and prints the result", func(t *testing.T) {
		text := f.printed(f.bash("X", "kch verify"), 0)
		has(t, text, "Verify task T-1 (rev 1): ĐẠT", "VERIFIED", "- unit: PASS", "- scope: PASS")
		if n := f.events("/v1/projects/"+f.id["alpha"]+"/events", "TASK_VERIFIED", ""); n == 0 && str(f.o(http.StatusOK, "GET", "/v1/tasks/"+task, nil), "task", "state") != "VERIFIED" {
			t.Fatal("the runner did not really verify the task")
		}
	})
	t.Run("AC-S2-004 context, search, note, propose, remember take project and task from the lease", func(t *testing.T) {
		has(t, f.printed(f.bash("X", "kch context"), 0), "T-1", "ALPHA-RULE")
		has(t, f.printed(f.bash("X", "kch search 'xóa nhóm'"), 0), "REQ-1", "AUTHORITATIVE", "ALPHA-RULE")
		has(t, f.printed(f.bash("X", "kch search khongcogi-zzz"), 0), "Không tìm thấy")
		has(t, f.printed(f.bash("X", "kch note finding 'nút thêm nằm ngoài Part; cần && sửa'"), 0), "Đã ghi finding #")
		has(t, f.printed(f.bash("X", "kch propose RULE-9 rule 'Tiêu đề' nội dung đề xuất"), 0), "Đã lưu RULE-9 v1", "CANDIDATE")
		has(t, f.printed(f.bash("X", "kch remember NOTE-1 'Ghi nhớ' chi tiết"), 0), "Đã lưu NOTE-1 v1")
		has(t, f.printed(f.bash("X", "kch note wrongkind text"), 1), "HTTP 422")
		board := f.o(http.StatusOK, "GET", "/v1/projects/"+f.id["alpha"]+"/blackboard?task="+task, nil)["_raw"].(string)
		has(t, board, "nút thêm nằm ngoài Part; cần \\u0026\\u0026 sửa", f.id["X"])
	})
	t.Run("AC-S2-004 a heredoc body reaches the server byte for byte", func(t *testing.T) {
		body := "{\"goal\": \"giữ $HOME và `id` nguyên vẹn\",\n  \"preserve\": [\"REQ-1\"], \"q\": \"a && b | c > d\"}\n"
		text := f.printed(f.bash("X", "kch echo-body <<'EOF'\n"+body+"EOF"), 0)
		if text != "body="+body+"|args=|stdin=\n" {
			t.Fatalf("the JSON body changed on the way:\n%q\nwant\n%q", text, "body="+body+"|args=|stdin=\n")
		}
		text = f.printed(f.bash("X", "kch echo-body a 'b c' <<'END'\ndòng 1\n$(rm -rf /) dòng 2\nEND\n"), 0)
		if text != "body=|args=a,b c|stdin=dòng 1\n$(rm -rf /) dòng 2\n" {
			t.Fatalf("args and heredoc: %q", text)
		}
		has(t, f.printed(f.bash("X", "kch echo-body <<'EOF'\nkhông phải JSON\nEOF"), 1), "HTTP 422")
	})
	t.Run("AC-S2-004 a compound command is not handled as kch; other commands pass", func(t *testing.T) {
		victim := filepath.Join(f.dir, "victim")
		if err := os.WriteFile(victim, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"kch verify && rm -rf " + victim, "kch verify; rm " + victim, "kch status | head -1", "kch verify 2>&1", "kch note finding $(cat " + victim + ")",
			"cd src && kch verify"} { // kch after cd … && would run in the sandbox: refused with the hint (C run camera-plate-validation-C-1)
			out := f.bash("X", command)
			has(t, out, `"permissionDecision":"deny"`, "MỘT lệnh Bash riêng")
			lacks(t, out, "updatedInput")
		}
		for _, command := range []string{"echo kch verify", "ls -la", "git commit -m 'kch verify'"} {
			if out := f.bash("X", command); out != "" {
				t.Fatalf("%q was not forwarded like any other command: %s", command, out)
			}
		}
		if _, err := os.Stat(victim); err != nil {
			t.Fatal("something ran the agent's command")
		}
	})
	t.Run("AC-S2-004 kch release ends the lease", func(t *testing.T) {
		has(t, f.printed(f.bash("X", "kch release"), 0), "Đã nhả task T-1")
		has(t, f.printed(f.bash("X", "kch status"), 0), "Chưa giữ task nào")
	})
	t.Run("AC-S2-004 a verb is refused for the Owner and typed outside a hook it only explains itself", func(t *testing.T) {
		if status, _ := f.from(f.detached(), f.ownerSock, "POST", "/v1/agent/status", nil); status != http.StatusNotFound {
			t.Fatalf("agent verb on the owner socket: %d", status)
		}
		if a := f.kchDirect("verify"); a.Exit != 1 || a.Body != "" { // the explanation goes to stderr
			t.Fatalf("kch verify typed directly: %+v", a)
		}
	})
}
