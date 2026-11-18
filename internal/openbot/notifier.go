package openbot

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Notifier asks the host to deliver a message to an agent, or to clear an
// agent's session context, through the host's local automation API. Delivery
// is best effort: the caller journals NOTIFY_SENT or NOTIFY_FAILED and goes on
// (T-H-02, T-H-22, AC-H-056).
type Notifier struct {
	URLFile   string        // file holding the base URL of the automation API
	TokenFile string        // file holding its bearer token
	Timeout   time.Duration // per attempt; 10 s when zero
	client    *http.Client
}

// NewNotifier reads the endpoint from the two files at every call, because the
// host writes a new URL and token each time it starts. Empty names mean
// ~/.config/OpenBot/automation/url and .../token.
func NewNotifier(urlFile, tokenFile string) *Notifier {
	if home, err := os.UserHomeDir(); err == nil {
		dir := filepath.Join(home, ".config", "OpenBot", "automation")
		if urlFile == "" {
			urlFile = filepath.Join(dir, "url")
		}
		if tokenFile == "" {
			tokenFile = filepath.Join(dir, "token")
		}
	}
	// The token goes to the configured host only: redirects are not followed.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return &Notifier{URLFile: urlFile, TokenFile: tokenFile, client: client}
}

// Send posts a message to the agent's mailbox and returns the host's delivery
// id ("" when the host did not name one). clientMessageID lets the host drop a
// repeated delivery of the same message.
func (n *Notifier) Send(ctx context.Context, agentID, text, clientMessageID string) (string, error) {
	if agentID == "" || text == "" {
		return "", fmt.Errorf("openbot automation: agent id and text are required")
	}
	body, err := n.call(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(agentID)+"/messages",
		map[string]string{"text": text, "clientMessageId": clientMessageID})
	if err != nil {
		return "", err
	}
	// The host answers 202 {"messageId": …, "deliveries": [{"id": …, "recipientAgentId": …, "status": …}]}
	// (observed on the live automation API, 2026-11-17); the delivery to agentID is what we track.
	var out struct {
		Deliveries []struct {
			ID          string `json:"id"`
			RecipientID string `json:"recipientAgentId"`
		} `json:"deliveries"`
	}
	_ = json.Unmarshal(body, &out) // a host that answers without a body still delivered the message
	for _, d := range out.Deliveries {
		if d.RecipientID == agentID || d.RecipientID == "" {
			return d.ID, nil
		}
	}
	return "", nil
}

// Delivery reads the state of one delivery: its status and the id of the host
// turn that handled the message ("" until a turn picked it up). Spec S2 C.7
// attributes the usage of exactly that turn to the consult that sent it.
func (n *Notifier) Delivery(ctx context.Context, agentID, deliveryID string) (status, turnID string, err error) {
	if agentID == "" || deliveryID == "" {
		return "", "", fmt.Errorf("openbot automation: agent id and delivery id are required")
	}
	body, err := n.call(ctx, http.MethodGet, "/v1/agents/"+url.PathEscape(agentID)+"/deliveries/"+url.PathEscape(deliveryID), nil)
	if err != nil {
		return "", "", err
	}
	var out struct {
		ID     string `json:"id"`
		Status string `json:"status"`
		TurnID string `json:"turnId"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", "", fmt.Errorf("openbot automation: delivery answer is not JSON")
	}
	return out.Status, out.TurnID, nil
}

// ClearContext asks the host to clear the agent's session context, used when
// an agent moves to a task of another project (§0, limitation L8).
func (n *Notifier) ClearContext(ctx context.Context, agentID string) error {
	if agentID == "" {
		return fmt.Errorf("openbot automation: agent id is required")
	}
	_, err := n.call(ctx, http.MethodPost, "/v1/agents/"+url.PathEscape(agentID)+"/context/clear", map[string]string{})
	return err
}

// call sends one request and retries at most once, on a transport error or a
// 5xx answer. The endpoint files are read again for the retry, which covers a
// host that restarted in between. A 4xx answer is final. A nil body sends none.
func (n *Notifier) call(ctx context.Context, method, path string, body any) ([]byte, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	for attempt := 1; ; attempt++ {
		var status int
		var answer []byte
		base, token, err := n.endpoint()
		if err != nil {
			return nil, err // nothing to retry: the host is not configured
		}
		if status, answer, err = n.do(ctx, method, base+path, token, payload); err == nil {
			if status >= 200 && status < 300 {
				return answer, nil
			}
			err = fmt.Errorf("openbot automation: %s %s: HTTP %d", method, path, status)
		}
		if attempt == 2 || (status >= 300 && status < 500) || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func (n *Notifier) do(ctx context.Context, method, target, token string, payload []byte) (int, []byte, error) {
	timeout := n.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, rd)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := n.client.Do(req)
	if err != nil {
		// A *url.Error names the URL only; the token is in a header and never in an error.
		return 0, nil, fmt.Errorf("openbot automation: %w", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, nil, fmt.Errorf("openbot automation: reading the answer: %w", err)
	}
	return resp.StatusCode, answer, nil
}

// endpoint reads the base URL and token the host wrote for this run.
func (n *Notifier) endpoint() (base, token string, err error) {
	rawURL, err := os.ReadFile(n.URLFile)
	if err != nil {
		return "", "", fmt.Errorf("openbot automation url: %w", err)
	}
	rawToken, err := os.ReadFile(n.TokenFile)
	if err != nil {
		return "", "", fmt.Errorf("openbot automation token: %w", err)
	}
	base = strings.TrimRight(strings.TrimSpace(string(rawURL)), "/")
	token = strings.TrimSpace(string(rawToken))
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || token == "" {
		return "", "", fmt.Errorf("openbot automation: url file or token file has no usable value")
	}
	return base, token, nil
}
