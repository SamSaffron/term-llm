package llm

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestResponsesWebSocketPoolConcurrentLifecycle(t *testing.T) {
	pool := newResponsesWebSocketPool(8)
	const (
		workers    = 32
		iterations = 200
	)
	var wg sync.WaitGroup
	for worker := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprintf("credential-%d@endpoint", worker%2)
			for range iterations {
				lease, err := pool.acquire(key)
				if errors.Is(err, errResponsesWebSocketPoolSaturated) {
					continue
				}
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				lease.park()
				if lease.activate() {
					lease.park()
				}
				lease.release()
			}
		}()
	}
	wg.Wait()
	for _, key := range []string{"credential-0@endpoint", "credential-1@endpoint"} {
		if got := pool.count(key); got != 0 {
			t.Fatalf("pool count for %s = %d after concurrent lifecycle, want 0", key, got)
		}
	}
}

func TestResponsesWebSocketPoolReleaseClearsLeaseState(t *testing.T) {
	pool := newResponsesWebSocketPool(1)
	lease, err := pool.acquire("credential@endpoint")
	if err != nil {
		t.Fatal(err)
	}
	lease.release()
	admitted, active, count := lease.diagnosticSnapshot()
	if admitted || active || count != 0 {
		t.Fatalf("released lease snapshot = admitted:%t active:%t count:%d, want false/false/0", admitted, active, count)
	}
}

func TestResponsesWebSocketPoolRejectsWhenAllConnectionsAreActive(t *testing.T) {
	pool := newResponsesWebSocketPool(2)
	first, err := pool.acquire("credential@endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()
	second, err := pool.acquire("credential@endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer second.release()

	if _, err := pool.acquire("credential@endpoint"); !errors.Is(err, errResponsesWebSocketPoolSaturated) {
		t.Fatalf("third acquire error = %v, want pool saturation", err)
	}
	if got := pool.count("credential@endpoint"); got != 2 {
		t.Fatalf("pool count = %d, want 2", got)
	}
}

func TestResponsesWebSocketPoolEvictsLeastRecentlyParkedConnection(t *testing.T) {
	pool := newResponsesWebSocketPool(2)
	oldest, err := pool.acquire("credential@endpoint")
	if err != nil {
		t.Fatal(err)
	}
	newest, err := pool.acquire("credential@endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer newest.release()

	pool.mu.Lock()
	oldest.active = false
	oldest.parkedAt = time.Unix(1, 0)
	newest.active = false
	newest.parkedAt = time.Unix(2, 0)
	pool.mu.Unlock()

	replacement, err := pool.acquire("credential@endpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.release()
	if oldest.activate() {
		t.Fatal("oldest parked lease remained admitted after LRU eviction")
	}
	if !newest.activate() {
		t.Fatal("newer parked lease was evicted instead of oldest")
	}
	if got := pool.count("credential@endpoint"); got != 2 {
		t.Fatalf("pool count = %d, want 2", got)
	}
}

func TestResponsesClientWebSocketToolContinuationCannotBeEvicted(t *testing.T) {
	key := "pinned-tool-" + t.Name()
	var handshakes atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		handshakes.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file"}})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{}"}})
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_tool"}})
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_final"}})
	}))
	defer server.Close()

	client := &ResponsesClient{BaseURL: server.URL, UseWebSocket: true, WebSocketServerState: true, DisableServerState: true, WebSocketPoolKey: key}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", SessionID: "session-x", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "first"}}, Stream: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, first)
	_ = first.Close()

	fillers := make([]*responsesWebSocketLease, 0, defaultResponsesWebSocketMaxConnectionsPerKey-1)
	for range defaultResponsesWebSocketMaxConnectionsPerKey - 1 {
		lease, acquireErr := sharedResponsesWebSocketPool.acquire(key)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		fillers = append(fillers, lease)
	}
	defer func() {
		for _, lease := range fillers {
			lease.release()
		}
	}()
	if _, err := sharedResponsesWebSocketPool.acquire(key); !errors.Is(err, errResponsesWebSocketPoolSaturated) {
		t.Fatalf("acquire at capacity = %v, want saturation rather than continuation eviction", err)
	}

	second, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", SessionID: "session-x", Input: []ResponsesInputItem{
		{Type: "message", Role: "user", Content: "first"},
		{Type: "function_call", CallID: "call_1", Name: "read_file", Arguments: "{}"},
		{Type: "function_call_output", CallID: "call_1", Output: "contents"},
	}, Stream: true}, false)
	if err != nil {
		t.Fatal(err)
	}
	drainStreamToDone(t, second)
	_ = second.Close()
	if handshakes.Load() != 1 {
		t.Fatalf("handshakes = %d, want pinned exact socket", handshakes.Load())
	}
}

