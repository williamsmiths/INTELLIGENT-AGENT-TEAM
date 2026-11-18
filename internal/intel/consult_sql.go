package intel

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

// ConsultRow is one consult (C.3).
type ConsultRow struct {
	ID         string       `json:"id"`
	ProjectID  string       `json:"project_id"`
	TaskID     string       `json:"task_id"`
	LeaseID    string       `json:"lease_id"`
	Asker      string       `json:"asker"`
	Answerer   string       `json:"answerer"`
	Specialty  string       `json:"specialty"`
	Question   string       `json:"question"`
	Hash       string       `json:"-"`
	Refs       []ConsultRef `json:"refs"`
	Status     string       `json:"status"`
	DeliveryID string       `json:"delivery_id,omitempty"`
	TurnID     string       `json:"turn_id,omitempty"`
	Answer     string       `json:"answer,omitempty"`
	AnswerRefs []ConsultRef `json:"answer_refs,omitempty"` // checked keys of the consult's project (REV-005 #6)
	FindingID  *int64       `json:"finding_id,omitempty"`
	ExpiresAt  time.Time    `json:"expires_at"`
	CreatedAt  time.Time    `json:"created_at"`
	Attempts   int          `json:"send_attempts"`
}

// Consult states (C.3): PENDING_SEND until the host took the message, OPEN
// until answered or past its deadline (EXPIRED); FAILED when never sent.
const (
	ConsultPending  = "PENDING_SEND"
	ConsultOpen     = "OPEN"
	ConsultAnswered = "ANSWERED"
	ConsultExpired  = "EXPIRED"
	ConsultFailed   = "FAILED"
)

const consultCols = `id, project_id, task_id, lease_id, asker, answerer, specialty, question, question_hash, refs, status,
	delivery_id, turn_id, COALESCE(answer, ''), answer_refs, finding_id, expires_at, created_at, send_attempts`

func scanConsult(row pgx.Row) (ConsultRow, error) {
	var c ConsultRow
	var refs, arefs []byte
	if err := row.Scan(&c.ID, &c.ProjectID, &c.TaskID, &c.LeaseID, &c.Asker, &c.Answerer, &c.Specialty, &c.Question, &c.Hash,
		&refs, &c.Status, &c.DeliveryID, &c.TurnID, &c.Answer, &arefs, &c.FindingID, &c.ExpiresAt, &c.CreatedAt, &c.Attempts); err != nil {
		return c, err
	}
	if err := json.Unmarshal(refs, &c.Refs); err != nil {
		return c, err
	}
	var err error
	c.AnswerRefs, err = decodeRefs(arefs)
	return c, err
}

func collectConsults(rows pgx.Rows, err error) ([]ConsultRow, error) {
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (ConsultRow, error) { return scanConsult(row) })
}

