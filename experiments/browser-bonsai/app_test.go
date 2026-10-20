package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedInference replays deterministic browser-inference behaviour. It
// asserts the exact wire contract the Go provider sends (tools, transcript,
// tool results) so a regression in either side fails here.
type scriptedInference struct {
	t       *testing.T
	mu      sync.Mutex
	turns   []scriptedTurn
	calls   int
	cancel  context.CancelFunc
	cancels int
}

type scriptedTurn struct {
	check   func(t *testing.T, request map[string]any)
	content string
	calls   []scriptedCall
	block   bool
}

type scriptedCall struct {
	id, name, arguments string
}

func (s *scriptedInference) Complete(ctx context.Context, requestJSON string, onDelta func(string)) (string, error) {
	s.mu.Lock()
	turnIndex := s.calls
	s.calls++
	var turn scriptedTurn
	if turnIndex < len(s.turns) {
		turn = s.turns[turnIndex]
	}
	s.cancel = context.CancelFunc(func() {})
	s.mu.Unlock()

	var request map[string]any
	if err := json.Unmarshal([]byte(requestJSON), &request); err != nil {
		s.t.Errorf("request is not JSON: %v", err)
		return "", err
	}
	if turn.check != nil {
		turn.check(s.t, request)
	}
	if turn.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if onDelta != nil && turn.content != "" {
		chunk, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": turn.content}}},
		})
		onDelta(string(chunk))
	}
	message := map[string]any{"role": "assistant", "content": turn.content}
	if len(turn.calls) > 0 {
		toolCalls := make([]any, 0, len(turn.calls))
		for _, call := range turn.calls {
			toolCalls = append(toolCalls, map[string]any{
				"id": call.id, "type": "function",
				"function": map[string]any{"name": call.name, "arguments": call.arguments},
			})
		}
		message["tool_calls"] = toolCalls
		message["content"] = ""
	}
	response, _ := json.Marshal(map[string]any{
		"id": "bonsai_test", "model": demoModelName,
		"choices": []any{map[string]any{"message": message, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
	})
	return string(response), nil
}

func (s *scriptedInference) CancelInference() {
	s.mu.Lock()
	s.cancels++
	s.mu.Unlock()
}

func newTestApp(t *testing.T, inference Inference, seed map[string]string) (*App, string) {
	t.Helper()
	workspace := t.TempDir()
	for name, content := range seed {
		path := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app, err := NewAppWithWorkspace(inference, workspace, false)
	if err != nil {
		t.Fatalf("new app: %v", err)
	}
	return app, workspace
}

func createSession(t *testing.T, app *App) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/demo-api/v1/sessions", strings.NewReader(`{}`))
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create session status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Session.ID == "" {
		t.Fatal("create session returned no id")
	}
	return response.Session.ID
}

func postResponse(t *testing.T, app *App, sessionID, input string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"input": input, "stream": true})
	request := httptest.NewRequest(http.MethodPost, "/demo-api/v1/responses", strings.NewReader(string(body)))
	request.Header.Set("X-Term-LLM-Session-ID", sessionID)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	return recorder
}

func sseEvents(t *testing.T, body string) map[string][]map[string]any {
	t.Helper()
	events := map[string][]map[string]any{}
	for _, block := range strings.Split(body, "\n\n") {
		var name string
		var data string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event: ") {
				name = strings.TrimPrefix(line, "event: ")
			}
			if strings.HasPrefix(line, "data: ") {
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if name == "" || data == "" || data == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(data), &payload); err != nil {
			t.Fatalf("event %s payload: %v", name, err)
		}
		events[name] = append(events[name], payload)
	}
	return events
}

func roleMessages(request map[string]any) []map[string]any {
	raw, _ := request["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		message, _ := entry.(map[string]any)
		out = append(out, message)
	}
	return out
}

