package attest

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// procFS is the mounted proc file system the chain is read from.
type procFS string

// stat returns the start time and the parent of a process.
func (fs procFS) stat(pid int) (start uint64, parent int, err error) {
	raw, err := os.ReadFile(filepath.Join(string(fs), strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, 0, errors.New("process is gone")
	}
	return parseStat(string(raw))
}

// parseStat reads fields 4 (ppid) and 22 (starttime) of a stat line. The
// command name (field 2) is in parentheses and may itself contain spaces and
// parentheses, so the fields are counted from the LAST ")".
func parseStat(line string) (start uint64, parent int, err error) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 {
		return 0, 0, errors.New("malformed stat")
	}
	fields := strings.Fields(line[end+1:]) // fields[0] is field 3 (state)
	if len(fields) < 20 {
		return 0, 0, errors.New("malformed stat")
	}
	parent, err = strconv.Atoi(fields[1])
	if err != nil || parent < 0 {
		return 0, 0, errors.New("malformed stat")
	}
	start, err = strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0, 0, errors.New("malformed stat")
	}
	return start, parent, nil
}

// read returns one process and its parent pid. The start time is read before
// and after the links, so exe and cwd belong to the same process as the pid.
func (fs procFS) read(pid int) (Proc, int, error) {
	start, parent, err := fs.stat(pid)
	if err != nil {
		return Proc{}, 0, err
	}
	dir := filepath.Join(string(fs), strconv.Itoa(pid))
	p := Proc{PID: pid, StartTime: start, Exe: link(filepath.Join(dir, "exe")), CWD: link(filepath.Join(dir, "cwd"))}
	again, parentAgain, err := fs.stat(pid)
	if err != nil || again != start || parentAgain != parent {
		return Proc{}, 0, errors.New("process changed while it was read")
	}
	return p, parent, nil
}

// link reads a /proc link. A target the kernel marks as deleted is not a path
// that can be compared with a configured executable, so it reads as "".
func link(path string) string {
	target, err := os.Readlink(path)
	if err != nil || !filepath.IsAbs(target) || strings.HasSuffix(target, " (deleted)") {
		return ""
	}
	return target
}

// PeerPID returns the process id of the peer of a unix socket connection, as
// the kernel recorded it when the peer connected (SO_PEERCRED).
func PeerPID(conn *net.UnixConn) (int, error) {
	pid, _, err := PeerCred(conn)
	return pid, err
}

// PeerCred returns the pid and the uid of the peer of a unix socket connection.
func PeerCred(conn *net.UnixConn) (pid, uid int, err error) {
	if conn == nil {
		return 0, 0, fmt.Errorf("%w: not a unix socket connection", ErrNotAttested)
	}
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrNotAttested, err)
	}
	var cred *syscall.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, fmt.Errorf("%w: %v", ErrNotAttested, err)
	}
	if credErr != nil || cred == nil || cred.Pid <= 0 {
		return 0, 0, fmt.Errorf("%w: no peer credentials", ErrNotAttested)
	}
	return int(cred.Pid), int(cred.Uid), nil
}
