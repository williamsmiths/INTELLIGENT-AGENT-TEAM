package intel

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5"
)

// Sending a consult message (C.3 after REV-005 #4): the request that opens a
// consult tries once within a short deadline; the minute worker retries a
// PENDING_SEND consult with a doubling backoff until its deadline, when it
// becomes FAILED and leaves the task's allowance.

// MarkSent records that the host took the consult message: OPEN with its delivery id.
func MarkSent(ctx context.Context, tx pgx.Tx, id, deliveryID string) error {
	_, err := tx.Exec(ctx, `UPDATE consult SET status = 'OPEN', delivery_id = $2, send_attempts = send_attempts + 1, next_send_at = NULL
		WHERE id = $1 AND status = 'PENDING_SEND'`, id, deliveryID)
	return err
}

// MarkSendFailed counts a failed send and schedules the next one: SendBackoff
// doubled at each attempt (1, 2, 4, 8 minutes), never after the deadline.
func MarkSendFailed(ctx context.Context, tx pgx.Tx, id string) error {
	_, err := tx.Exec(ctx, `UPDATE consult SET send_attempts = send_attempts + 1,
			next_send_at = least(now() + make_interval(secs => $2 * power(2, least(send_attempts, 6))), expires_at)
		WHERE id = $1 AND status = 'PENDING_SEND'`, id, SendBackoff.Seconds())
	return err
}

// DueSends lists the PENDING_SEND consults whose next send is due (worker).
func DueSends(ctx context.Context, tx pgx.Tx) ([]ConsultRow, error) {
	return collectConsults(tx.Query(ctx, `SELECT `+consultCols+` FROM consult
		WHERE status = 'PENDING_SEND' AND next_send_at <= now() AND expires_at >= now() ORDER BY next_send_at LIMIT 50`))
}

// decodeRefs reads answer_refs: reference objects, or the bare keys rows
// written before REV-005 #6 hold (shown as a key without title or trust).
func decodeRefs(raw []byte) ([]ConsultRef, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, err
	}
	out := make([]ConsultRef, 0, len(items))
	for _, it := range items {
		var r ConsultRef
		if err := json.Unmarshal(it, &r); err != nil {
			var key string
			if json.Unmarshal(it, &key) != nil {
				return nil, err
			}
			r.Key = key
		}
		out = append(out, r)
	}
	return out, nil
}
