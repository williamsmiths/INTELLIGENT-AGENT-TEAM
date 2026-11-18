package attesttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Level is the main function of the helper binary: one process of the tree.
// It starts the next level, or acts when it is the last one.
func Level() int {
	var p Plan
	if err := json.Unmarshal([]byte(os.Getenv(envPlan)), &p); err != nil || len(p.Steps) == 0 {
		return fail("bad plan")
	}
	me, rest := p.Steps[0], p.Steps[1:]
	if me.Orphan && !orphaned() {
		return fail("still attached to the level above")
	}
	if me.Chdir != "" {
		if err := os.Chdir(me.Chdir); err != nil {
			return fail(err.Error())
		}
	}
	if len(rest) == 0 {
		if len(p.Exec) > 0 {
			return run(p)
		}
		return request(p)
	}
	next := rest[0]
	exe := next.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return fail(err.Error())
		}
	}
	p.Steps = rest
	plan, err := json.Marshal(p)
	if err != nil {
		return fail(err.Error())
	}
	cmd := exec.Command(exe)
	cmd.Dir = next.Dir
	cmd.Env = append(append(cleanEnv(), next.Env...), envPlan+"="+string(plan), envParent+"="+strconv.Itoa(os.Getpid()))
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if next.Orphan {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			return fail(err.Error())
		}
		return 0 // exit at once: the level below loses its parent
	}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		return fail(err.Error())
	}
	return 0
}

// orphaned waits until the level above has exited and this process was re-parented.
func orphaned() bool {
	parent, _ := strconv.Atoi(os.Getenv(envParent))
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if os.Getppid() != parent {
			return true
		}
	}
	return false
}

// cleanEnv is the environment without the helper's own variables.
func cleanEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, envPlan+"=") && !strings.HasPrefix(kv, envParent+"=") {
			env = append(env, kv)
		}
	}
	return env
}

// request makes the HTTP call of the plan over its unix socket.
func request(p Plan) int {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", p.Socket)
	}}}
	method := p.Method
	if method == "" {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://harness"+p.Path, strings.NewReader(p.Body))
	if err != nil {
		return answer(Answer{Error: err.Error()})
	}
	for k, v := range p.Header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return answer(Answer{Error: err.Error()})
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return answer(Answer{Status: resp.StatusCode, Body: string(body)})
}

// run executes the command of the plan as a child of this level.
func run(p Plan) int {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.Exec[0], p.Exec[1:]...)
	cmd.Env = cleanEnv()
	cmd.Stdin = strings.NewReader(p.Stdin)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, os.Stderr
	a := Answer{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return answer(Answer{Error: err.Error()})
		}
		a.Exit = exit.ExitCode()
	}
	a.Body = out.String()
	return answer(a)
}

func answer(a Answer) int {
	raw, _ := json.Marshal(a)
	fmt.Println(string(raw))
	return 0
}

func fail(msg string) int {
	answer(Answer{Error: "attesttest: " + msg})
	return 1
}
