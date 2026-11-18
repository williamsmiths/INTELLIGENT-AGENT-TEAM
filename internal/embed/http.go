package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// do sends one bounded JSON request. Transport errors, timeouts and 5xx are
// ErrUnavailable (and drop the cached model check: the sidecar may have been
// restarted with another model); other 4xx are ErrRejected.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrRejected, err)
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.BaseURL, "/")+path, body)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = http.DefaultClient
	}
	resp, err := hc.Do(req)
	if err != nil {
		c.setVerified(false)
		return fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		c.setVerified(false)
		return fmt.Errorf("%w: read %s: %w", ErrUnavailable, path, err)
	}
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusRequestTimeout:
		c.setVerified(false)
		return fmt.Errorf("%w: %s %s: HTTP %d", ErrUnavailable, method, path, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return fmt.Errorf("%w: %s %s: HTTP %d: %s", ErrRejected, method, path, resp.StatusCode, Clip(string(raw[:min(len(raw), 200)])))
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrBadResponse, path, err)
	}
	return nil
}
