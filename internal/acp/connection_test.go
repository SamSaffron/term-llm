package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type testHandler struct {
	mu            sync.Mutex
	notifications []string
	requests      []string
}

func (h *testHandler) HandleNotification(_ context.Context, method string, _ json.RawMessage) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.notifications = append(h.notifications, method)
}

func (h *testHandler) HandleRequest(_ context.Context, method string, _ json.RawMessage) (any, *RPCError) {
	h.mu.Lock()
	h.requests = append(h.requests, method)
	h.mu.Unlock()
	if method == "client/ping" {
		return map[string]string{"pong": "yes"}, nil
	}
	return nil, MethodNotFound(method)
}

func TestConnectionHandlesBidirectionalTrafficAndOutOfOrderResponses(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()

	handler := &testHandler{}
	conn := NewConnection(clientSide, clientSide, handler, Options{})

	agentErr := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(agentSide)
		requests := make(map[string]wireEnvelope)
		for len(requests) < 2 {
			line, err := reader.ReadBytes('\n')
			if err != nil {
				agentErr <- err
				return
			}
			var msg wireEnvelope
			if err := json.Unmarshal(line, &msg); err != nil {
				agentErr <- err
				return
			}
			requests[msg.Method] = msg
		}

		if _, err := fmt.Fprintf(agentSide, `{"jsonrpc":"2.0","id":"agent-1","method":"client/ping","params":{}}`+"\n"); err != nil {
			agentErr <- err
			return
		}
		line, err := reader.ReadBytes('\n')
		if err != nil {
			agentErr <- err
			return
		}
		var callback wireEnvelope
		if err := json.Unmarshal(line, &callback); err != nil {
			agentErr <- err
			return
		}
		if string(callback.ID) != `"agent-1"` || !strings.Contains(string(callback.Result), `"pong":"yes"`) {
			agentErr <- fmt.Errorf("callback response = %s", line)
			return
		}

		for _, method := range []string{"second", "first"} {
			msg := requests[method]
			if _, err := fmt.Fprintf(agentSide, `{"jsonrpc":"2.0","id":%s,"result":{"method":%q}}`+"\n", msg.ID, method); err != nil {
				agentErr <- err
				return
			}
		}
		agentErr <- nil
	}()

	type result struct {
		Method string `json:"method"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var first, second result
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := conn.Call(ctx, "first", map[string]any{}, &first); err != nil {
			t.Error(err)
		}
	}()
	go func() {
		defer wg.Done()
		if err := conn.Call(ctx, "second", map[string]any{}, &second); err != nil {
			t.Error(err)
		}
	}()
	wg.Wait()
	if err := <-agentErr; err != nil {
		t.Fatal(err)
	}
	if first.Method != "first" || second.Method != "second" {
		t.Fatalf("results = %+v %+v", first, second)
	}
	if len(handler.requests) != 1 || handler.requests[0] != "client/ping" {
		t.Fatalf("callback requests = %v", handler.requests)
	}
}

func TestConnectionPreservesNotificationOrderBeforeResponse(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()
	handler := &testHandler{}
	conn := NewConnection(clientSide, clientSide, handler, Options{})

	go func() {
		reader := bufio.NewReader(agentSide)
		line, _ := reader.ReadBytes('\n')
		var request wireEnvelope
		_ = json.Unmarshal(line, &request)
		fmt.Fprintln(agentSide, `{"jsonrpc":"2.0","method":"one","params":{}}`)
		fmt.Fprintln(agentSide, `{"jsonrpc":"2.0","method":"two","params":{}}`)
		fmt.Fprintf(agentSide, `{"jsonrpc":"2.0","id":%s,"result":{}}`+"\n", request.ID)
	}()

	if err := conn.Call(context.Background(), "prompt", map[string]any{}, &struct{}{}); err != nil {
		t.Fatal(err)
	}
	handler.mu.Lock()
	defer handler.mu.Unlock()
	if got := strings.Join(handler.notifications, ","); got != "one,two" {
		t.Fatalf("notification order = %q", got)
	}
}

func TestConnectionCallAfterWriteFiresBeforeResponse(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()
	conn := NewConnection(clientSide, clientSide, &testHandler{}, Options{})

	wrote := make(chan struct{})
	go func() {
		reader := bufio.NewReader(agentSide)
		line, err := reader.ReadBytes('\n')
		if err != nil {
			return
		}
		<-wrote
		var request wireEnvelope
		_ = json.Unmarshal(line, &request)
		fmt.Fprintf(agentSide, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`+"\n", request.ID)
	}()

	hooked := false
	var result struct {
		OK bool `json:"ok"`
	}
	if err := conn.CallAfterWrite(context.Background(), "prompt", map[string]any{}, &result, func() {
		hooked = true
		close(wrote)
	}); err != nil {
		t.Fatal(err)
	}
	if !hooked {
		t.Fatal("afterWrite hook was not called")
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

func TestConnectionSupportsFramesLargerThanScannerDefault(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()
	conn := NewConnection(clientSide, clientSide, &testHandler{}, Options{MaxFrameBytes: 2 << 20})

	large := strings.Repeat("x", 256<<10)
	go func() {
		reader := bufio.NewReader(agentSide)
		line, _ := reader.ReadBytes('\n')
		var request wireEnvelope
		_ = json.Unmarshal(line, &request)
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(request.ID), "result": map[string]string{"value": large}})
		agentSide.Write(append(data, '\n'))
	}()
	var result struct {
		Value string `json:"value"`
	}
	if err := conn.Call(context.Background(), "large", nil, &result); err != nil {
		t.Fatal(err)
	}
	if result.Value != large {
		t.Fatalf("large result length = %d", len(result.Value))
	}
}