func TestResponsesClientNeverStartedWebSocketReleasesPoolLease(t *testing.T) {
	const parkedTimeout = 30 * time.Millisecond

	key := "never-started-" + t.Name()
	closed := make(chan struct{})
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		close(closed)
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:                server.URL,
		WebSocketPoolKey:       key,
		WebSocketParkedTimeout: parkedTimeout,
	}
	client.wsMu.Lock()
	_, _, err := client.ensureWebSocket(context.Background(), ResponsesRequest{SessionID: "never-started"})
	client.wsMu.Unlock()
	if err != nil {
		t.Fatalf("ensureWebSocket: %v", err)
	}
	if got := sharedResponsesWebSocketPool.count(key); got != 1 {
		t.Fatalf("pool count after dial = %d, want 1", got)
	}
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("never-started WebSocket was not closed by bounded handoff timer")
	}
	if got := sharedResponsesWebSocketPool.count(key); got != 0 {
		t.Fatalf("pool count after never-started timeout = %d, want 0", got)
	}
}

func TestResponsesClientWebSocketPoolSaturationDuringRetrySkipsRemainingRetries(t *testing.T) {
	withResponsesWebSocketBaseBackoff(t, 0)
	key := "retry-saturation-" + t.Name()
	var wsAttempts atomic.Int32
	var httpRequests atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			wsAttempts.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_, _, _ = conn.ReadMessage()
			_ = conn.Close()
			return
		}
		httpRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n")
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:          server.URL,
		HTTPClient:       server.Client(),
		UseWebSocket:     true,
		WebSocketPoolKey: key,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := client.Stream(ctx, ResponsesRequest{Model: "gpt-test", Input: []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}}, Stream: true}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	first, err := stream.Recv()
	if err != nil {
		t.Fatalf("first Recv: %v", err)
	}
	if first.Type != EventRetry {
		t.Fatalf("first event = %v, want retry after initial transport failure", first.Type)
	}

	leases := make([]*responsesWebSocketLease, 0, defaultResponsesWebSocketMaxConnectionsPerKey)
	for range defaultResponsesWebSocketMaxConnectionsPerKey {
		lease, acquireErr := sharedResponsesWebSocketPool.acquire(key)
		if acquireErr != nil {
			t.Fatalf("reserve retry pool slot: %v", acquireErr)
		}
		leases = append(leases, lease)
	}
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()

	retries := 1
	for {
		event, recvErr := stream.Recv()
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if event.Type == EventRetry {
			retries++
		}
		if event.Type == EventDone {
			break
		}
		if event.Type == EventError {
			t.Fatalf("stream error: %v", event.Err)
		}
	}
	if retries != 1 {
		t.Fatalf("retry events = %d, want only the pre-saturation retry", retries)
	}
	if got := wsAttempts.Load(); got != 1 {
		t.Fatalf("WebSocket attempts = %d, want no retry while saturated", got)
	}
	if got := httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestResponsesClientWebSocketPoolSaturationFallsBackImmediatelyToHTTP(t *testing.T) {
	key := "pool-saturation-" + t.Name()
	leases := make([]*responsesWebSocketLease, 0, defaultResponsesWebSocketMaxConnectionsPerKey)
	for range defaultResponsesWebSocketMaxConnectionsPerKey {
		lease, err := sharedResponsesWebSocketPool.acquire(key)
		if err != nil {
			t.Fatalf("reserve pool slot: %v", err)
		}
		leases = append(leases, lease)
	}
	defer func() {
		for _, lease := range leases {
			lease.release()
		}
	}()

	var upgrades atomic.Int32
	var httpRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "" {
			upgrades.Add(1)
			http.Error(w, "unexpected WebSocket attempt", http.StatusBadRequest)
			return
		}
		httpRequests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_http\"}}\n\n")
	}))
	defer server.Close()

	client := &ResponsesClient{
		BaseURL:          server.URL,
		HTTPClient:       server.Client(),
		UseWebSocket:     true,
		WebSocketPoolKey: key,
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := client.Stream(ctx, ResponsesRequest{
		Model:  "gpt-test",
		Input:  []ResponsesInputItem{{Type: "message", Role: "user", Content: "hi"}},
		Stream: true,
	}, false)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	drainStreamToDone(t, stream)

	if got := upgrades.Load(); got != 0 {
		t.Fatalf("WebSocket upgrade attempts = %d, want 0 while pool is saturated", got)
	}
	if got := httpRequests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}
