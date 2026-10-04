package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type typedInlinePageProvider struct {
	result chan llm.ToolExecutionResponse
}

func (p *typedInlinePageProvider) Name() string       { return "typed-inline-page" }
func (p *typedInlinePageProvider) Credential() string { return "test" }
func (p *typedInlinePageProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true, InlineToolLoop: true}
}
func (p *typedInlinePageProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	stream := &pendingTestStream{events: make(chan llm.Event)}
	go func() {
		defer close(stream.events)
		call := llm.ToolCall{ID: "call_phone", Name: "webmcp__ping", Arguments: json.RawMessage(`{"message":"hello"}`)}
		response := make(chan llm.ToolExecutionResponse, 1)
		select {
		case stream.events <- llm.Event{Type: llm.EventToolCall, Tool: &call, ToolResponse: response}:
		case <-ctx.Done():
			return
		}
		select {
		case result := <-response:
			select {
			case p.result <- result:
			case <-ctx.Done():
				return
			}
		case <-ctx.Done():
			return
		}
		select {
		case stream.events <- llm.Event{Type: llm.EventDone}:
		case <-ctx.Done():
		}
	}()
	return stream, nil
}

func TestTypedInlinePageToolResponseProtocol(t *testing.T) {
	provider := &typedInlinePageProvider{result: make(chan llm.ToolExecutionResponse, 1)}
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	srv.sessionMgr = newServeSessionManager(time.Minute, 8, func(context.Context) (*serveRuntime, error) {
		rt := &serveRuntime{provider: provider, providerKey: provider.Name(), engine: llm.NewEngine(provider, nil), defaultModel: "mock-model", maxTurns: 4}
		rt.Touch()
		return rt, nil
	})
	defer srv.sessionMgr.Close()
	defer srv.responseRuns.Close()
	ts := newServeHTTPTestServer(srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := `{"input":"ping my phone","client_message_id":"typed-inline-1","stream":true,"tools":[{"type":"function","name":"webmcp__ping","description":"ping","parameters":{"type":"object"}}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Term-LLM-UI-Version", "test")
	req.Header.Set("session_id", "typed-inline-phone")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status %d: %s", resp.StatusCode, b)
	}
	id := resp.Header.Get("x-response-id")
	if id == "" {
		t.Fatal("missing response id")
	}
	scanner := bufio.NewScanner(resp.Body)
	for {
		name, data, ok := readSSEEvent(t, scanner)
		if !ok {
			t.Fatal("stream ended before inline request")
		}
		if name != "response.client_tool.requested" {
			continue
		}
		var payload struct {
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		}
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.CallID != "call_phone" || payload.Name != "webmcp__ping" || payload.Arguments != `{"message":"hello"}` {
			t.Fatalf("request: %+v", payload)
		}
		resultReq, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/responses/"+id+"/client_tool_calls/call_phone/result", strings.NewReader(`{"output":"pong from phone"}`))
		if err != nil {
			t.Fatal(err)
		}
		resultReq.Header.Set("Content-Type", "application/json")
		resultResp, err := ts.Client().Do(resultReq)
		if err != nil {
			t.Fatal(err)
		}
		resultResp.Body.Close()
		if resultResp.StatusCode != http.StatusOK {
			t.Fatalf("result status %d", resultResp.StatusCode)
		}
		select {
		case result := <-provider.result:
			if !strings.Contains(result.Result.Content, "pong from phone") {
				t.Fatalf("provider result = %+v", result)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return
	}
}

func TestTypedInlinePageToolCancellationAndScope(t *testing.T) {
	run := newResponseRun("resp_scoped", "session_scoped", "", "mock", time.Now().Unix(), nil)
	runner := &typedClientToolRunner{run: run, names: map[string]bool{"webmcp__ping": true}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := runner.RunClientTool(ctx, llm.ToolCall{ID: "call/scoped", Name: "webmcp__ping"})
		done <- err
	}()
	// The response's durable SSE event exposes this call only after registration.
	deadline := time.After(2 * time.Second)
	for {
		run.mu.Lock()
		found := len(run.events) > 0
		run.mu.Unlock()
		if found {
			break
		}
		select {
		case <-deadline:
			t.Fatal("no inline request event")
		default:
			runtime.Gosched()
		}
	}
	srv := &serveServer{responseRuns: newServeResponseRunManager()}
	if err := srv.responseRuns.create(run); err != nil {
		t.Fatal(err)
	}
	defer srv.responseRuns.Close()
	run.mu.Lock()
	run.typedClientTools = runner
	run.mu.Unlock()
	pending, ok := run.snapshot()["pending_inline_client_tools"].([]map[string]string)
	if !ok || len(pending) != 1 || pending[0]["call_id"] != "call/scoped" || pending[0]["name"] != "webmcp__ping" || pending[0]["arguments"] != "{}" {
		t.Fatalf("recovery snapshot pending inline tools = %#v", pending)
	}
	post := func(id, callID, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses/"+id+"/client_tool_calls/"+url.PathEscape(callID)+"/result", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rr := httptest.NewRecorder()
		srv.handleResponseByID(rr, req)
		return rr.Code
	}
	if code := post("resp_other", "call/scoped", `{"output":"wrong"}`); code != http.StatusNotFound {
		t.Fatalf("other response status %d", code)
	}
	if code := post(run.id, "call_other", `{"output":"wrong"}`); code != http.StatusNotFound {
		t.Fatalf("other call status %d", code)
	}
	if code := post(run.id, "call/scoped", `{"output":"pong"}`); code != http.StatusOK {
		t.Fatalf("answer status %d", code)
	}
	if pending := run.snapshot()["pending_inline_client_tools"].([]map[string]string); len(pending) != 0 {
		t.Fatalf("answered call still pending in recovery snapshot: %#v", pending)
	}
	if code := post(run.id, "call/scoped", `{"output":"pong"}`); code != http.StatusOK {
		t.Fatalf("replay status %d", code)
	}
	if code := post(run.id, "call/scoped", `{"output":"other"}`); code != http.StatusConflict {
		t.Fatalf("conflict status %d", code)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-deadline:
		t.Fatal("no tool result")
	}
	if code := post(run.id, "call/scoped", `{"output":"pong"}`); code != http.StatusOK {
		t.Fatalf("late replay status %d", code)
	}

	parallel := make(chan string, 2)
	for _, callID := range []string{"parallel_1", "parallel_2"} {
		go func(callID string) {
			output, err := runner.RunClientTool(ctx, llm.ToolCall{ID: callID, Name: "webmcp__ping"})
			if err != nil {
				parallel <- err.Error()
				return
			}
			parallel <- output.Content
		}(callID)
	}
	for {
		run.mu.Lock()
		count := len(run.events)
		run.mu.Unlock()
		if count >= 3 {
			break
		}
		select {
		case <-time.After(2 * time.Second):
			t.Fatal("parallel calls not requested")
		default:
			runtime.Gosched()
		}
	}
	for _, callID := range []string{"parallel_1", "parallel_2"} {
		if code := post(run.id, callID, `{"output":"`+callID+`"}`); code != http.StatusOK {
			t.Fatalf("parallel result %s: %d", callID, code)
		}
	}
	for range 2 {
		select {
		case output := <-parallel:
			if output != "parallel_1" && output != "parallel_2" {
				t.Fatalf("parallel output %q", output)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("parallel call did not complete")
		}
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	cancelled := make(chan error, 1)
	go func() {
		_, err := runner.RunClientTool(ctx2, llm.ToolCall{ID: "call_cancel", Name: "webmcp__ping"})
		cancelled <- err
	}()
	cancel2()
	select {
	case err := <-cancelled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel error %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancel did not release runner")
	}
	if code := post(run.id, "call_cancel", `{"output":"late"}`); code != http.StatusNotFound {
		t.Fatalf("cancelled result status %d", code)
	}
	cancel()
}
