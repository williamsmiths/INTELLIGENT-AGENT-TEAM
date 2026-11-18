package embed

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

const (
	defaultInterval = 5 * time.Second
	// Eight short items take about 0.5 s on the sidecar, so a search query that
	// arrives during a backfill usually still fits its deadline.
	defaultBatch = 8
	maxBackoff   = time.Minute
	maxHold      = time.Hour
)

// Worker embeds knowledge in the background (spec S2 B.1: "ghi không chờ
// embed"). Knowledge writes never call it; it finds its work in the database.
type Worker struct {
	Pool     *pgxpool.Pool
	Client   *Client
	Interval time.Duration                    // idle poll period; 0 = 5 s
	Batch    int                              // rows per step; 0 = 8
	Logf     func(format string, args ...any) // optional; receives ids and errors, never content

	mu   sync.Mutex
	size int              // current batch size: halved after a timeout, restored on success
	hold map[string]*held // rows the sidecar rejected, kept out of the queue for a while
}

type held struct {
	until time.Time
	delay time.Duration
}

type pendingRow struct{ id, hash, text string }

// Step embeds up to Batch current knowledge rows that have no embedding for the
// client's model, or one made from other content, and stores the vectors under
// store.Runtime. A row whose content hash is already embedded is never selected,
// so unchanged content is never embedded again (AC-S2-020). On a sidecar error
// nothing is stored and the rows stay pending (AC-S2-023).
func (w *Worker) Step(ctx context.Context) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.size == 0 {
		w.size = w.batch()
	}
	rows, err := w.pending(ctx, w.size)
	if err != nil || len(rows) == 0 {
		return 0, err
	}
	texts := make([]string, len(rows))
	for i, r := range rows {
		texts[i] = r.text
	}
	vecs, err := w.Client.Embed(ctx, texts)
	switch {
	case errors.Is(err, ErrRejected):
		rows, vecs, err = w.oneByOne(ctx, rows)
		if len(rows) == 0 {
			return 0, err
		}
	case errors.Is(err, context.DeadlineExceeded):
		w.size = max(1, w.size/2) // a smaller request may fit the timeout next time
		return 0, err
	case err != nil:
		return 0, err
	default:
		w.size = min(w.batch(), w.size*2)
	}
	if saveErr := w.save(ctx, rows, vecs); saveErr != nil {
		return 0, saveErr
	}
	return len(rows), err
}

func (w *Worker) batch() int {
	if w.Batch > 0 {
		return w.Batch
	}
	return defaultBatch
}

// pending lists the oldest rows that need an embedding, skipping rows on hold.
func (w *Worker) pending(ctx context.Context, limit int) ([]pendingRow, error) {
	skip := []string{}
	for id, h := range w.hold {
		if time.Now().Before(h.until) {
			skip = append(skip, id)
		}
	}
	var out []pendingRow
	err := store.InScope(ctx, w.Pool, store.Runtime, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT k.id::text, k.content_hash, k.title, k.body FROM knowledge k
			LEFT JOIN knowledge_embedding e ON e.knowledge_id = k.id AND e.model = $1
			WHERE k.is_current AND (e.knowledge_id IS NULL OR e.content_hash <> k.content_hash)
			  AND k.id <> ALL($2::uuid[])
			ORDER BY k.created_at, k.id LIMIT $3`, w.Client.Model, skip, limit)
		if err != nil {
			return err
		}
		out, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (pendingRow, error) {
			var r pendingRow
			var title, body string
			err := row.Scan(&r.id, &r.hash, &title, &body)
			r.text = ItemText(title, body)
			return r, err
		})
		return err
	})
	return out, err
}

func (w *Worker) logf(format string, args ...any) {
	if w.Logf != nil {
		w.Logf(format, args...)
	}
}

// Run calls Step until ctx ends: at once while there is a backlog, every
// Interval when idle, and with exponential backoff (up to one minute) while the
// sidecar or the database fails. A nil worker returns immediately (semantic off).
func (w *Worker) Run(ctx context.Context) {
	if w == nil {
		return
	}
	interval := w.Interval
	if interval <= 0 {
		interval = defaultInterval
	}
	failures := 0
	for ctx.Err() == nil {
		n, err := w.Step(ctx)
		wait := interval
		switch {
		case err != nil:
			if failures == 0 {
				w.logf("embed: worker paused, will retry: %v", err)
			}
			failures++
			wait = min(max(maxBackoff, interval), interval<<min(failures-1, 16))
		case n > 0:
			if failures > 0 {
				w.logf("embed: worker recovered")
			}
			failures, wait = 0, 0 // there may be more
		default:
			failures = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
