package openbot

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type call struct{ method, path, auth, contentType, body string }

// fakeHost is a stand-in for the host's automation API; the real one is never called.
type fakeHost struct {
	mu      sync.Mutex
	calls   []call
	answers []int // status of each request in order; 200 when exhausted
	delay   time.Duration
	*httptest.Server
}

func newFakeHost(t *testing.T, answers ...int) *fakeHost {
	h := &fakeHost{answers: answers}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.calls = append(h.calls, call{r.Method, r.URL.EscapedPath(), r.Header.Get("Authorization"), r.Header.Get("Content-Type"), string(body)})
		status := http.StatusOK
		if len(h.answers) > 0 {
			status, h.answers = h.answers[0], h.answers[1:]
		}
		delay := h.delay
		h.mu.Unlock()
		time.Sleep(delay)
		w.WriteHeader(status)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHost) seen() []call {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]call{}, h.calls...)
}

func endpointFiles(t *testing.T, baseURL, token string) (string, string) {
	dir := t.TempDir()
	urlFile, tokenFile := filepath.Join(dir, "url"), filepath.Join(dir, "token")
	for name, v := range map[string]string{urlFile: baseURL + "/\n", tokenFile: token + "\n"} {
		if err := os.WriteFile(name, []byte(v), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return urlFile, tokenFile
}

func TestNotifierSendsMessageAndClearsContext(t *testing.T) {
	host := newFakeHost(t)
	urlFile, tokenFile := endpointFiles(t, host.URL, "tok-1")
	n := NewNotifier(urlFile, tokenFile)
	ctx := context.Background()
	if _, err := n.Send(ctx, "agent-1", "Nút thêm phải nằm trong Part", "lease-2"); err != nil {
		t.Fatal(err)
	}
	if err := n.ClearContext(ctx, "agent/1"); err != nil {
		t.Fatal(err)
	}
	calls := host.seen()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want 2", len(calls))
	}
	msg := calls[0]
	var body map[string]string
	if err := json.Unmarshal([]byte(msg.body), &body); err != nil {
		t.Fatal(err)
	}
	if msg.method != "POST" || msg.path != "/v1/agents/agent-1/messages" || msg.auth != "Bearer tok-1" ||
		msg.contentType != "application/json" || body["text"] != "Nút thêm phải nằm trong Part" ||
		body["clientMessageId"] != "lease-2" || len(body) != 2 {
		t.Fatalf("message request = %+v", msg)
	}
	if c := calls[1]; c.method != "POST" || c.path != "/v1/agents/agent%2F1/context/clear" || c.auth != "Bearer tok-1" {
		t.Fatalf("clear request = %+v", c)
	}
	if _, err := n.Send(ctx, "", "x", ""); err == nil || n.ClearContext(ctx, "") == nil || len(host.seen()) != 2 {
		t.Fatal("a request without an agent id was sent")
	}
}

// The host writes a new URL and token at every start: both files are read at call time.
func TestNotifierReadsEndpointAtCallTime(t *testing.T) {
	first, second := newFakeHost(t), newFakeHost(t)
	urlFile, tokenFile := endpointFiles(t, first.URL, "tok-1")
	n := NewNotifier(urlFile, tokenFile)
	if _, err := n.Send(context.Background(), "a", "one", "1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urlFile, []byte(second.URL), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("tok-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(context.Background(), "a", "two", "2"); err != nil {
		t.Fatal(err)
	}
	if a, b := first.seen(), second.seen(); len(a) != 1 || len(b) != 1 || b[0].auth != "Bearer tok-2" {
		t.Fatalf("first host %+v, second host %+v", a, b)
	}
	// Missing or empty files are an error, not a request.
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(context.Background(), "a", "three", "3"); err == nil || len(second.seen()) != 1 {
		t.Fatalf("send without a token file: %v", err)
	}
	if err := os.WriteFile(tokenFile, []byte("tok-2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(urlFile, []byte("not a url"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Send(context.Background(), "a", "four", "4"); err == nil {
		t.Fatal("send with an unusable url file succeeded")
	}
}

// One retry at most, only for 5xx and transport errors; errors never carry the token.
func TestNotifierRetriesOnceAndReturnsErrors(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		answers  []int
		requests int
		ok       bool
	}{
		"500 then 200":      {[]int{500, 200}, 2, true},
		"503 twice":         {[]int{503, 503, 200}, 2, false},
		"404 is final":      {[]int{404, 200}, 1, false},
		"401 is final":      {[]int{401, 200}, 1, false},
		"redirect is final": {[]int{307, 200}, 1, false},
	} {
		host := newFakeHost(t, c.answers...)
		urlFile, tokenFile := endpointFiles(t, host.URL, "secret-token")
		_, err := NewNotifier(urlFile, tokenFile).Send(ctx, "a", "hello", "m-1")
		if (err == nil) != c.ok || len(host.seen()) != c.requests {
			t.Fatalf("%s: err %v after %d requests", name, err, len(host.seen()))
		}
		if err != nil && strings.Contains(err.Error(), "secret-token") {
			t.Fatalf("%s: error carries the token: %v", name, err)
		}
	}

	// A host that does not answer in time: each attempt times out, two attempts, an error.
	slow := newFakeHost(t)
	slow.delay = 300 * time.Millisecond
	urlFile, tokenFile := endpointFiles(t, slow.URL, "secret-token")
	n := NewNotifier(urlFile, tokenFile)
	n.Timeout = 50 * time.Millisecond
	started := time.Now()
	_, err := n.Send(ctx, "a", "hello", "m-2")
	if err == nil || time.Since(started) > 2*time.Second || strings.Contains(err.Error(), "secret-token") {
		t.Fatalf("slow host: err %v after %v", err, time.Since(started))
	}
	// A host that is gone: connection refused, retried once, returned.
	dead := newFakeHost(t)
	urlFile, tokenFile = endpointFiles(t, dead.URL, "secret-token")
	dead.Close()
	if err := NewNotifier(urlFile, tokenFile).ClearContext(ctx, "a"); err == nil {
		t.Fatal("clear on a dead host succeeded")
	}
	// A cancelled caller is not retried.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	host := newFakeHost(t, 500, 500)
	urlFile, tokenFile = endpointFiles(t, host.URL, "secret-token")
	if _, err := NewNotifier(urlFile, tokenFile).Send(cancelled, "a", "hello", "m-3"); err == nil || len(host.seen()) > 1 {
		t.Fatalf("cancelled send: err %v, %d requests", err, len(host.seen()))
	}
}
