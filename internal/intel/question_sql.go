package intel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/journal"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// QuestionRow is one question of a lease holder to the owner (C.2).
type QuestionRow struct {
	ID              string     `json:"id"`
	ProjectID       string     `json:"project_id"`
	TaskID          string     `json:"task_id"`
	Seq             int        `json:"seq"`
	UnderstandingID string     `json:"understanding_id,omitempty"`
	AskedBy         string     `json:"asked_by"`
	TaskRevision    int        `json:"task_revision"`
	Text            string     `json:"text"`
	Options         []string   `json:"options"`
	Default         string     `json:"default"`
	Impact          string     `json:"impact"`
	Blocking        bool       `json:"blocking"`
	Status          string     `json:"status"`
	Answer          string     `json:"answer,omitempty"`
	AnsweredAt      *time.Time `json:"answered_at,omitempty"`
	Differs         *bool      `json:"differs,omitempty"`
	DecisionID      string     `json:"decision_id,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
}

// WaitingAfter is how long a blocking question stays OPEN before QUESTION_WAITING (C.2, REV-004 #17).
const WaitingAfter = 10 * time.Minute

const questionCols = `id, project_id, task_id, seq, COALESCE(understanding_id::text, ''), asked_by, task_revision, text, options,
	default_option, impact, blocking, status, COALESCE(answer, ''), answered_at, differs, COALESCE(decision_id::text, ''), created_at`

func scanQuestion(row pgx.Row) (QuestionRow, error) {
	var q QuestionRow
	var opts []byte
	if err := row.Scan(&q.ID, &q.ProjectID, &q.TaskID, &q.Seq, &q.UnderstandingID, &q.AskedBy, &q.TaskRevision, &q.Text, &opts,
		&q.Default, &q.Impact, &q.Blocking, &q.Status, &q.Answer, &q.AnsweredAt, &q.Differs, &q.DecisionID, &q.CreatedAt); err != nil {
		return q, err
	}
	return q, json.Unmarshal(opts, &q.Options)
}

// InsertQuestion stores an OPEN question with the next number of its task.
// The caller holds LockTask. The asker's own scope may write it (C.11).
func InsertQuestion(ctx context.Context, tx pgx.Tx, q QuestionRow) (QuestionRow, error) {
	opts, err := json.Marshal(orNone(q.Options))
	if err != nil {
		return QuestionRow{}, err
	}
	return scanQuestion(tx.QueryRow(ctx, `INSERT INTO question (project_id, task_id, seq, understanding_id, asked_by, task_revision,
			text, options, default_option, impact, blocking)
		VALUES ($1, $2, (SELECT COALESCE(max(seq), 0) + 1 FROM question WHERE task_id = $2), nullif($3, '')::uuid, $4, $5, $6, $7, $8, $9, $10)
		RETURNING `+questionCols, q.ProjectID, q.TaskID, q.UnderstandingID, q.AskedBy, q.TaskRevision, q.Text, opts, q.Default, q.Impact, q.Blocking))
}

// GetQuestion loads one question visible in the caller's scope (404 otherwise).
func GetQuestion(ctx context.Context, tx pgx.Tx, id string, lock bool) (QuestionRow, error) {
	if !isUUID(id) {
		return QuestionRow{}, kerr.New(kerr.NotFound, "question not found")
	}
	q := `SELECT ` + questionCols + ` FROM question WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	r, err := scanQuestion(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return QuestionRow{}, kerr.New(kerr.NotFound, "question not found")
	}
	return r, err
}

// ListQuestions lists questions of one task, or of the given projects when
// taskID is empty (nil projects: every project the scope sees), newest first.
func ListQuestions(ctx context.Context, tx pgx.Tx, taskID string, projects []string, status string) ([]QuestionRow, error) {
	rows, err := tx.Query(ctx, `SELECT `+questionCols+` FROM question
		WHERE ($1 = '' OR task_id = nullif($1, '')::uuid) AND ($2::uuid[] IS NULL OR project_id = ANY($2::uuid[]))
		  AND ($3 = '' OR status = $3) ORDER BY created_at DESC, seq DESC LIMIT 200`, taskID, projects, status)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (QuestionRow, error) { return scanQuestion(row) })
}

// OpenBlocking counts the OPEN blocking questions of a task.
func OpenBlocking(ctx context.Context, tx pgx.Tx, taskID string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM question WHERE task_id = $1 AND status = 'OPEN' AND blocking`, taskID).Scan(&n)
	return n, err
}

// MarkAnswered records the owner's answer (owner or runtime scope).
func MarkAnswered(ctx context.Context, tx pgx.Tx, id, answer, by string, differs bool, decisionID string) error {
	_, err := tx.Exec(ctx, `UPDATE question SET status = 'ANSWERED', answer = $2, answered_by = $3, answered_at = now(),
		differs = $4, decision_id = nullif($5, '')::uuid WHERE id = $1 AND status = 'OPEN'`, id, answer, by, differs, decisionID)
	return err
}

// Unsurfaced returns the OPEN questions of a task not yet listed in a Stop block, and marks them listed.
func Unsurfaced(ctx context.Context, tx pgx.Tx, taskID string) ([]QuestionRow, error) {
	rows, err := tx.Query(ctx, `UPDATE question SET surfaced_at = now() WHERE task_id = $1 AND status = 'OPEN' AND surfaced_at IS NULL
		RETURNING `+questionCols, taskID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (QuestionRow, error) { return scanQuestion(row) })
}

// JournalWaitingQuestions journals QUESTION_WAITING once for every blocking
// question OPEN longer than WaitingAfter: the owner may have answered in the
// host's chat, which the harness cannot see, and the operator enters the
// owner's words verbatim (C.2). The worker passes WaitingAfter. Payload: ids
// only. Runtime scope.
func JournalWaitingQuestions(ctx context.Context, tx pgx.Tx, after time.Duration) (int, error) {
	rows, err := tx.Query(ctx, `UPDATE question SET waiting_noted_at = now()
		WHERE status = 'OPEN' AND blocking AND waiting_noted_at IS NULL AND created_at < now() - make_interval(secs => $1)
		RETURNING id::text, project_id::text, task_id::text, seq`, after.Seconds())
	if err != nil {
		return 0, err
	}
	type due struct {
		ID, Project, Task string
		Seq               int
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[due])
	if err != nil {
		return 0, err
	}
	for _, q := range list {
		if _, err := journal.Append(ctx, tx, journal.Event{ProjectID: q.Project, TaskID: q.Task, Type: "QUESTION_WAITING",
			Payload: map[string]any{"question_id": q.ID, "seq": q.Seq}}); err != nil {
			return 0, err
		}
	}
	return len(list), nil
}
