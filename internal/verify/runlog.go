package verify

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// runLog is the log file of one check. It hashes exactly the bytes it writes,
// so the evidence's log_sha256 is the sha256 of the file on disk.
type runLog struct {
	f         *os.File
	h         hash.Hash
	path      string
	left      int64
	truncated bool
	err       error
}

// openLog creates <dir>/<task>-<check>-<UTC timestamp>.log, mode 0600, never overwriting.
func openLog(dir, taskID, checkID string, at time.Time) (*runLog, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("verify: log directory: %w", err)
	}
	name := fmt.Sprintf("%s-%s-%s.log", safeName(taskID), safeName(checkID), at.UTC().Format("20060102T150405.000000000Z"))
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("verify: create log: %w", err)
	}
	return &runLog{f: f, h: sha256.New(), path: path, left: MaxLogBytes}, nil
}

// Write keeps the first MaxLogBytes and drains the rest, so a noisy command
// neither fills the disk nor blocks on a full pipe.
func (l *runLog) Write(p []byte) (int, error) {
	n := len(p)
	if int64(len(p)) > l.left {
		p = p[:l.left]
		l.truncated = true
	}
	if len(p) > 0 && l.err == nil {
		if _, err := l.f.Write(p); err != nil {
			l.err = err
		} else {
			l.h.Write(p)
			l.left -= int64(len(p))
		}
	}
	return n, nil
}

// finish closes the file and returns its sha256.
func (l *runLog) finish() (string, error) {
	if l.truncated && l.err == nil {
		note := []byte(fmt.Sprintf("\n[output truncated at %d bytes]\n", MaxLogBytes))
		if _, err := l.f.Write(note); err != nil {
			l.err = err
		} else {
			l.h.Write(note)
		}
	}
	if err := l.f.Close(); err != nil && l.err == nil {
		l.err = err
	}
	return hex.EncodeToString(l.h.Sum(nil)), l.err
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			return r
		}
		return '_'
	}, s)
}
