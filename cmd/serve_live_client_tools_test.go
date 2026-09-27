package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
)

// pageToolDefinition is a page tool as the web UI declares it.
func pageToolDefinition(name string) json.RawMessage {
	return json.RawMessage(`{"type":"function","name":"` + name + `","description":"Replies pong. Runs on the user's phone.",` +
		`"parameters":{"type":"object","properties":{"message":{"type":"string"}}}}`)
}

func pageToolCall(id string) llm.ToolCall {
	return llm.ToolCall{ID: id, Name: "webmcp__ping", Arguments: json.RawMessage(`{"message":"hi"}`)}
}

// requestedToolCalls returns every live.tool_calls_requested published so far.
func requestedToolCalls(record *liveSession) []liveSessionEvent {
	var requested []liveSessionEvent
	for _, event := range liveEventSnapshot(record) {
		if event.Type == liveEventToolCallsRequested {
			requested = append(requested, event)
		}
	}
	return requested
}

func waitForRequestedToolCalls(t *testing.T, record *liveSession, count int) []liveSessionEvent {
	t.Helper()
	var requested []liveSessionEvent
	waitForLiveCondition(t, fmt.Sprintf("%d live.tool_calls_requested events", count), func() bool {
		requested = requestedToolCalls(record)
		return len(requested) >= count
	})
	return requested
}

// requestedCalls decodes the calls one request event asked the client to run.
func requestedCalls(t *testing.T, event liveSessionEvent) (string, []liveClientToolCall) {
	t.Helper()
	requestID, _ := event.Data["request_id"].(string)
	calls, ok := event.Data["calls"].([]liveClientToolCall)
	if requestID == "" || !ok {
		t.Fatalf("tool call request payload = %+v", event.Data)
	}
	return requestID, calls
}

func postLiveToolResult(t *testing.T, srv *serveServer, liveID, requestID, body string) (int, map[string]any) {
	t.Helper()
	return postLivePath(t, srv, http.MethodPost, "/v1/live/sessions/"+liveID+"/tool_calls/"+requestID+"/result", body)
}

func postLivePath(t *testing.T, srv *serveServer, method, path, body string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.handleLiveSessionByID(recorder, request)
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		_ = json.Unmarshal(recorder.Body.Bytes(), &decoded)
	}
	return recorder.Code, decoded
}

// toolResultContent finds the tool result a model request carried for callID.
func toolResultContent(request llm.Request, callID string) (string, bool) {
	for _, message := range request.Messages {
		for _, part := range message.Parts {
			if part.Type == llm.PartToolResult && part.ToolResult != nil && part.ToolResult.ID == callID {
				return part.ToolResult.Content, true
			}
		}
	}
	return "", false
}

func requestOffersTool(request llm.Request, name string) bool {
	return slices.ContainsFunc(request.Tools, func(spec llm.ToolSpec) bool { return spec.Name == name })
}

// liveSpeech collects what a delegation said aloud.
type liveSpeech struct {
	mu   sync.Mutex
	text strings.Builder
}

func (s *liveSpeech) emit(chunk live.DelegationChunk) {
	if chunk.Channel != live.ChannelSpeakable || chunk.Progress {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.text.WriteString(chunk.Text)
}

func (s *liveSpeech) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.text.String()
}

// newPageToolDelegation registers a server-mode call on sessionID that declared
// tools, and returns the session's scripted provider and a delegator with
// test-speed pacing.
func newPageToolDelegation(t *testing.T, sessionID string, tools ...json.RawMessage) (*serveServer, *liveSession, *llm.MockProvider, *serveLiveDelegator) {
	t.Helper()
	srv := newTestServeServer()
	runtime, _, err := srv.runtimeForRequest(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := runtime.provider.(*llm.MockProvider)
	if !ok {
		t.Fatalf("unexpected provider %T", runtime.provider)
	}
	record := newLiveSession("live_"+sessionID, sessionID)
	record.delegationMode = liveDelegationModeServer
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	record.clientTools = tools
	if err := srv.registerLiveSession(record); err != nil {
		t.Fatal(err)
	}
	delegator := &serveLiveDelegator{server: srv, live: record, toolResendInterval: time.Hour, toolTimeout: 10 * time.Second}
	return srv, record, provider, delegator
}

func runPageToolDelegation(ctx context.Context, delegator *serveLiveDelegator, input string) (chan error, *liveSpeech) {
	speech := &liveSpeech{}
	done := make(chan error, 1)
	go func() {
		done <- delegator.Run(ctx, live.DelegationRequest{ID: "item_" + strings.ReplaceAll(input, " ", "_"), Input: input}, speech.emit)
	}()
	return done, speech
}

func waitForDelegationRun(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the delegation never finished")
		return nil
	}
}

