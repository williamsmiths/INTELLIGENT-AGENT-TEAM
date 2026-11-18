// Package embed is the client and the background queue of the local embedding
// sidecar (spec S2 B.1): an OpenAI-style POST /v1/embeddings on loopback,
// declared in infra/compose.yaml. Nothing here calls a service outside the machine.
package embed

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"
)

// Defaults chosen by evals/retrieval (README.md, report.json).
const (
	DefaultModel   = "BAAI/bge-m3"
	DefaultDim     = 1024
	DefaultMinSim  = 0.51 // provisional cosine threshold
	MaxBatch       = 32   // the sidecar refuses more inputs per request (422)
	DefaultTimeout = 60 * time.Second

	maxResponseBytes = 32 << 20
)

var (
	// ErrUnavailable: the sidecar is not configured, unreachable, too slow or
	// answers 5xx. Callers fall back to the lexical stages (AC-S2-023).
	ErrUnavailable = errors.New("embed: sidecar unavailable")
	// ErrModelMismatch: the sidecar serves another model than the configured one.
	// The sidecar does not validate the request's model field, so a vector stored
	// under the wrong model name would silently corrupt retrieval.
	ErrModelMismatch = errors.New("embed: sidecar serves another model")
	// ErrRejected: the sidecar refused this input (4xx).
	ErrRejected = errors.New("embed: sidecar rejected the input")
	// ErrBadResponse: wrong count, dimension or values in the answer.
	ErrBadResponse = errors.New("embed: malformed sidecar response")
)

// Client talks to one sidecar. Use it by pointer; the zero value of the
// unexported fields is ready.
type Client struct {
	BaseURL   string        // e.g. http://127.0.0.1:8870
	Model     string        // must equal the sidecar's model id
	Dim       int           // expected vector length
	Timeout   time.Duration // bound of one HTTP request; 0 = DefaultTimeout
	HTTP      *http.Client  // nil = http.DefaultClient
	CacheSize int           // query-embedding cache entries; 0 = 1024

	mu       sync.Mutex
	verified bool // /info reported Model; reset after any transport error
	cache    *lru
}

// Embed returns one L2-normalised vector per text, in order. Requests carry at
// most MaxBatch texts and are bounded by Timeout and by ctx.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if c == nil || c.BaseURL == "" {
		return nil, fmt.Errorf("%w: not configured", ErrUnavailable)
	}
	if err := c.verifyModel(ctx); err != nil {
		return nil, err
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += MaxBatch {
		vecs, err := c.embedBatch(ctx, texts[start:min(start+MaxBatch, len(texts))])
		if err != nil {
			return nil, err
		}
		out = append(out, vecs...)
	}
	return out, nil
}

func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	clipped := make([]string, len(texts))
	for i, t := range texts {
		clipped[i] = Clip(t)
	}
	var resp struct {
		Model string `json:"model"`
		Data  []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	req := map[string]any{"model": c.Model, "input": clipped}
	if err := c.do(ctx, http.MethodPost, "/v1/embeddings", req, &resp); err != nil {
		return nil, err
	}
	// The answer names the model that really ran (the request field is ignored).
	if resp.Model != "" && resp.Model != c.Model {
		c.setVerified(false)
		return nil, fmt.Errorf("%w: answer from %q, configured %q", ErrModelMismatch, resp.Model, c.Model)
	}
	if len(resp.Data) != len(texts) {
		return nil, fmt.Errorf("%w: %d vectors for %d texts", ErrBadResponse, len(resp.Data), len(texts))
	}
	out := make([][]float32, len(texts))
	for _, d := range resp.Data {
		if d.Index < 0 || d.Index >= len(out) || out[d.Index] != nil {
			return nil, fmt.Errorf("%w: index %d", ErrBadResponse, d.Index)
		}
		if len(d.Embedding) != c.Dim {
			return nil, fmt.Errorf("%w: dimension %d, configured %d", ErrBadResponse, len(d.Embedding), c.Dim)
		}
		if !normalize(d.Embedding) {
			return nil, fmt.Errorf("%w: vector %d is zero or not finite", ErrBadResponse, d.Index)
		}
		out[d.Index] = d.Embedding
	}
	return out, nil
}

// verifyModel asks GET /info once and caches a match. A mismatch is not
// cached, so a sidecar restarted with the right model recovers by itself.
func (c *Client) verifyModel(ctx context.Context) error {
	c.mu.Lock()
	ok := c.verified
	c.mu.Unlock()
	if ok {
		return nil
	}
	var info struct {
		ModelID string `json:"model_id"`
	}
	if err := c.do(ctx, http.MethodGet, "/info", nil, &info); err != nil {
		if errors.Is(err, ErrUnavailable) {
			return err
		}
		return fmt.Errorf("%w: cannot read /info: %v", ErrModelMismatch, err)
	}
	if info.ModelID != c.Model {
		return fmt.Errorf("%w: sidecar has %q, configured %q", ErrModelMismatch, info.ModelID, c.Model)
	}
	c.setVerified(true)
	return nil
}

func (c *Client) setVerified(v bool) {
	c.mu.Lock()
	c.verified = v
	c.mu.Unlock()
}
