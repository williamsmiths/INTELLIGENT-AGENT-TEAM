package main

import (
	"path/filepath"
	"regexp"
	"strings"
)

// kchInside reports a `kch <verb>` in command position anywhere in a command
// that does not start with kch: after &&, ||, ;, |, & or a new line, e.g.
// `cd /repo && kch understand <<'END' …`. Such a command would run kch inside
// the sandbox, where it cannot reach the harness, and the agent would read
// "kch: command not found" as "the harness is not installed" (C run
// camera-plate-validation-C-1). It is refused with the hint instead of run.
// Quoted text and heredoc bodies are skipped: `echo 'a; kch verify'` or a
// heredoc line "kch verify" are data, and `echo kch status` names kch as an
// argument, not as a command.
func kchInside(s string) bool {
	atCmd := true     // the next word starts a simple command
	prefixed := false // the last word was a command runner (env, sudo, …) or one of its options
	var heredocs []string
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '\n':
			i++
			for _, d := range heredocs { // the bodies start on the next line
				i = skipHeredoc(s, i, d)
			}
			heredocs, atCmd = nil, true
		case c == ';' || c == '|' || c == '&' || c == '(':
			i, atCmd = i+1, true
		case c == ')':
			i, atCmd = i+1, false
		case c == '#' && (i == 0 || strings.ContainsRune(" \t\n;|&", rune(s[i-1]))): // a comment, up to the end of the line
			if end := strings.IndexByte(s[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = len(s)
			}
		case strings.HasPrefix(s[i:], "<<"):
			i += 2
			for i < len(s) && (s[i] == '-' || s[i] == ' ' || s[i] == '\t') {
				i++
			}
			var d string
			d, i = rawWord(s, i)
			if d = strings.Trim(d, `'"\`); d != "" {
				heredocs = append(heredocs, d)
			}
			atCmd = false
		case c == '<' || c == '>': // a redirection operator: its target is not a command
			i, atCmd = i+1, false
		default:
			var w string
			w, i = rawWord(s, i)
			bare := strings.TrimPrefix(strings.Trim(w, `'"`), `\`)
			if atCmd && filepath.Base(bare) == "kch" {
				j := skipBlank(s, i, false)
				if verb, _ := rawWord(s, j); verbRe.MatchString(strings.Trim(verb, `'"`)) {
					return true
				}
			}
			// A word that keeps the next word in command position (REV-007 M2): an assignment
			// prefix (FOO=1 kch …), a shell keyword (then/do/else/{/!), or a command runner
			// (time/env/sudo/nohup/exec/command/builtin) and its options.
			atCmd = atCmd && (assignRe.MatchString(bare) || cmdPrefix[bare] || (prefixed && strings.HasPrefix(bare, "-")))
			prefixed = atCmd && (cmdRunner[bare] || (prefixed && strings.HasPrefix(bare, "-")))
		}
	}
	return false
}

var (
	assignRe  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	cmdRunner = map[string]bool{"time": true, "env": true, "sudo": true, "nohup": true, "exec": true, "command": true, "builtin": true, "nice": true}
	cmdPrefix = map[string]bool{"then": true, "do": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "{": true, "!": true,
		"time": true, "env": true, "sudo": true, "nohup": true, "exec": true, "command": true, "builtin": true, "nice": true}
)

// rawWord returns the text of one word as written (quotes kept, quoted
// separators included) and the index after it.
func rawWord(s string, i int) (string, int) {
	start := i
	for i < len(s) {
		switch c := s[i]; {
		case c == ' ' || c == '\t' || c == '\n' || c == ';' || c == '|' || c == '&' || c == '<' || c == '>' || c == '(' || c == ')':
			return s[start:i], i
		case c == '\'' || c == '"':
			if end := strings.IndexByte(s[i+1:], c); end >= 0 {
				i += end + 2
			} else {
				return s[start:], len(s)
			}
		case c == '\\' && i+1 < len(s):
			i += 2
		default:
			i++
		}
	}
	return s[start:i], i
}

// skipHeredoc returns the index after the line that is exactly delim.
func skipHeredoc(s string, i int, delim string) int {
	for i < len(s) {
		line, _, found := strings.Cut(s[i:], "\n")
		i += len(line) + 1
		if line == delim || !found {
			break
		}
	}
	return min(i, len(s))
}