func TestLiveStartValidatesClientTools(t *testing.T) {
	tooMany := make([]json.RawMessage, liveClientToolsMax+1)
	for i := range tooMany {
		tooMany[i] = pageToolDefinition(fmt.Sprintf("webmcp__tool_%d", i))
	}
	for _, testCase := range []struct {
		name   string
		mode   string
		tools  []json.RawMessage
		status int
		want   int
	}{
		{name: "default mode accepts page tools", tools: []json.RawMessage{pageToolDefinition("webmcp__ping")}, status: http.StatusOK, want: 1},
		{name: "server mode accepts page tools", mode: liveDelegationModeServer, tools: []json.RawMessage{pageToolDefinition("webmcp__ping")}, status: http.StatusOK, want: 1},
		{name: "no tools", status: http.StatusOK},
		{name: "client mode refuses page tools", mode: liveDelegationModeClient, tools: []json.RawMessage{pageToolDefinition("webmcp__ping")}, status: http.StatusBadRequest},
		{name: "not an object", tools: []json.RawMessage{json.RawMessage(`"webmcp__ping"`)}, status: http.StatusBadRequest},
		{name: "null entry", tools: []json.RawMessage{json.RawMessage(`null`)}, status: http.StatusBadRequest},
		{name: "not a function", tools: []json.RawMessage{json.RawMessage(`{"type":"web_search"}`)}, status: http.StatusBadRequest},
		{name: "unsafe name", tools: []json.RawMessage{pageToolDefinition("webmcp ping")}, status: http.StatusBadRequest},
		{name: "duplicate names", tools: []json.RawMessage{pageToolDefinition("webmcp__ping"), pageToolDefinition("webmcp__ping")}, status: http.StatusBadRequest},
		{name: "too many", tools: tooMany, status: http.StatusBadRequest},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			harness := newClientDelegationHarness(t)
			body := map[string]any{"session_id": "tools-" + strings.ReplaceAll(testCase.name, " ", "-")}
			if testCase.mode != "" {
				body["delegation_mode"] = testCase.mode
			}
			if testCase.tools != nil {
				body["client_tools"] = testCase.tools
			}
			status, decoded := harness.start(t, body)
			if status != testCase.status {
				t.Fatalf("status = %d, want %d (body %v)", status, testCase.status, decoded)
			}
			if status != http.StatusOK {
				return
			}
			record, ok := harness.srv.lookupLiveSession(fmt.Sprint(decoded["live_id"]))
			if !ok {
				t.Fatal("the call was not registered")
			}
			if got := len(record.declaredClientTools()); got != testCase.want {
				t.Fatalf("declared tools = %d, want %d", got, testCase.want)
			}
		})
	}
}

