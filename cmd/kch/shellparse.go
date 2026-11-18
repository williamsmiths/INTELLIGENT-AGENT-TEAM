package main

import (
	"errors"
	"path/filepath"
	"strings"
)

// What a Bash command is to the pre-tool hook (AC-S2-004).
const (
	notKch      = iota // any other command: forwarded like every tool call
	kchCall            // exactly one `kch <verb> [args…]`, optionally with a heredoc body
	kchCompound        // kch but not one plain invocation (or kch after &&, ;, |, a new line): refused with a hint
)

// kchCommand is one parsed invocation.
type kchCommand struct {
	Verb    string
	Args    []string // words after the verb, as the shell would pass them
	Body    string   // heredoc text
	HasBody bool
}

var errExpansion = errors.New("có phần shell phải diễn giải ($, `, *, ?, ~, {…}); hãy đặt tham số trong nháy đơn")

// parseKch classifies a Bash command. Only a command the shell would run as
// one process — kch with literal words, nothing expanded, nothing chained,
// nothing redirected except one heredoc — is a kchCall. The words are split
// with POSIX shell quoting rules, so the server gets exactly what a real kch
// process would have got. reason says why a kch-looking command was refused.
func parseKch(command string) (cmd kchCommand, kind int, reason string) {
	if strings.ContainsRune(command, 0) {
		return cmd, notKch, ""
	}
	i := skipBlank(command, 0, true)
	first, next, err := lexWord(command, i)
	if err != nil || first == "" || filepath.Base(first) != "kch" {
		if kchInside(command) { // `cd /x && kch understand …` would run kch inside the sandbox: refuse with the hint
			return cmd, kchCompound, "kch nằm sau cd, &&, ;, | hoặc xuống dòng trong một lệnh ghép"
		}
		return cmd, notKch, "" // a command that merely contains the word kch is not ours
	}
	words, delim, quoted, rest, err := lexLine(command, next)
	if err != nil {
		return cmd, kchCompound, err.Error()
	}
	if delim == "" {
		if strings.TrimSpace(rest) != "" {
			return cmd, kchCompound, "nhiều dòng lệnh"
		}
	} else {
		body, after, ok := heredocBody(rest, delim)
		switch {
		case !ok:
			return cmd, kchCompound, "heredoc không có dòng kết thúc " + delim
		case strings.TrimSpace(after) != "":
			return cmd, kchCompound, "có lệnh khác sau heredoc"
		case !quoted && strings.ContainsAny(body, "$`\\"):
			return cmd, kchCompound, "heredoc không đặt nháy sẽ bị shell diễn giải; dùng <<'" + delim + "'"
		}
		cmd.Body, cmd.HasBody = body, true
	}
	if len(words) > 0 {
		cmd.Verb, cmd.Args = words[0], words[1:]
	}
	return cmd, kchCall, ""
}

// lexLine reads the words of the command line after the program name, up to
// the end of the line. A heredoc operator must be the last thing on the line;
// its delimiter is returned with whether it was quoted. rest is the text
// after the line.
func lexLine(s string, i int) (words []string, delim string, quoted bool, rest string, err error) {
	for {
		i = skipBlank(s, i, false)
		switch {
		case i >= len(s):
			return words, delim, quoted, "", nil
		case s[i] == '\n':
			return words, delim, quoted, s[i+1:], nil
		case delim != "":
			return nil, "", false, "", errors.New("có tham số hoặc lệnh sau heredoc")
		case strings.HasPrefix(s[i:], "<<"):
			i += 2
			if i < len(s) && (s[i] == '<' || s[i] == '-') {
				return nil, "", false, "", errors.New("chỉ nhận heredoc dạng <<'EOF'")
			}
			i = skipBlank(s, i, false)
			quoted = i < len(s) && (s[i] == '\'' || s[i] == '"' || s[i] == '\\')
			if delim, i, err = lexWord(s, i); err != nil || delim == "" || strings.ContainsAny(delim, " \t\n") {
				return nil, "", false, "", errors.New("heredoc thiếu từ kết thúc hợp lệ")
			}
		case strings.ContainsRune(";&|<>()", rune(s[i])):
			return nil, "", false, "", errors.New("lệnh ghép hoặc chuyển hướng (" + string(s[i]) + ")")
		default:
			var w string
			if w, i, err = lexWord(s, i); err != nil {
				return nil, "", false, "", err
			}
			words = append(words, w)
		}
	}
}

// lexWord reads one shell word starting at i and returns its literal value.
// It refuses everything the shell would expand or interpret.
func lexWord(s string, i int) (word string, next int, err error) {
	var b strings.Builder
	start := i
	for i < len(s) {
		c := s[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || strings.ContainsRune(";&|<>()", rune(c)):
			return b.String(), i, nil
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return "", i, errors.New("thiếu nháy đơn đóng")
			}
			b.WriteString(s[i+1 : i+1+end])
			i += end + 2
		case c == '"':
			i++
			for {
				if i >= len(s) {
					return "", i, errors.New("thiếu nháy kép đóng")
				}
				if s[i] == '"' {
					i++
					break
				}
				switch {
				case s[i] == '$' || s[i] == '`':
					return "", i, errExpansion
				case s[i] == '\\' && i+1 < len(s) && strings.ContainsRune("\"\\$`\n", rune(s[i+1])):
					if s[i+1] != '\n' { // backslash-newline is a line continuation
						b.WriteByte(s[i+1])
					}
					i += 2
				default:
					b.WriteByte(s[i])
					i++
				}
			}
		case c == '\\':
			if i+1 >= len(s) {
				return "", i, errors.New("dấu \\ ở cuối lệnh")
			}
			if s[i+1] != '\n' {
				b.WriteByte(s[i+1])
			}
			i += 2
		case c == '$' || c == '`' || c == '*' || c == '?' || c == '[' || c == '{':
			return "", i, errExpansion
		case (c == '~' || c == '#') && i == start:
			return "", i, errExpansion
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), i, nil
}

// skipBlank skips spaces and tabs, and newlines too when lines is set.
func skipBlank(s string, i int, lines bool) int {
	for i < len(s) && (s[i] == ' ' || s[i] == '\t' || (lines && s[i] == '\n')) {
		i++
	}
	return i
}

// heredocBody splits the text after the command line into the heredoc body
// and what follows its terminator line.
func heredocBody(rest, delim string) (body, after string, ok bool) {
	var lines []string
	for rest != "" {
		line, tail, _ := strings.Cut(rest, "\n")
		if line == delim {
			if len(lines) == 0 {
				return "", tail, true
			}
			return strings.Join(lines, "\n") + "\n", tail, true
		}
		lines = append(lines, line)
		rest = tail
	}
	return "", "", false
}
