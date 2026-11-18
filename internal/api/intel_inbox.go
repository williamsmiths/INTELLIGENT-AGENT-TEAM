package api

import (
	"encoding/json"
	"net/http"

	"github.com/jackc/pgx/v5"

	"github.com/williamsmiths/kongcowork-harness/internal/work"
)

// withInbox adds what waits for a FULL lease holder (consult outcomes, steer
// text; spec S2 C.3, C.4) to the "text" of any successful /v1/agent/* answer,
// so an agent that keeps typing kch commands learns it without a new prompt.
// The verify answer carries its own steer text, so the steer stays queued for
// the next hook there.
func (s *Server) withInbox(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &capture{header: w.Header(), status: http.StatusOK}
		next(rec, r)
		body := rec.body.Bytes()
		actor := principalOf(r)
		if rec.status < 300 && !actor.IsOwner() {
			var held *work.Held
			_ = s.runtime(r.Context(), func(tx pgx.Tx) (err error) {
				held, err = work.ActiveLease(r.Context(), tx, actor.ID)
				return err
			})
			if inbox := s.intelInbox(r.Context(), actor, held, r.PathValue("verb") != "verify"); inbox != "" {
				out := map[string]any{}
				if json.Unmarshal(body, &out) == nil {
					text, _ := out["text"].(string)
					out["text"] = text + "\n" + inbox
					if raw, err := json.Marshal(out); err == nil {
						body = append(raw, '\n')
					}
				}
			}
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(body)
	}
}