func TestLiveClientToolsRouteReplacesTheDeclaration(t *testing.T) {
	harness := newClientDelegationHarness(t)
	status, decoded := harness.start(t, map[string]any{
		"session_id": "declare-session", "client_tools": []json.RawMessage{pageToolDefinition("webmcp__ping")},
	})
	if status != http.StatusOK {
		t.Fatalf("start status = %d, body = %v", status, decoded)
	}
	liveID := fmt.Sprint(decoded["live_id"])
	record, _ := harness.srv.lookupLiveSession(liveID)
	path := "/v1/live/sessions/" + liveID + "/client_tools"

	replacement := `{"tools":[` + string(pageToolDefinition("webmcp__music_search")) + `]}`
	if status, body := postLivePath(t, harness.srv, http.MethodPost, path, replacement); status != http.StatusOK || body["tools"] != float64(1) {
		t.Fatalf("replace status = %d, body = %v", status, body)
	}
	if names := liveClientToolNames(record.declaredClientTools()); !names["webmcp__music_search"] || names["webmcp__ping"] {
		t.Fatalf("declared tools after replacement = %v", names)
	}
	// A rejected declaration leaves the previous one in force.
	if status, _ := postLivePath(t, harness.srv, http.MethodPost, path, `{"tools":[{"type":"function","name":"bad name"}]}`); status != http.StatusBadRequest {
		t.Fatalf("invalid declaration status = %d, want 400", status)
	}
	if names := liveClientToolNames(record.declaredClientTools()); !names["webmcp__music_search"] {
		t.Fatalf("an invalid declaration replaced the tools: %v", names)
	}
	if status, _ := postLivePath(t, harness.srv, http.MethodPost, path, `{"tools":[]}`); status != http.StatusOK {
		t.Fatalf("clearing status = %d", status)
	}
	if tools := record.declaredClientTools(); len(tools) != 0 {
		t.Fatalf("declared tools after clearing = %d", len(tools))
	}
	if status, _ := postLivePath(t, harness.srv, http.MethodGet, path, ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", status)
	}
	if status, _ := postLivePath(t, harness.srv, http.MethodPost, "/v1/live/sessions/live_missing/client_tools", `{"tools":[]}`); status != http.StatusNotFound {
		t.Fatalf("unknown call status = %d, want 404", status)
	}
	record.appendEvent(liveEventEnded, map[string]any{})
	if status, _ := postLivePath(t, harness.srv, http.MethodPost, path, `{"tools":[]}`); status != http.StatusConflict {
		t.Fatalf("ended call status = %d, want 409", status)
	}

	clientID, _ := harness.startClient(t, "declare-client-session", "")
	if status, _ := postLivePath(t, harness.srv, http.MethodPost, "/v1/live/sessions/"+clientID+"/client_tools", `{"tools":[]}`); status != http.StatusBadRequest {
		t.Fatalf("client-mode declaration status = %d, want 400", status)
	}
}

// TestLiveDelegationRunsPageToolsOnTheClientAndSpeaksTheContinuation covers the
// whole server-mode round trip through the real controller: the declared page
// tool reaches the delegated turn, the call it stops on is published to the
// client, and the client's result continues the turn to the spoken answer.
func TestLiveDelegationRunsPageToolsOnTheClientAndSpeaksTheContinuation(t *testing.T) {
	harness := newClientDelegationHarness(t)
	const sessionID = "page-tools-session"
	runtime, _, err := harness.srv.runtimeForRequest(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.provider.(*llm.MockProvider)
	provider.AddToolCall("call_ping", "webmcp__ping", map[string]any{"message": "hi"}).AddTextResponse("Your phone answered pong.")

	status, decoded := harness.start(t, map[string]any{
		"session_id": sessionID, "client_tools": []json.RawMessage{pageToolDefinition("webmcp__ping")},
	})
	if status != http.StatusOK {
		t.Fatalf("start status = %d, body = %v", status, decoded)
	}
	liveID := fmt.Sprint(decoded["live_id"])
	record, _ := harness.srv.lookupLiveSession(liveID)
	harness.session.events <- live.Event{Kind: live.EventDelegationCreated, DelegationID: "item_ping", Text: "ping my phone"}

	event := waitForRequestedToolCalls(t, record, 1)[0]
	requestID, calls := requestedCalls(t, event)
	if event.Data["session_id"] != sessionID || event.Data["resend"] != false || event.Data["response_id"] == "" {
		t.Fatalf("tool call request payload = %+v", event.Data)
	}
	if len(calls) != 1 || calls[0].CallID != "call_ping" || calls[0].Name != "webmcp__ping" || calls[0].Arguments != `{"message":"hi"}` {
		t.Fatalf("requested calls = %+v", calls)
	}
	if status, body := postLiveToolResult(t, harness.srv, liveID, requestID, `{"outputs":[{"call_id":"call_ping","output":"pong"}]}`); status != http.StatusOK || body["ok"] != true {
		t.Fatalf("result status = %d, body = %v", status, body)
	}

	waitForDelegationState(t, record, "item_ping", string(live.DelegationDone))
	waitForLiveCondition(t, "the spoken continuation", func() bool {
		harness.session.mu.Lock()
		defer harness.session.mu.Unlock()
		return slices.ContainsFunc(harness.session.delegated, func(chunk live.DelegationChunk) bool {
			return chunk.Channel == live.ChannelSpeakable && strings.Contains(chunk.Text, "Your phone answered pong.")
		})
	})
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("model turns = %d, want the delegation and its continuation", len(requests))
	}
	for i, request := range requests {
		if !requestOffersTool(request, "webmcp__ping") {
			t.Fatalf("model turn %d was not offered the page tool", i+1)
		}
	}
	if output, ok := toolResultContent(requests[1], "call_ping"); !ok || output != "pong" {
		t.Fatalf("continuation carried tool result %q (found %v)", output, ok)
	}
	if prompt := lastUserMessageText(t, requests[1]); !strings.Contains(prompt, "<input>ping my phone</input>") {
		t.Fatalf("the continuation lost the delegated request: %s", prompt)
	}
	// An answered round is retired, and a replay of its result is a duplicate.
	if status, body := postLiveToolResult(t, harness.srv, liveID, requestID, `{"outputs":[{"call_id":"call_ping","output":"pong"}]}`); status != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("replayed result status = %d, body = %v", status, body)
	}
}

