package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// shellWords runs the command line in a real shell with a kch that prints
// its arguments, one per line terminated by NUL.
func shellWords(t *testing.T, command string) []string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\0' \"$a\"; done\n"
	if err := os.WriteFile(filepath.Join(dir, "kch"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("shell could not run %q: %v", command, err)
	}
	words := strings.Split(string(out), "\x00")
	return words[:len(words)-1]
}

// runShell runs a command the way the Bash tool does and returns its stdout.
func runShell(t *testing.T, command string, wantExit int) string {
	t.Helper()
	out, err := exec.Command("bash", "-c", command).Output()
	exit := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		exit = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if exit != wantExit {
		t.Fatalf("exit %d, want %d: %q", exit, wantExit, command)
	}
	return string(out)
}
