package intel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// Lesson is the body of `kch lesson` (C.6).
type Lesson struct {
	Cause       string `json:"cause"`
	Fix         string `json:"fix"`
	AppliesWhen string `json:"applies_when"`
	Limits      string `json:"limits"`
}

// MinLessonChars is the minimum length of every lesson field (C.6).
const (
	MinLessonChars = 15
	maxLessonChars = 1500
)

// Check returns 422 naming every field that is too short or too long.
func (l Lesson) Check() error {
	var bad []string
	for name, v := range map[string]string{"cause": l.Cause, "fix": l.Fix, "applies_when": l.AppliesWhen, "limits": l.Limits} {
		if n := utf8.RuneCountInString(strings.TrimSpace(v)); n < MinLessonChars || n > maxLessonChars {
			bad = append(bad, name)
		}
	}
	if len(bad) > 0 {
		slices.Sort(bad)
		return kerr.New(kerr.Invalid, "mỗi trường cause, fix, applies_when, limits cần %d–%d ký tự; chưa đạt: %s",
			MinLessonChars, maxLessonChars, strings.Join(bad, ", "))
	}
	return nil
}

// LessonCommand is the exact command an agent uses for a lesson.
const LessonCommand = "kch lesson <<'END'\n" +
	`{"cause":"nguyên nhân gốc","fix":"cách đã sửa","applies_when":"khi nào áp dụng","limits":"khi nào không áp dụng"}` + "\nEND"

// LessonRequest is the agent-facing text of lesson_requested.
func LessonRequest(f LessonFacts) string {
	var why []string
	if f.FailedAttempts > 0 {
		why = append(why, fmt.Sprintf("%d/%d lần verify FAIL (check: %s)", f.FailedAttempts, f.Attempts, strings.Join(f.FailedChecks, ", ")))
	}
	if f.ScopeViolations > 0 {
		why = append(why, fmt.Sprintf("%d lần ghi ngoài phạm vi", f.ScopeViolations))
	}
	if f.Feedback {
		why = append(why, "có feedback của chủ sản phẩm")
	}
	return "Ghi lại bài học của task này (" + strings.Join(why, "; ") + ") bằng MỘT lệnh Bash riêng, mỗi trường ≥ 15 ký tự:\n" + LessonCommand
}

// LessonPending reports whether a lesson was requested on the lease and not recorded since.
func LessonPending(ctx context.Context, tx pgx.Tx, taskID, leaseID string) (bool, error) {
	var pending bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal r WHERE r.task_id = $1 AND r.type = 'LESSON_REQUESTED'
			AND r.payload->>'lease_id' = $2
			AND NOT EXISTS (SELECT 1 FROM journal d WHERE d.task_id = $1 AND d.type = 'LESSON_RECORDED'
				AND d.payload->>'lease_id' = $2 AND d.seq > r.seq))`, taskID, leaseID).Scan(&pending)
	return pending, err
}

// LessonStopOnce reports whether the Stop hook may still block for the
// lesson of a lease, and records that it does (journal LESSON_STOP_BLOCKED):
// spec C.6 blocks Stop once per lease for a missing lesson (REV-005 #7).
func LessonStopOnce(ctx context.Context, tx pgx.Tx, projectID, taskID, leaseID string) (bool, error) {
	var done bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM journal WHERE task_id = $1 AND type = 'LESSON_STOP_BLOCKED'
		AND payload->>'lease_id' = $2)`, taskID, leaseID).Scan(&done); err != nil || done {
		return false, err
	}
	_, err := tx.Exec(ctx, `INSERT INTO journal (project_id, task_id, type, payload)
		VALUES ($1, $2, 'LESSON_STOP_BLOCKED', jsonb_build_object('lease_id', $3::text))`, projectID, taskID, leaseID)
	return err == nil, err
}

// InsertRuntimeSource stores the immutable facts of a lesson as a Source with
// origin RUNTIME (C.6): the runtime recorded them, about the given agent.
// Runtime scope.
func InsertRuntimeSource(ctx context.Context, tx pgx.Tx, projectID, about, kind, content string) (string, error) {
	sum := sha256.Sum256([]byte(content))
	var id string
	err := tx.QueryRow(ctx, `INSERT INTO source (project_id, kind, origin, content, content_hash, created_by)
		VALUES ($1, $2, 'RUNTIME', $3, $4, $5) RETURNING id::text`, projectID, kind, content, hex.EncodeToString(sum[:]), about).Scan(&id)
	return id, err
}