// Server tools run inside the host turn as before; only the page calls a run
// stops on go to the client, round after round, until the model answers.
func TestLiveDelegationRunsServerToolsThenPageToolsOverSeveralRounds(t *testing.T) {
	srv, record, provider, delegator := newPageToolDelegation(t, "rounds-session", pageToolDefinition("webmcp__ping"))
	runtime, _, err := srv.runtimeForRequest(context.Background(), "rounds-session")
	if err != nil {
		t.Fatal(err)
	}
	probe := &liveContextProbeTool{}
	runtime.engine.RegisterTool(probe)
	provider.AddToolCall("call_server", "live_probe", map[string]any{})
	provider.AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{pageToolCall("call_one")}})
	provider.AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{pageToolCall("call_two"), pageToolCall("call_three")}})
	provider.AddTextResponse("Both rounds are done.")

	done, speech := runPageToolDelegation(context.Background(), delegator, "use the phone twice")
	requestID, calls := requestedCalls(t, waitForRequestedToolCalls(t, record, 1)[0])
	if len(calls) != 1 || calls[0].CallID != "call_one" {
		t.Fatalf("first round = %+v", calls)
	}
	if status, _ := postLiveToolResult(t, srv, record.id, requestID, `{"outputs":[{"call_id":"call_one","output":"first"}]}`); status != http.StatusOK {
		t.Fatalf("first round status = %d", status)
	}
	requestID, calls = requestedCalls(t, waitForRequestedToolCalls(t, record, 2)[1])
	if len(calls) != 2 || calls[0].CallID != "call_two" || calls[1].CallID != "call_three" {
		t.Fatalf("second round = %+v", calls)
	}
	// Outputs are matched by call id, not by position.
	body := `{"outputs":[{"call_id":"call_three","output":"third"},{"call_id":"call_two","output":""}]}`
	if status, _ := postLiveToolResult(t, srv, record.id, requestID, body); status != http.StatusOK {
		t.Fatalf("second round status = %d", status)
	}

	if err := waitForDelegationRun(t, done); err != nil {
		t.Fatalf("Run = %v", err)
	}
	if !strings.Contains(speech.String(), "Both rounds are done.") {
		t.Fatalf("spoken answer = %q", speech.String())
	}
	if probe.calls.Load() != 1 {
		t.Fatalf("server tool calls = %d, want 1", probe.calls.Load())
	}
	requests := provider.RecordedRequests()
	if len(requests) != 4 {
		t.Fatalf("model turns = %d, want 4", len(requests))
	}
	// Turn 2 follows the server tool; turns 3 and 4 continue the two page rounds.
	for _, check := range []struct {
		turn         int
		callID, want string
	}{{turn: 2, callID: "call_one", want: "first"}, {turn: 3, callID: "call_two"}, {turn: 3, callID: "call_three", want: "third"}} {
		if got, ok := toolResultContent(requests[check.turn], check.callID); !ok || got != check.want {
			t.Fatalf("turn %d result for %s = %q (found %v), want %q", check.turn+1, check.callID, got, ok, check.want)
		}
	}
	if got := len(requestedToolCalls(record)); got != 2 {
		t.Fatalf("published rounds = %d, want 2", got)
	}
}

