package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
	"github.com/williamsmiths/kongcowork-harness/internal/store"
)

// Row states of the idempotency table.
const (
	idemRunning = 0  // first request still in flight
	idemFailed  = -1 // first request failed with 5xx; the key may be retried
)

// idemStale is how long a row may stay "in flight" before another request
// with the same key takes it over: the process that claimed it is gone
// (spec S2 D.1, AC-S2-054). No handler runs that long — the longest work
// behind this middleware, a verification, is not replayed from a stale row.
const idemStale = 2 * time.Minute

// idempotent implements the Idempotency-Key contract of spec §3: the key is
// scoped to (principal, method + full path); the same body replays the stored
// answer, another body is 422. Requests without the header pass through.
func idempotent(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get("Idempotency-Key")
			// A response that carries a secret is never stored for replay: creating
			// a principal returns its token once (PM-H-02, REV-003 #7).
			if key == "" || (r.Method != http.MethodPost && r.Method != http.MethodPut) || r.URL.Path == "/v1/principals" {
				next.ServeHTTP(w, r)
				return
			}
			raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
			if err != nil || len(raw) > maxBody {
				writeErr(w, r, kerr.New(kerr.Invalid, "body không hợp lệ"))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(raw))
			sum := sha256.Sum256(raw)
			bodyHash := hex.EncodeToString(sum[:])
			p := principalOf(r)
			route := r.Method + " " + r.URL.Path

			var claimed bool
			var status int
			var stored []byte
			var storedHash string
			err = store.InScope(r.Context(), pool, p.Scope(), func(tx pgx.Tx) error {
				tag, err := tx.Exec(r.Context(),
					`INSERT INTO idempotency (principal_id, route, key, body_hash, status, body)
					 VALUES ($1,$2,$3,$4,$5,'null') ON CONFLICT DO NOTHING`, p.ID, route, key, bodyHash, idemRunning)
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 1 {
					claimed = true
					return nil
				}
				// Take over a key whose first attempt failed, or whose first attempt has
				// been "in flight" for longer than idemStale (AC-S2-054); created_at
				// restarts so the takeover gets its own two minutes. Else read the stored answer.
				tag, err = tx.Exec(r.Context(),
					`UPDATE idempotency SET status = $5, body_hash = $4, body = 'null', created_at = now()
					 WHERE principal_id = $1 AND route = $2 AND key = $3
					   AND (status = $6 OR (status = $5 AND created_at < now() - make_interval(secs => $7)))`,
					p.ID, route, key, bodyHash, idemRunning, idemFailed, idemStale.Seconds())
				if err != nil {
					return err
				}
				if tag.RowsAffected() == 1 {
					claimed = true
					return nil
				}
				return tx.QueryRow(r.Context(),
					`SELECT status, body, body_hash FROM idempotency WHERE principal_id = $1 AND route = $2 AND key = $3`,
					p.ID, route, key).Scan(&status, &stored, &storedHash)
			})
			switch {
			case err != nil:
				writeErr(w, r, err)
				return
			case !claimed && storedHash != bodyHash:
				writeErr(w, r, kerr.New(kerr.Invalid, "Idempotency-Key đã dùng với nội dung khác"))
				return
			case !claimed && status == idemRunning:
				writeErr(w, r, kerr.New(kerr.Conflict, "yêu cầu cùng Idempotency-Key đang được xử lý"))
				return
			case !claimed:
				w.Header().Set("Idempotent-Replay", "true")
				w.Header().Set("Content-Type", "application/json; charset=utf-8")
				// The first answer is replayed as 200 when it created something (spec AC-H-006, AC-H-013).
				if status == http.StatusCreated {
					status = http.StatusOK
				}
				w.WriteHeader(status)
				_, _ = w.Write(stored)
				return
			}

			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			final, body := rec.status, rec.buf.Bytes()
			if final >= 500 {
				final, body = idemFailed, []byte("null")
			}
			if !json.Valid(body) {
				body = []byte("null")
			}
			// The outcome is stored even if the client went away, or the key would stay "in flight" forever.
			done := context.WithoutCancel(r.Context())
			save := func(body []byte) error {
				return store.InScope(done, pool, p.Scope(), func(tx pgx.Tx) error {
					_, err := tx.Exec(done,
						`UPDATE idempotency SET status = $4, body = $5 WHERE principal_id = $1 AND route = $2 AND key = $3`,
						p.ID, route, key, final, body)
					return err
				})
			}
			err = save(body)
			if err != nil {
				// A body PostgreSQL cannot keep as jsonb (a \u0000 escape, for one) must not
				// wedge the key: the real status is stored with a null body (AC-S2-054).
				slog.WarnContext(done, "idempotency: answer not storable, keeping the status only", "route", route, "err", err)
				err = save([]byte("null"))
			}
			if err != nil { // the row stays "in flight" and is taken over after idemStale
				slog.ErrorContext(done, "idempotency: outcome not stored", "route", route, "err", err)
			}
		})
	}
}

// recorder copies the response so it can be stored for replay.
type recorder struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	r.buf.Write(b)
	return r.ResponseWriter.Write(b)
}