// FindDuplicate returns the earlier consult of the task with the same question hash.
func FindDuplicate(ctx context.Context, tx pgx.Tx, taskID, hash string) (*ConsultRow, error) {
	c, err := scanConsult(tx.QueryRow(ctx, `SELECT `+consultCols+` FROM consult WHERE task_id = $1 AND question_hash = $2
		AND status <> 'FAILED'`, taskID, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// ConsultBudget is the allowance of a task: the mode's allowance plus one
// extra consult once NONPROGRESS was journaled for the task (C.3, C.4), and
// how many consults it already used: a pending one counts, an expired one is
// not given back, a FAILED one (never sent) is (REV-005 #4).
func ConsultBudget(ctx context.Context, tx pgx.Tx, taskID, mode string) (allowed, used int, err error) {
	var bonus bool
	err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM consult WHERE task_id = $1 AND status <> 'FAILED'),
		EXISTS (SELECT 1 FROM journal WHERE task_id = $1 AND type = 'NONPROGRESS')`, taskID).Scan(&used, &bonus)
	allowed = ConsultAllowance(mode)
	if bonus {
		allowed += ConsultBonusLimit
	}
	return allowed, used, err
}

// Answerer picks who answers (C.3): a member of the same project with the
// specialty, not the asker, holding no live lease in another project; the one
// with the fewest OPEN consults, then the oldest principal. Runtime scope.
func Answerer(ctx context.Context, tx pgx.Tx, projectID, specialty, askerID string) (id, agentID, name string, err error) {
	err = tx.QueryRow(ctx, `SELECT p.id::text, COALESCE(p.openbot_agent_id, ''), p.name FROM principal p
		JOIN project_member m ON m.principal_id = p.id AND m.project_id = $1
		WHERE p.kind = 'agent' AND p.specialty = $2 AND p.id <> $3
		  AND NOT EXISTS (SELECT 1 FROM lease l WHERE l.principal_id = p.id AND l.state = 'ACTIVE'
		                   AND l.expires_at >= now() AND l.project_id <> $1)
		ORDER BY (SELECT count(*) FROM consult c WHERE c.answerer = p.id AND c.status IN ('PENDING_SEND','OPEN')), p.created_at, p.id
		LIMIT 1`, projectID, specialty, askerID).Scan(&id, &agentID, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", "", kerr.New(kerr.Conflict, "không có thành viên nào của project có chuyên môn %s và đang rảnh để trả lời", specialty)
	}
	return id, agentID, name, err
}

// InsertConsult stores a PENDING_SEND consult that expires after ConsultTTL;
// the worker's first retry is due after SendBackoff.
func InsertConsult(ctx context.Context, tx pgx.Tx, c ConsultRow) (ConsultRow, error) {
	refs, err := json.Marshal(orNone(c.Refs))
	if err != nil {
		return ConsultRow{}, err
	}
	return scanConsult(tx.QueryRow(ctx, `INSERT INTO consult (project_id, task_id, lease_id, asker, answerer, specialty, question,
			question_hash, refs, expires_at, status, next_send_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, now() + make_interval(secs => $10), 'PENDING_SEND',
			now() + make_interval(secs => $11)) RETURNING `+consultCols,
		c.ProjectID, c.TaskID, c.LeaseID, c.Asker, c.Answerer, c.Specialty, c.Question, c.Hash, refs, ConsultTTL.Seconds(), SendBackoff.Seconds()))
}

// GetConsult loads one consult; lock takes its row for the answer.
func GetConsult(ctx context.Context, tx pgx.Tx, id string, lock bool) (*ConsultRow, error) {
	if !isUUID(id) {
		return nil, nil
	}
	q := `SELECT ` + consultCols + ` FROM consult WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	c, err := scanConsult(tx.QueryRow(ctx, q, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// SetTurn records the host turn that handled the consult message (C.7).
func SetTurn(ctx context.Context, tx pgx.Tx, id, turnID string) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE consult SET turn_id = $2 WHERE id = $1 AND turn_id = ''`, id, turnID)
	return tag.RowsAffected() == 1, err
}

// SetAnswer records the answer of the selected answerer.
func SetAnswer(ctx context.Context, tx pgx.Tx, id, answer string, refs []ConsultRef, findingID int64) error {
	raw, err := json.Marshal(orNone(refs))
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE consult SET status = 'ANSWERED', answer = $2, answer_refs = $3, finding_id = $4, answered_at = now()
		WHERE id = $1 AND status = 'OPEN'`, id, answer, raw, findingID)
	return err
}

// ExpireDue ends the consults past their deadline and returns them (runtime
// scope): an OPEN one is EXPIRED, one never sent (PENDING_SEND) is FAILED.
func ExpireDue(ctx context.Context, tx pgx.Tx, id string) ([]ConsultRow, error) {
	return collectConsults(tx.Query(ctx, `UPDATE consult SET next_send_at = NULL,
			status = CASE WHEN status = 'OPEN' THEN 'EXPIRED' ELSE 'FAILED' END
		WHERE status IN ('OPEN','PENDING_SEND') AND expires_at < now()
		AND ($1 = '' OR id = nullif($1, '')::uuid) RETURNING `+consultCols, id))
}

// Unresolved lists consults whose message was delivered but whose host turn is not known yet.
func Unresolved(ctx context.Context, tx pgx.Tx) ([]ConsultRow, error) {
	return collectConsults(tx.Query(ctx, `SELECT `+consultCols+` FROM consult
		WHERE delivery_id <> '' AND turn_id = '' AND created_at > now() - interval '1 day' ORDER BY created_at LIMIT 100`))
}

// Outcomes returns the answered or expired consults of an asker's task that
// the asker was not told yet, and marks them told (C.3).
func Outcomes(ctx context.Context, tx pgx.Tx, askerID, taskID string) ([]ConsultRow, error) {
	return collectConsults(tx.Query(ctx, `UPDATE consult SET informed_at = now()
		WHERE asker = $1 AND task_id = $2 AND status IN ('ANSWERED','EXPIRED','FAILED') AND informed_at IS NULL
		RETURNING `+consultCols, askerID, taskID))
}