// Without declared page tools a delegated turn is exactly what it was: nothing
// extra is offered, and a passthrough call it stops on is never sent to the
// client.
func TestLiveDelegationWithoutPageToolsNeverAsksTheClient(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		tools []json.RawMessage
	}{
		{name: "no declaration"},
		{name: "undeclared name", tools: []json.RawMessage{pageToolDefinition("webmcp__music_search")}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, record, provider, delegator := newPageToolDelegation(t, "no-tools-"+strings.ReplaceAll(testCase.name, " ", "-"), testCase.tools...)
			provider.AddTurn(llm.MockTurn{Text: "Working on it.", ToolCalls: []llm.ToolCall{pageToolCall("call_ping")}})
			done, speech := runPageToolDelegation(context.Background(), delegator, "ping my phone")
			if err := waitForDelegationRun(t, done); err != nil {
				t.Fatalf("Run = %v", err)
			}
			if !strings.Contains(speech.String(), "Working on it.") {
				t.Fatalf("spoken answer = %q", speech.String())
			}
			if requested := requestedToolCalls(record); len(requested) != 0 {
				t.Fatalf("published %d tool call requests", len(requested))
			}
			requests := provider.RecordedRequests()
			if len(requests) != 1 || requestOffersTool(requests[0], "webmcp__ping") {
				t.Fatalf("model turns = %d; offered undeclared tool = %v", len(requests), len(requests) > 0 && requestOffersTool(requests[0], "webmcp__ping"))
			}
		})
	}
}

func TestLiveToolCallResultIsValidatedAndIdempotent(t *testing.T) {
	srv, record, _, _ := newPageToolDelegation(t, "result-session", pageToolDefinition("webmcp__ping"))
	calls := []liveClientToolCall{{CallID: "c1", Name: "webmcp__ping", Arguments: "{}"}, {CallID: "c2", Name: "webmcp__ping", Arguments: "{}"}}
	if _, err := record.beginClientToolRequest("tools_1", "result-session", "resp_1", calls); err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name   string
		body   string
		status int
	}{
		{name: "neither", body: `{}`, status: http.StatusBadRequest},
		{name: "both", body: `{"outputs":[{"call_id":"c1","output":"a"},{"call_id":"c2","output":"b"}],"error":"no"}`, status: http.StatusBadRequest},
		{name: "blank error", body: `{"error":"  "}`, status: http.StatusBadRequest},
		{name: "missing call", body: `{"outputs":[{"call_id":"c1","output":"a"}]}`, status: http.StatusBadRequest},
		{name: "unknown call", body: `{"outputs":[{"call_id":"c1","output":"a"},{"call_id":"c9","output":"b"}]}`, status: http.StatusBadRequest},
		{name: "duplicate call", body: `{"outputs":[{"call_id":"c1","output":"a"},{"call_id":"c1","output":"b"}]}`, status: http.StatusBadRequest},
		{name: "missing output", body: `{"outputs":[{"call_id":"c1","output":"a"},{"call_id":"c2"}]}`, status: http.StatusBadRequest},
		{name: "invalid json", body: `{`, status: http.StatusBadRequest},
		{name: "oversize", body: `{"error":"` + strings.Repeat("a", liveClientToolResultLimitBytes) + `"}`, status: http.StatusRequestEntityTooLarge},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if status, body := postLiveToolResult(t, srv, record.id, "tools_1", testCase.body); status != testCase.status {
				t.Fatalf("status = %d, want %d (body %v)", status, testCase.status, body)
			}
		})
	}
	accepted := `{"outputs":[{"call_id":"c2","output":"b"},{"call_id":"c1","output":" a "}]}`
	if status, body := postLiveToolResult(t, srv, record.id, "tools_1", accepted); status != http.StatusOK || body["ok"] != true || body["duplicate"] != nil {
		t.Fatalf("first result status = %d, body = %v", status, body)
	}
	// The same outputs in another order are the same answer; outputs are data,
	// so a whitespace difference is not.
	if status, body := postLiveToolResult(t, srv, record.id, "tools_1", `{"outputs":[{"call_id":"c1","output":" a "},{"call_id":"c2","output":"b"}]}`); status != http.StatusOK || body["duplicate"] != true {
		t.Fatalf("identical replay status = %d, body = %v", status, body)
	}
	if status, _ := postLiveToolResult(t, srv, record.id, "tools_1", `{"outputs":[{"call_id":"c1","output":"a"},{"call_id":"c2","output":"b"}]}`); status != http.StatusConflict {
		t.Fatalf("differing replay status = %d, want 409", status)
	}
	if status, _ := postLiveToolResult(t, srv, record.id, "tools_missing", `{"error":"x"}`); status != http.StatusNotFound {
		t.Fatalf("unknown request status = %d, want 404", status)
	}
	if status, _ := postLiveToolResult(t, srv, "live_missing", "tools_1", `{"error":"x"}`); status != http.StatusNotFound {
		t.Fatalf("unknown call status = %d, want 404", status)
	}
	if status, _ := postLivePath(t, srv, http.MethodGet, "/v1/live/sessions/"+record.id+"/tool_calls/tools_1/result", ""); status != http.StatusMethodNotAllowed {
		t.Fatalf("GET status = %d, want 405", status)
	}
	for _, path := range []string{"/tool_calls/tools_1", "/tool_calls/tools_1/results"} {
		if status, body := postLivePath(t, srv, http.MethodPost, "/v1/live/sessions/"+record.id+path, `{"error":"x"}`); status != http.StatusNotFound || !strings.Contains(fmt.Sprint(body), "tool call request") {
			t.Fatalf("%s status = %d, body = %v", path, status, body)
		}
	}
}

