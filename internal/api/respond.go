package api

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/williamsmiths/kongcowork-harness/internal/authn"
	"github.com/williamsmiths/kongcowork-harness/internal/kerr"
)

const maxBody = 1 << 20 // 1 MiB per request

// writeJSON sends a JSON response.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// writeErr maps service errors to the status codes of spec §3.
// Untyped errors are internal: logged, never echoed to the caller.
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	msg := "lỗi nội bộ"
	switch {
	case errors.Is(err, authn.ErrUnauthenticated):
		status, msg = http.StatusUnauthorized, "thiếu hoặc sai token"
	case kerr.Is(err, kerr.Forbidden):
		status, msg = http.StatusForbidden, err.Error()
	case kerr.Is(err, kerr.NotFound):
		status, msg = http.StatusNotFound, "không tìm thấy"
	case kerr.Is(err, kerr.Conflict):
		status, msg = http.StatusConflict, err.Error()
	case kerr.Is(err, kerr.Invalid):
		status, msg = http.StatusUnprocessableEntity, err.Error()
	case kerr.Is(err, kerr.TooMany):
		status, msg = http.StatusTooManyRequests, err.Error()
	default:
		slog.ErrorContext(r.Context(), "request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// readJSON decodes a bounded JSON body. An empty body leaves dst untouched.
func readJSON(r *http.Request, dst any) error {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		return kerr.New(kerr.Invalid, "không đọc được body")
	}
	if len(raw) > maxBody {
		return kerr.New(kerr.Invalid, "body quá lớn")
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return kerr.New(kerr.Invalid, "JSON không hợp lệ")
	}
	return nil
}