func TestConnectionRejectsOversizedFrameAndUnblocksCall(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()
	conn := NewConnection(clientSide, clientSide, &testHandler{}, Options{MaxFrameBytes: 128})

	go func() {
		reader := bufio.NewReader(agentSide)
		_, _ = reader.ReadBytes('\n')
		fmt.Fprintln(agentSide, strings.Repeat("x", 256))
	}()
	var result any
	err := conn.Call(context.Background(), "oversized", nil, &result)
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("error = %v, want ErrFrameTooLarge", err)
	}
}

func TestCallCancellationSendsCancelNotification(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	defer agentSide.Close()
	conn := NewConnection(clientSide, clientSide, &testHandler{}, Options{})

	seenCancel := make(chan bool, 1)
	go func() {
		reader := bufio.NewReader(agentSide)
		first, _ := reader.ReadBytes('\n')
		var request wireEnvelope
		_ = json.Unmarshal(first, &request)
		second, _ := reader.ReadBytes('\n')
		var cancel wireEnvelope
		_ = json.Unmarshal(second, &cancel)
		seenCancel <- cancel.Method == "$/cancel_request" && strings.Contains(string(cancel.Params), string(request.ID))
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- conn.Call(ctx, "slow", nil, &struct{}{}) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("call error = %v", err)
	}
	if !<-seenCancel {
		t.Fatal("cancel notification was not sent with request ID")
	}
}

func TestConnectionEOFUnblocksPendingCalls(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	conn := NewConnection(clientSide, clientSide, &testHandler{}, Options{})

	done := make(chan error, 1)
	go func() { done <- conn.Call(context.Background(), "wait", nil, &struct{}{}) }()
	reader := bufio.NewReader(agentSide)
	if _, err := reader.ReadBytes('\n'); err != nil {
		t.Fatal(err)
	}
	agentSide.Close()
	if err := <-done; !errors.Is(err, io.EOF) {
		t.Fatalf("call error = %v, want EOF", err)
	}
	select {
	case <-conn.Done():
	case <-time.After(time.Second):
		t.Fatal("connection Done was not closed")
	}
}

func TestConnectionDeliversResponseQueuedBeforeEOF(t *testing.T) {
	clientSide, agentSide := net.Pipe()
	defer clientSide.Close()
	conn := NewConnection(clientSide, clientSide, &testHandler{}, Options{})

	go func() {
		reader := bufio.NewReader(agentSide)
		line, _ := reader.ReadBytes('\n')
		var request wireEnvelope
		_ = json.Unmarshal(line, &request)
		fmt.Fprintf(agentSide, `{"jsonrpc":"2.0","id":%s,"result":{"ok":true}}`+"\n", request.ID)
		agentSide.Close()
	}()
	var result struct {
		OK bool `json:"ok"`
	}
	if err := conn.Call(context.Background(), "last", nil, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("result = %+v", result)
	}
}

func TestNumericIDAcceptsIntegralJSONNumbers(t *testing.T) {
	for raw, want := range map[string]int64{"1": 1, "1.0": 1, "-2.000": -2} {
		got, ok := numericID(json.RawMessage(raw))
		if !ok || got != want {
			t.Fatalf("numericID(%q) = %d, %t; want %d, true", raw, got, ok, want)
		}
	}
	for _, raw := range []string{"1.5", `"1"`, "null", "9223372036854775808"} {
		if got, ok := numericID(json.RawMessage(raw)); ok {
			t.Fatalf("numericID(%q) = %d, true; want rejected", raw, got)
		}
	}
}
