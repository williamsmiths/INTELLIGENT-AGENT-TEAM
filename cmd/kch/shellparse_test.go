package main

import (
	"reflect"
	"strings"
	"testing"
)

// AC-S2-004: only a command that is exactly one kch invocation is a kch call.
func TestAC_S2_004_ParseKchCall(t *testing.T) {
	type want struct {
		verb string
		args []string
		body string
		has  bool
	}
	for command, w := range map[string]want{
		"kch verify":                        {verb: "verify"},
		"  kch   status  \n":                {verb: "status"},
		"/opt/harness/bin/kch verify":       {verb: "verify"},
		"./bin/kch context":                 {verb: "context"},
		"'kch' verify":                      {verb: "verify"},
		"kch":                               {},
		"kch search nút thêm nhóm":          {verb: "search", args: []string{"nút", "thêm", "nhóm"}},
		`kch note finding 'a b' "c d" e\ f`: {verb: "note", args: []string{"finding", "a b", "c d", "e f"}},
		`kch note finding 'đã thấy; không && không | cũng không $(x) *'`: {verb: "note", args: []string{"finding", "đã thấy; không && không | cũng không $(x) *"}},
		`kch note finding "nháy \"kép\" và \\ gạch"`:                     {verb: "note", args: []string{"finding", `nháy "kép" và \ gạch`}},
		"kch note finding 'dòng 1\ndòng 2'":                              {verb: "note", args: []string{"finding", "dòng 1\ndòng 2"}},
		"kch propose K-1 rule '' x":                                      {verb: "propose", args: []string{"K-1", "rule", "", "x"}},
		"kch consult product 'câu hỏi' --refs k1,k2":                     {verb: "consult", args: []string{"product", "câu hỏi", "--refs", "k1,k2"}},
		"kch understand <<'EOF'\n{\"goal\": \"x $HOME `id`\"}\nEOF":      {verb: "understand", body: "{\"goal\": \"x $HOME `id`\"}\n", has: true},
		"kch understand <<'EOF'\n{\"a\":1}\n\n  {\"b\": 2}\nEOF\n":       {verb: "understand", body: "{\"a\":1}\n\n  {\"b\": 2}\n", has: true},
		"kch understand << \"END\"\nx\nEOF\nEND":                         {verb: "understand", body: "x\nEOF\n", has: true},
		"kch understand <<\\EOF\n$x\nEOF":                                {verb: "understand", body: "$x\n", has: true},
		"kch note finding<<EOF\nvăn bản thường\nEOF":                     {verb: "note", args: []string{"finding"}, body: "văn bản thường\n", has: true},
		"kch understand <<'EOF'\nEOF":                                    {verb: "understand", has: true},
	} {
		cmd, kind, reason := parseKch(command)
		if kind != kchCall {
			t.Errorf("%q: kind %d (%s), want a kch call", command, kind, reason)
			continue
		}
		if cmd.Verb != w.verb || !reflect.DeepEqual(cmd.Args, w.args) && !(len(cmd.Args) == 0 && len(w.args) == 0) || cmd.Body != w.body || cmd.HasBody != w.has {
			t.Errorf("%q: parsed %+v, want %+v", command, cmd, w)
		}
	}
}