func TestBrowserAgentUsesRealFileTools(t *testing.T) {
	inference := &scriptedInference{t: t, turns: []scriptedTurn{
		{
			check: func(t *testing.T, request map[string]any) {
				tools, ok := request["tools"].([]any)
				if !ok || len(tools) == 0 {
					t.Fatal("first provider turn has no tools")
				}
				for _, entry := range tools {
					tool, _ := entry.(map[string]any)
					fn, _ := tool["function"].(map[string]any)
					if fn["name"] == toolsManageWorkspaceName {
						t.Fatal("manage_workspace leaked into the provider tool surface")
					}
				}
				messages := roleMessages(request)
				if len(messages) < 2 || messages[0]["role"] != "system" {
					t.Fatalf("messages = %#v, want system + user", messages)
				}
			},
			calls: []scriptedCall{{id: "call_read", name: toolsReadToolName, arguments: `{"path":"README.md"}`}},
		},
		{
			check: func(t *testing.T, request map[string]any) {
				messages := roleMessages(request)
				var toolContent string
				for _, message := range messages {
					if message["role"] == "tool" {
						toolContent = fmt.Sprint(message["content"])
					}
				}
				if !strings.Contains(toolContent, "hello browser workspace") {
					t.Fatalf("tool result did not carry file contents: %q", toolContent)
				}
			},
			calls: []scriptedCall{{id: "call_write", name: toolsWriteToolName, arguments: `{"path":"proof.txt","content":"BROWSER_OK"}`}},
		},
		{
			check: func(t *testing.T, request map[string]any) {
				messages := roleMessages(request)
				last := messages[len(messages)-1]
				if last["role"] != "tool" || !strings.Contains(fmt.Sprint(last["content"]), "proof.txt") {
					t.Fatalf("last message = %#v, want write_file result", last)
				}
			},
			content: "Wrote proof.txt with the requested contents.",
		},
	}}

	app, workspace := newTestApp(t, inference, map[string]string{"README.md": "hello browser workspace\n"})
	sessionID := createSession(t, app)
	recorder := postResponse(t, app, sessionID, "Read README.md and write proof.txt")
	if recorder.Code != http.StatusOK {
		t.Fatalf("response status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Response-ID"); !strings.HasPrefix(got, "resp_") {
		t.Fatalf("X-Response-ID = %q", got)
	}
	events := sseEvents(t, recorder.Body.String())
	for _, required := range []string{"response.created", "response.output_text.delta", "response.output_item.added", "response.tool_exec.start", "response.tool_exec.end", "response.completed"} {
		if len(events[required]) == 0 {
			t.Fatalf("missing %s event; got %v", required, eventNames(events))
		}
	}
	if len(events["response.tool_exec.end"]) != 2 {
		t.Fatalf("tool_exec.end count = %d, want 2", len(events["response.tool_exec.end"]))
	}
	// Sequence numbers are contiguous and start at 1.
	sequence := 1
	for _, name := range []string{"response.created"} {
		for _, payload := range events[name] {
			if payload["sequence_number"] != float64(sequence) {
				t.Fatalf("%s sequence = %v, want %d", name, payload["sequence_number"], sequence)
			}
			sequence++
		}
	}

	proof, err := os.ReadFile(filepath.Join(workspace, "proof.txt"))
	if err != nil {
		t.Fatalf("proof.txt not written by the real write_file tool: %v", err)
	}
	if string(proof) != "BROWSER_OK" {
		t.Fatalf("proof.txt = %q", proof)
	}

	// The durable transcript must include the assistant answer and both tools.
	selected := httptest.NewRecorder()
	app.Handler().ServeHTTP(selected, httptest.NewRequest(http.MethodGet,
		"/demo-api/v1/sessions?selected_only=1&include_transcript=1&selected_session="+sessionID, nil))
	if selected.Code != http.StatusOK {
		t.Fatalf("selected session status = %d", selected.Code)
	}
	if !strings.Contains(selected.Body.String(), "Wrote proof.txt with the requested contents.") {
		t.Fatalf("transcript missing assistant answer: %s", selected.Body.String())
	}
	if !strings.Contains(selected.Body.String(), `"tool_name":"read_file"`) {
		t.Fatalf("transcript missing read_file tool row: %s", selected.Body.String())
	}
	if inference.calls != 3 {
		t.Fatalf("inference calls = %d, want 3 engine turns", inference.calls)
	}
}

func TestCancelEmitsCancelledAndStopsInference(t *testing.T) {
	inference := &scriptedInference{t: t, turns: []scriptedTurn{{block: true}}}
	app, _ := newTestApp(t, inference, nil)
	sessionID := createSession(t, app)
	body, _ := json.Marshal(map[string]any{"input": "please hang", "stream": true})
	request := httptest.NewRequest(http.MethodPost, "/demo-api/v1/responses", strings.NewReader(string(body)))
	request.Header.Set("X-Term-LLM-Session-ID", sessionID)
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		app.Handler().ServeHTTP(recorder, request)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		app.mu.Lock()
		run := app.runs[sessionID]
		app.mu.Unlock()
		if run != nil {
			if !app.cancelResponse(run.ResponseID) {
				t.Fatal("cancelResponse refused a live run")
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("response handler did not stop after cancel")
	}
	events := sseEvents(t, recorder.Body.String())
	if len(events["response.cancelled"]) == 0 {
		t.Fatalf("missing response.cancelled; got %v", eventNames(events))
	}
	inference.mu.Lock()
	cancels := inference.cancels
	inference.mu.Unlock()
	if cancels == 0 {
		t.Fatal("browser inference was not cancelled")
	}
}

func TestSessionLifecycleEndpoints(t *testing.T) {
	app, _ := newTestApp(t, &scriptedInference{t: t}, nil)
	sessionID := createSession(t, app)

	list := httptest.NewRecorder()
	app.Handler().ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/demo-api/v1/sessions?limit=30", nil))
	if !strings.Contains(list.Body.String(), sessionID) {
		t.Fatalf("session list missing %s: %s", sessionID, list.Body.String())
	}
	state := httptest.NewRecorder()
	app.Handler().ServeHTTP(state, httptest.NewRequest(http.MethodGet, "/demo-api/v1/sessions/"+sessionID+"/state", nil))
	for _, field := range []string{"last_response_id", "active_response_id", "approval_policy"} {
		if !strings.Contains(state.Body.String(), field) {
			t.Fatalf("state missing %s: %s", field, state.Body.String())
		}
	}
	capabilities := httptest.NewRecorder()
	app.Handler().ServeHTTP(capabilities, httptest.NewRequest(http.MethodGet, "/demo-api/v1/capabilities", nil))
	if strings.Contains(capabilities.Body.String(), "cache_scope") {
		t.Fatal("capabilities must not advertise a cache scope")
	}
}

func eventNames(events map[string][]map[string]any) []string {
	names := make([]string, 0, len(events))
	for name := range events {
		names = append(names, name)
	}
	return names
}

const (
	toolsReadToolName        = "read_file"
	toolsWriteToolName       = "write_file"
	toolsManageWorkspaceName = "manage_workspace"
)

func TestParseResponseInputAcceptsGenuineFrontendShape(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "message with string content",
			body: `[{"type":"message","role":"user","client_message_id":"c1","content":"Read README.md"}]`,
			want: "Read README.md",
		},
		{
			name: "message with input_text parts",
			body: `[{"type":"message","role":"user","content":[{"type":"input_text","text":"one"},{"type":"input_text","text":"two"}]}]`,
			want: "one\ntwo",
		},
		{
			name: "bare string",
			body: `"hello"`,
			want: "hello",
		},
		{
			name: "function call output continuation",
			body: `[{"type":"function_call_output","call_id":"c","output":"tool says hi"}]`,
			want: "tool says hi",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			parts := parseResponseInput(json.RawMessage(testCase.body))
			var texts []string
			for _, part := range parts {
				texts = append(texts, part.Text)
			}
			if got := strings.Join(texts, "\n"); got != testCase.want {
				t.Fatalf("parsed %q, want %q", got, testCase.want)
			}
		})
	}
}

