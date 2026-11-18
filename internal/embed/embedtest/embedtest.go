// Package embedtest is a fake embedding sidecar for tests of harness logic
// (spec S2 part B). It speaks the sidecar's HTTP contract and returns
// deterministic vectors: texts that share a declared concept are close, all
// other texts are nearly orthogonal. It measures nothing about a real model.
package embedtest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/williamsmiths/kongcowork-harness/internal/embed"
)

// RejectMarker makes the fake refuse (400) any request with an input containing it.
const RejectMarker = "[[REJECT]]"

// Sidecar is one fake server. Its counters are what AC-S2-020 asserts on.
type Sidecar struct {
	URL string
	Dim int

	mu       sync.Mutex
	model    string
	requests int      // POST /v1/embeddings requests received
	texts    []string // every input received, in order
	infos    int      // GET /info requests received
	delay    time.Duration
	status   int // answer every request with this status when not 0
	concepts map[string]int
}

// New starts a fake sidecar that serves embed.DefaultModel with 1024 dimensions.
func New(t testing.TB) *Sidecar {
	s := &Sidecar{Dim: embed.DefaultDim, model: embed.DefaultModel, concepts: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)
	s.URL = srv.URL
	return s
}

// Client returns a client configured for this sidecar.
func (s *Sidecar) Client() *embed.Client {
	return &embed.Client{BaseURL: s.URL, Model: embed.DefaultModel, Dim: s.Dim}
}

// Concept declares words (matched case-insensitively as substrings) that mean
// the same thing: texts containing any of them get close vectors.
func (s *Sidecar) Concept(words ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	axis := len(s.concepts) // one axis per call is enough for tests
	for _, w := range words {
		s.concepts[strings.ToLower(w)] = axis % s.Dim
	}
}

func (s *Sidecar) SetDelay(d time.Duration) { s.set(func() { s.delay = d }) }
func (s *Sidecar) SetStatus(code int)       { s.set(func() { s.status = code }) }
func (s *Sidecar) SetModel(model string)    { s.set(func() { s.model = model }) }

func (s *Sidecar) set(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn()
}

// Requests is the number of embedding requests received so far.
func (s *Sidecar) Requests() int { s.mu.Lock(); defer s.mu.Unlock(); return s.requests }

// Infos is the number of GET /info requests received so far.
func (s *Sidecar) Infos() int { s.mu.Lock(); defer s.mu.Unlock(); return s.infos }

// Texts returns every input received so far.
func (s *Sidecar) Texts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.texts...)
}

// Vector is the deterministic unit vector of a text.
func (s *Sidecar) Vector(text string) []float32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := make([]float64, s.Dim)
	seed := sha256.Sum256([]byte(text))
	x := binary.LittleEndian.Uint64(seed[:8]) | 1
	for i := range v { // small pseudo-random noise so that no two texts are identical
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		v[i] = (float64(x%2001)/1000 - 1) * 0.02
	}
	lower := strings.ToLower(text)
	seen := map[int]bool{}
	for word, axis := range s.concepts {
		if strings.Contains(lower, word) && !seen[axis] {
			seen[axis] = true
			v[axis] += 1
		}
	}
	var sum float64
	for _, f := range v {
		sum += f * f
	}
	out := make([]float32, s.Dim)
	for i, f := range v {
		out[i] = float32(f / math.Sqrt(sum))
	}
	return out
}

func (s *Sidecar) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	delay, status, model := s.delay, s.status, s.model
	s.mu.Unlock()
	body, _ := io.ReadAll(r.Body) // read first: the server notices a client that gave up only afterwards
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done(): // the client gave up
			return
		}
	}
	if status != 0 {
		http.Error(w, `{"message":"fake failure"}`, status)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/health":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/info":
		s.set(func() { s.infos++ })
		_ = json.NewEncoder(w).Encode(map[string]any{"model_id": model})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/embeddings":
		var req struct{ Input []string }
		if err := json.Unmarshal(body, &req); err != nil || len(req.Input) == 0 {
			http.Error(w, `{"message":"bad request"}`, http.StatusBadRequest)
			return
		}
		if len(req.Input) > embed.MaxBatch { // like the real sidecar
			http.Error(w, `{"message":"batch too large"}`, http.StatusUnprocessableEntity)
			return
		}
		for _, in := range req.Input {
			if in == "" || strings.Contains(in, RejectMarker) { // "" like the real sidecar
				http.Error(w, `{"message":"input rejected"}`, http.StatusBadRequest)
				return
			}
		}
		s.set(func() { s.requests++; s.texts = append(s.texts, req.Input...) })
		data := make([]map[string]any, len(req.Input))
		for i, in := range req.Input {
			data[i] = map[string]any{"object": "embedding", "index": i, "embedding": s.Vector(in)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data, "model": model})
	default:
		http.NotFound(w, r)
	}
}
