package peer

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestWebSocketUpgradeReturnsNotImplemented(t *testing.T) {
	var called atomic.Bool
	p := newPair(t, pairConfig{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called.Store(true) })})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "/socket", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Connection", "keep-alive, Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Key", "not-forwarded")
	resp, err := p.httpClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501; body=%q", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "websocket upgrade is not supported") {
		t.Fatalf("unexpected body %q", body)
	}
	if called.Load() {
		t.Fatal("handler received websocket upgrade")
	}
}
