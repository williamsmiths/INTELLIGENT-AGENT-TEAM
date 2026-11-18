package openbot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Spec S2 C.7: a consult message returns the host's delivery id, and the
// delivery names the turn that handled it, so exactly that turn's usage goes
// to the asker. Shapes as observed on the live host: POST messages answers
// {"messageId","deliveries":[{"id",…}]}; GET deliveries/{id} answers {"id","status","turnId","error","answer"}.
func TestNotifierDeliveryIDAndTurn_AC_S2_037(t *testing.T) {
	var seen []string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath()+" "+r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.EscapedPath() {
		case "/v1/agents/agent-1/messages":
			_, _ = w.Write([]byte(`{"messageId":"msg-1","deliveries":[{"id":"dlv-7","recipientAgentId":"agent-1","status":"queued","position":1}]}`))
		case "/v1/agents/agent-1/deliveries/dlv-7":
			_, _ = w.Write([]byte(`{"id":"dlv-7","status":"delivered","turnId":"turn-42","error":null,"answer":null}`))
		case "/v1/agents/agent-1/deliveries/dlv-8":
			_, _ = w.Write([]byte(`{"id":"dlv-8","status":"queued","turnId":""}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(host.Close)
	urlFile, tokenFile := endpointFiles(t, host.URL, "tok-9")
	n := NewNotifier(urlFile, tokenFile)
	ctx := context.Background()
	id, err := n.Send(ctx, "agent-1", "câu hỏi tham vấn", "kch-consult-1")
	if err != nil || id != "dlv-7" {
		t.Fatalf("send: id %q, err %v", id, err)
	}
	status, turn, err := n.Delivery(ctx, "agent-1", id)
	if err != nil || status != "delivered" || turn != "turn-42" {
		t.Fatalf("delivery: %q %q %v", status, turn, err)
	}
	if status, turn, err = n.Delivery(ctx, "agent-1", "dlv-8"); err != nil || status != "queued" || turn != "" {
		t.Fatalf("pending delivery: %q %q %v", status, turn, err)
	}
	if _, _, err := n.Delivery(ctx, "agent-1", "missing"); err == nil {
		t.Fatal("an unknown delivery is not an error")
	}
	if _, _, err := n.Delivery(ctx, "agent-1", ""); err == nil || len(seen) != 4 {
		t.Fatalf("a delivery without id was requested: %v %v", err, seen)
	}
	if seen[1] != "GET /v1/agents/agent-1/deliveries/dlv-7 Bearer tok-9" {
		t.Fatalf("delivery request: %v", seen)
	}
}