func TestGenuineResponseRequestCommitsUserMessage(t *testing.T) {
	inference := &scriptedInference{t: t, turns: []scriptedTurn{{content: "ok"}}}
	app, _ := newTestApp(t, inference, nil)
	sessionID := createSession(t, app)
	body := `{"stream":true,"model":"bonsai-2-2b","input":[{"type":"message","role":"user","client_message_id":"intent-1","content":"hello from the real UI"}]}`
	request := httptest.NewRequest(http.MethodPost, "/demo-api/v1/responses", strings.NewReader(body))
	request.Header.Set("X-Term-LLM-Session-ID", sessionID)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	app.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "input is required") {
		t.Fatalf("genuine body rejected: %s", recorder.Body.String())
	}
	session := app.session(sessionID)
	if session == nil {
		t.Fatal("session disappeared")
	}
	var user *demoMessage
	for _, message := range session.Messages {
		if message.Role == "user" {
			user = message
		}
	}
	if user == nil || userText(user.Parts) != "hello from the real UI" {
		t.Fatalf("user message = %#v", user)
	}
	if user.ClientMessageID != "intent-1" {
		t.Fatalf("client message id = %q, want intent-1", user.ClientMessageID)
	}
	if len(inference.turns) != 1 || inference.calls != 1 {
		t.Fatalf("inference calls = %d", inference.calls)
	}
}