// An answered round keeps only a digest of its result for replays, so the
// digest must tell apart results that a plain concatenation would confuse.
func TestLiveClientToolResultDigestKeepsReplaysExact(t *testing.T) {
	distinct := []liveClientToolResult{
		{Outputs: []string{"ab"}},
		{Outputs: []string{"a", "b"}},
		{Outputs: []string{"", "ab"}},
		{Outputs: []string{"ab", ""}},
		{Error: "ab"},
		{Error: "a", Outputs: []string{"b"}},
	}
	seen := map[[32]byte]int{}
	for i, result := range distinct {
		if previous, ok := seen[result.digest()]; ok {
			t.Fatalf("results %d and %d share a digest", previous, i)
		}
		seen[result.digest()] = i
	}

	record := newLiveSession("live_digest", "digest-session")
	calls := []liveClientToolCall{{CallID: "c1", Name: "webmcp__ping", Arguments: "{}"}}
	if _, err := record.beginClientToolRequest("tools_1", "digest-session", "resp_1", calls); err != nil {
		t.Fatal(err)
	}
	if outcome := record.completeClientToolRequest("tools_1", liveClientToolResult{Outputs: []string{"pong"}}); outcome != liveDelegationResultAccepted {
		t.Fatalf("first result outcome = %v", outcome)
	}
	if result, _ := record.finishClientToolRequest("tools_1"); result == nil || result.Outputs[0] != "pong" {
		t.Fatalf("handed-off result = %+v", result)
	}
	// The waiter took the outputs; the digest still recognises a replay.
	if outcome := record.completeClientToolRequest("tools_1", liveClientToolResult{Outputs: []string{"pong"}}); outcome != liveDelegationResultDuplicate {
		t.Fatalf("replay outcome = %v, want a duplicate", outcome)
	}
	if outcome := record.completeClientToolRequest("tools_1", liveClientToolResult{Outputs: []string{"pong!"}}); outcome != liveDelegationResultConflict {
		t.Fatalf("differing replay outcome = %v, want a conflict", outcome)
	}
}