// AC-S2-004: a compound or expanding command that starts with kch is refused
// with a hint, never run as a kch call.
func TestAC_S2_004_RejectsCompoundForms(t *testing.T) {
	for _, command := range []string{
		"kch verify && rm -rf x", "kch verify || true", "kch verify; ls", "kch verify | head", "kch verify & ", "kch verify &",
		"kch verify 2>&1", "kch verify > out.txt", "kch search x < in.txt", "kch verify >> log", "kch note finding `id`",
		"kch note finding $(id)", "kch note finding $HOME", `kch note finding "$HOME"`, "kch note finding \"`id`\"", "kch search *.go",
		"kch search what?", "kch search [a]", "kch search {a,b}", "kch search ~/x", "kch verify # comment", "kch verify\nrm -rf x",
		"kch (verify)", "kch verify <<< text", "kch understand <<-'EOF'\nx\nEOF", "kch understand <<'EOF'\nx\n", "kch understand <<'EOF'",
		"kch understand <<'EOF' extra\nx\nEOF", "kch understand <<'EOF'\nx\nEOF\nrm -rf x", "kch understand <<EOF\n$HOME\nEOF",
		"kch understand <<EOF\n`id`\nEOF", "kch understand <<'A' <<'B'\nx\nA\ny\nB", "kch note 'unterminated", `kch note "unterminated`, `kch note x\`,
		// kch in command position after cd/&&, ;, |, a new line (C run camera-plate-validation-C-1): refused with the hint, never run.
		"cd /x && kch understand <<'END'\n{}\nEND", "ls; kch status", "true | kch status", "cd repo && kch verify",
		"cd /x\nkch verify", "true || /opt/bin/kch context", "ls & kch status",
		// REV-007 M2: subshell, assignment prefix, keywords and command runners keep command position.
		"(cd x && kch verify)", "(kch verify)", "KCH_AGENT=x kch verify", "FOO=1 kch status", "sudo kch verify", "sudo -n kch status",
		"time kch verify", "env -i kch status", "if true; then kch status; fi", "ls && \\kch status", "{ kch status; }", "nohup kch verify &",
	} {
		if cmd, kind, reason := parseKch(command); kind != kchCompound || reason == "" {
			t.Errorf("%q: kind %d reason %q parsed %+v, want refused as compound", command, kind, reason, cmd)
		}
	}
}

// AC-S2-004: a command that merely contains the word kch is forwarded like any other.
func TestAC_S2_004_OtherCommandsAreNotKch(t *testing.T) {
	for _, command := range []string{
		"echo kch verify", "ls", "", "   ", "kchx verify",
		"/usr/bin/kch-tool verify", "$(kch) verify", "`kch` verify", "\"$K\" verify", "git commit -m 'kch verify'", "cat kch",
		"kch\x00 verify", "kchverify", "# kch verify", "!kch verify",
		"env FOO=1 make test", "sudo -n ls", "time go test ./...", "FOO=1 make", "(cd x && make)", "echo foo(kch)",
		// kch only as data: an argument, quoted text, a heredoc body, a comment, or without a verb.
		"grep -n kchx file", "ls && echo kch status", "git commit -m 'a; kch verify'", "cat <<'EOF' > notes.md\nkch verify\nEOF",
		"ls # ; kch verify", "ls; kch", "ls; kch 'X Y'",
	} {
		if cmd, kind, _ := parseKch(command); kind != notKch {
			t.Errorf("%q: kind %d parsed %+v, want not a kch command", command, kind, cmd)
		}
	}
}

// The parser must agree with a real shell on what the words are.
func TestAC_S2_004_WordsMatchARealShell(t *testing.T) {
	for _, command := range []string{
		`kch note finding 'a b' "c d" e\ f g'h'"i"`, `kch note 'x' "" '' y`, "kch search a\tb   c", `kch note "tab\there" 'q"q' "s's"`,
		"kch note 'multi\nline' end", `kch propose K-9 rule "tiêu đề có dấu" nội\ dung`,
	} {
		cmd, kind, reason := parseKch(command)
		if kind != kchCall {
			t.Fatalf("%q: %s", command, reason)
		}
		got := append([]string{cmd.Verb}, cmd.Args...)
		if want := shellWords(t, command); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: parser %q, shell %q", command, got, want)
		}
	}
}

func TestRewritePrintsTheTextThroughAQuotedHeredoc(t *testing.T) {
	for _, text := range []string{"một dòng", "nhiều\ndòng\n", "$HOME `id` $(id) \\ ' \" EOF\nKCH_\n", "", "x\x00y"} {
		out := rewrite(text, false)["hookSpecificOutput"].(map[string]any)
		command := out["updatedInput"].(map[string]any)["command"].(string)
		if out["permissionDecision"] != "allow" || out["hookEventName"] != "PreToolUse" {
			t.Fatalf("output: %v", out)
		}
		want := strings.ReplaceAll(text, "\x00", "")
		if !strings.HasSuffix(want, "\n") {
			want += "\n"
		}
		if got := runShell(t, command, 0); got != want {
			t.Errorf("text %q printed as %q", text, got)
		}
	}
	command := rewrite("lỗi", true)["hookSpecificOutput"].(map[string]any)["updatedInput"].(map[string]any)["command"].(string)
	if got := runShell(t, command, 1); got != "lỗi\n" {
		t.Errorf("failed answer printed %q", got)
	}
}

// kchInside must end on every input, however broken (it runs inside a hook).
func TestKchInsideEndsOnAnyInput(t *testing.T) {
	parts := []string{"<", ">", "<<", ";", "|", "&", "#", "'", "\"", "\\", "\n", " ", "kch", "verify", "x"}
	var walk func(prefix string, depth int)
	walk = func(prefix string, depth int) {
		_ = kchInside(prefix)
		if depth == 0 {
			return
		}
		for _, p := range parts {
			walk(prefix+p, depth-1)
		}
	}
	walk("", 4)
}
