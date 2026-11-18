package intel

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"
)

// Convergence thresholds (C.4): identical failing verifies in a row.
const (
	SteerAt = 2 // the reply and the next hook carry the steer text, +1 consult once per task
	ParkAt  = 3 // the task waits for the owner (reason nonprogress)
)

// tailLines is how much of each failing check's log enters the fingerprint.
const tailLines = 5

var (
	timeRe = regexp.MustCompile(`[0-9]{4}-[0-9]{2}-[0-9]{2}[t ][0-9:.]+z?`)
	durRe  = regexp.MustCompile(`\b[0-9]+(\.[0-9]+)?(ns|µs|us|ms|s|m|h)\b`)
	hexRe  = regexp.MustCompile(`\b(0x)?[0-9a-f]{7,64}\b`)
	tmpRe  = regexp.MustCompile(`/tmp/[^\s:'"]+`)
)

// NormalizeTail keeps the last non-empty lines of a log with what changes from
// run to run without being the error (timestamps, durations, hashes and
// addresses, temporary paths, spacing) removed, so the same error gives the
// same text and a different error a different one.
func NormalizeTail(log string) string {
	var lines []string
	for _, l := range strings.Split(log, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > tailLines {
		lines = lines[len(lines)-tailLines:]
	}
	for i, l := range lines {
		l = tmpRe.ReplaceAllString(strings.ToLower(l), "/tmp/…")
		l = timeRe.ReplaceAllString(l, "<time>")
		l = durRe.ReplaceAllString(l, "<dur>")
		l = hexRe.ReplaceAllString(l, "<hex>")
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

// Fingerprint is sha256 of the failing check ids and the normalized end of
// each failing check's log (C.4). An all-pass attempt has fingerprint "".
func Fingerprint(failing []string, tails map[string]string) string {
	if len(failing) == 0 {
		return ""
	}
	ids := slices.Clone(failing)
	slices.Sort(ids)
	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id + "\x00" + NormalizeTail(tails[id]) + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Repeats counts identical failing attempts in a row, this one included (C.4):
// same failing set and same fingerprint as the previous attempt continue the
// run; anything else (progress, a changed error, a pass, a resume by the
// owner) starts again. prev is nil when there is no previous attempt or the
// owner resumed the task after it.
func Repeats(prev *AttemptRow, failing []string, fingerprint string) int {
	if len(failing) == 0 {
		return 0
	}
	if prev != nil && prev.Fingerprint == fingerprint && sameSet(prev.Failing, failing) {
		return prev.Repeats + 1
	}
	return 1
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// Convergence is the outcome of an attempt for the agent and the owner.
type Convergence struct {
	Repeats int    `json:"repeats"`
	Status  string `json:"status"` // PROGRESS, STEER or PARKED; "" when convergence is off
	Steer   string `json:"steer,omitempty"`
	Bonus   bool   `json:"bonus_consult,omitempty"`
}

// Judge turns a repeat count into the convergence status of C.4.
func Judge(repeats int, bonusAlreadyGranted bool, failing []string) Convergence {
	c := Convergence{Repeats: repeats, Status: "PROGRESS"}
	switch {
	case repeats >= ParkAt:
		c.Status = "PARKED"
	case repeats == SteerAt:
		c.Status, c.Bonus = "STEER", !bonusAlreadyGranted
		c.Steer = Steer(failing, c.Bonus)
	}
	return c
}