func TestLivePageToolRoundFailurePaths(t *testing.T) {
	start := func(t *testing.T, sessionID string) (*serveServer, *liveSession, *serveLiveDelegator) {
		srv, record, provider, delegator := newPageToolDelegation(t, sessionID, pageToolDefinition("webmcp__ping"))
		provider.AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{pageToolCall("call_ping")}})
		return srv, record, delegator
	}

	t.Run("client error", func(t *testing.T) {
		srv, record, delegator := start(t, "client-error-session")
		done, _ := runPageToolDelegation(context.Background(), delegator, "ping my phone")
		requestID, _ := requestedCalls(t, waitForRequestedToolCalls(t, record, 1)[0])
		if status, _ := postLiveToolResult(t, srv, record.id, requestID, `{"error":"Music access was denied."}`); status != http.StatusOK {
			t.Fatalf("error result status = %d", status)
		}
		if err := waitForDelegationRun(t, done); err == nil || err.Error() != "Music access was denied." {
			t.Fatalf("Run = %v, want the client's reason", err)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		_, record, delegator := start(t, "timeout-tools-session")
		delegator.toolTimeout = 20 * time.Millisecond
		done, _ := runPageToolDelegation(context.Background(), delegator, "ping my phone")
		if err := waitForDelegationRun(t, done); err == nil || !strings.Contains(err.Error(), "did not run its tools") {
			t.Fatalf("Run = %v, want a user-safe timeout", err)
		}
		requestID, _ := requestedCalls(t, requestedToolCalls(record)[0])
		var cancelled bool
		for _, event := range liveEventSnapshot(record) {
			if event.Type == liveEventToolCallsCancelled && event.Data["request_id"] == requestID {
				cancelled = true
			}
		}
		if !cancelled {
			t.Fatal("timeout did not tell the page to stop the tool round")
		}
		// A client answering after the host gave up must not be absorbed silently.
		if outcome := record.completeClientToolRequest(requestID, liveClientToolResult{Outputs: []string{"late"}}); outcome != liveDelegationResultConflict {
			t.Fatalf("late result outcome = %v, want a conflict", outcome)
		}
	})

	t.Run("resends while pending", func(t *testing.T) {
		_, record, delegator := start(t, "resend-tools-session")
		delegator.toolResendInterval = 5 * time.Millisecond
		ctx, cancel := context.WithCancel(context.Background())
		done, _ := runPageToolDelegation(ctx, delegator, "ping my phone")
		requested := waitForRequestedToolCalls(t, record, 3)
		cancel()
		if err := waitForDelegationRun(t, done); err == nil {
			t.Fatal("Run returned nil after cancellation")
		}
		first, _ := requestedCalls(t, requested[0])
		for _, event := range requested[1:] {
			if id, calls := requestedCalls(t, event); id != first || event.Data["resend"] != true || len(calls) != 1 {
				t.Fatalf("resend payload drifted: %+v", event.Data)
			}
		}
		// A retired round stops being republished.
		before := len(requestedToolCalls(record))
		record.resendClientToolRequest(first)
		if after := len(requestedToolCalls(record)); after != before {
			t.Fatalf("a retired round was republished: %d then %d", before, after)
		}
	})

	t.Run("call ended", func(t *testing.T) {
		srv, record, delegator := start(t, "ended-tools-session")
		done, _ := runPageToolDelegation(context.Background(), delegator, "ping my phone")
		requestID, _ := requestedCalls(t, waitForRequestedToolCalls(t, record, 1)[0])
		record.appendEvent(liveEventEnded, map[string]any{})
		if err := waitForDelegationRun(t, done); err == nil || !strings.Contains(err.Error(), "ended") {
			t.Fatalf("Run = %v, want the call-ended reason", err)
		}
		if status, _ := postLiveToolResult(t, srv, record.id, requestID, `{"outputs":[{"call_id":"call_ping","output":"late"}]}`); status != http.StatusConflict {
			t.Fatalf("post-hangup result status = %d, want 409", status)
		}
	})
}

// Results that arrive after another turn took the session answer calls that
// turn already dropped, so they are refused rather than spliced into its history.
func TestLivePageToolResultsAreRefusedAfterAnotherTurnTookTheSession(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		moveOn func(srv *serveServer, runtime *serveRuntime, sessionID string)
	}{
		{name: "a later turn finished", moveOn: func(srv *serveServer, runtime *serveRuntime, sessionID string) {
			srv.registerResponseID(runtime, "resp_typed_turn", sessionID)
		}},
		{name: "a later turn is running", moveOn: func(srv *serveServer, _ *serveRuntime, sessionID string) {
			srv.ensureResponseRuns().setActiveRun(sessionID, "resp_typed_turn")
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sessionID := "moved-" + strings.ReplaceAll(testCase.name, " ", "-")
			srv, record, provider, delegator := newPageToolDelegation(t, sessionID, pageToolDefinition("webmcp__ping"))
			provider.AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{pageToolCall("call_ping")}})
			provider.AddTextResponse("this continuation must not run")
			done, _ := runPageToolDelegation(context.Background(), delegator, "ping my phone")
			event := waitForRequestedToolCalls(t, record, 1)[0]
			requestID, _ := requestedCalls(t, event)
			stopped, ok := srv.ensureResponseRuns().get(fmt.Sprint(event.Data["response_id"]))
			if !ok {
				t.Fatal("the stopped run is unknown")
			}
			<-stopped.settled
			runtime, _, err := srv.runtimeForRequest(context.Background(), sessionID)
			if err != nil {
				t.Fatal(err)
			}
			testCase.moveOn(srv, runtime, sessionID)
			if status, _ := postLiveToolResult(t, srv, record.id, requestID, `{"outputs":[{"call_id":"call_ping","output":"pong"}]}`); status != http.StatusOK {
				t.Fatalf("result status = %d", status)
			}
			if err := waitForDelegationRun(t, done); !errors.Is(err, errLivePageToolsSuperseded) {
				t.Fatalf("Run = %v, want %v", err, errLivePageToolsSuperseded)
			}
			if turns := len(provider.RecordedRequests()); turns != 1 {
				t.Fatalf("model turns = %d, want no continuation", turns)
			}
		})
	}
}

func TestLivePageToolContinuationAdmissionRejectsTurnCompletedAfterPrecheck(t *testing.T) {
	srv, _, _, delegator := newPageToolDelegation(t, "admission-fence-session", pageToolDefinition("webmcp__ping"))
	const sessionID = "admission-fence-session"
	runtime, _, err := srv.runtimeForRequest(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	// A completed delegation run has a concrete continuation ID; checking an
	// empty expectation would only exercise the fallback branch of the fence.
	srv.registerResponseID(runtime, "resp_stopped_turn", sessionID)
	previous := delegator.previousResponseID(context.Background(), runtime, sessionID)
	if previous != "resp_stopped_turn" {
		t.Fatalf("previous response = %q, want the stopped turn", previous)
	}
	// Simulate a typed turn finishing after the earlier ownership check, but
	// before admission. The slot is free again; only the fenced latest check
	// can prevent this stale continuation from running.
	srv.ensureResponseRuns().setActiveRun(sessionID, "resp_typed_turn")
	srv.registerResponseID(runtime, "resp_typed_turn", sessionID)
	srv.ensureResponseRuns().clearActiveRun(sessionID, "resp_typed_turn")
	if _, err := delegator.startDelegatedRun(runtime, sessionID, previous, []llm.Message{
		llm.ToolResultMessage("call_ping", "webmcp__ping", "pong", nil),
	}, delegator.pageTools(), previous); !errors.Is(err, errServeSessionBusy) {
		t.Fatalf("continuation admission = %v, want busy after a newer turn", err)
	}
}

func TestLivePageToolRoundsAreBounded(t *testing.T) {
	srv, record, provider, delegator := newPageToolDelegation(t, "bounded-session", pageToolDefinition("webmcp__ping"))
	for round := 0; round <= liveClientToolRoundLimit; round++ {
		provider.AddTurn(llm.MockTurn{ToolCalls: []llm.ToolCall{pageToolCall(fmt.Sprintf("call_%d", round))}})
	}
	done, _ := runPageToolDelegation(context.Background(), delegator, "keep pinging")
	for round := 1; round <= liveClientToolRoundLimit; round++ {
		requestID, calls := requestedCalls(t, waitForRequestedToolCalls(t, record, round)[round-1])
		body := fmt.Sprintf(`{"outputs":[{"call_id":%q,"output":"pong"}]}`, calls[0].CallID)
		if status, _ := postLiveToolResult(t, srv, record.id, requestID, body); status != http.StatusOK {
			t.Fatalf("round %d status = %d", round, status)
		}
	}
	if err := waitForDelegationRun(t, done); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("stopped after %d rounds", liveClientToolRoundLimit)) {
		t.Fatalf("Run = %v, want the round limit", err)
	}
	if got := len(requestedToolCalls(record)); got != liveClientToolRoundLimit {
		t.Fatalf("published rounds = %d, want %d", got, liveClientToolRoundLimit)
	}
}
