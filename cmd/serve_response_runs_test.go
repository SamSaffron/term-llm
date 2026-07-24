package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

func TestResponseRunCarriesStartedAndFinalTranscriptRevisions(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()
	sess := &session.Session{ID: "sess-run-revisions", Provider: "test", Model: "test", Mode: session.ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	server := &serveServer{store: store}
	run := newResponseRun("resp-revisions", sess.ID, "", "test", time.Now().Unix(), nil)
	server.configureResponseRunRevision(run, sess.ID)
	if run.startedRev != 0 {
		t.Fatalf("startedRev=%d before triggering row, want 0", run.startedRev)
	}
	if err := store.AddMessage(ctx, sess.ID, session.NewMessage(sess.ID, llm.UserText("trigger"), -1)); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if err := run.appendEvent("response.created", map[string]any{"response": map[string]any{"id": run.id}}); err != nil {
		t.Fatalf("append created: %v", err)
	}
	if err := run.complete(map[string]any{"response": map[string]any{"id": run.id}}, llm.Usage{}, llm.Usage{}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if len(run.events) != 2 {
		t.Fatalf("events=%d want 2", len(run.events))
	}
	var created, completed map[string]any
	if err := json.Unmarshal(run.events[0].Data, &created); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(run.events[1].Data, &completed); err != nil {
		t.Fatal(err)
	}
	if got, _ := created["started_rev"].(float64); int64(got) != 0 {
		t.Fatalf("created started_rev=%v", got)
	}
	if got, _ := completed["final_rev"].(float64); int64(got) < 1 {
		t.Fatalf("completed final_rev=%v, want >= triggering row rev", got)
	}
	snapshot := run.snapshot()
	if got, _ := snapshot["started_rev"].(int64); got != 0 {
		t.Fatalf("snapshot started_rev=%d", got)
	}
	if got, _ := snapshot["final_rev"].(int64); got < 1 {
		t.Fatalf("snapshot final_rev=%d", got)
	}
}

type deadlineTranscriptStore struct {
	*serveRuntimeTestStore
	deadlineSeen chan bool
}

func (s *deadlineTranscriptStore) GetTranscriptIndex(context.Context, string) (int64, []session.TranscriptIndexItem, error) {
	return 0, nil, nil
}

func (s *deadlineTranscriptStore) GetTranscriptSnapshot(context.Context, string) (session.TranscriptSnapshot, error) {
	return session.TranscriptSnapshot{}, nil
}

func (s *deadlineTranscriptStore) GetMessagesByTranscriptRanges(context.Context, string, []session.TranscriptRange) (int64, []session.Message, error) {
	return 0, nil, nil
}

func (s *deadlineTranscriptStore) TranscriptRev(ctx context.Context, _ string) (int64, error) {
	_, ok := ctx.Deadline()
	s.deadlineSeen <- ok
	return 7, nil
}

func TestConfigureResponseRunRevisionBoundsStartedRevisionRead(t *testing.T) {
	store := &deadlineTranscriptStore{
		serveRuntimeTestStore: newServeRuntimeTestStore(),
		deadlineSeen:          make(chan bool, 1),
	}
	run := newResponseRun("resp-started-timeout", "sess-timeout", "", "test", time.Now().Unix(), nil)
	(&serveServer{store: store}).configureResponseRunRevision(run, run.sessionID)
	if !<-store.deadlineSeen {
		t.Fatal("started revision read had no context deadline")
	}
	if run.startedRev != 7 {
		t.Fatalf("startedRev=%d, want 7", run.startedRev)
	}
}

func TestResponseRunFinalRevisionReadDoesNotBlockEventMutex(t *testing.T) {
	run := newResponseRun("resp-final-rev-lock", "sess-lock", "", "test", time.Now().Unix(), nil)
	readerStarted := make(chan struct{})
	releaseReader := make(chan struct{})
	run.finalRevReader = func() int64 {
		close(readerStarted)
		<-releaseReader
		return 11
	}

	completeDone := make(chan error, 1)
	go func() {
		completeDone <- run.complete(map[string]any{"response": map[string]any{"id": run.id}}, llm.Usage{}, llm.Usage{})
	}()
	<-readerStarted

	appendDone := make(chan error, 1)
	go func() {
		appendDone <- run.appendEvent("response.phase", map[string]any{"text": "persisting"})
	}()
	appendCompletedBeforeRead := false
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatalf("append phase: %v", err)
		}
		appendCompletedBeforeRead = true
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseReader)
	if !appendCompletedBeforeRead {
		if err := <-appendDone; err != nil {
			t.Fatalf("append phase after release: %v", err)
		}
	}
	if err := <-completeDone; err != nil {
		t.Fatalf("complete: %v", err)
	}
	if !appendCompletedBeforeRead {
		t.Fatal("final revision I/O held responseRun.mu")
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.events) != 2 || run.events[0].Event != "response.phase" || run.events[1].Event != "response.completed" {
		t.Fatalf("terminal ordering = %#v", run.events)
	}
	var payload map[string]any
	if err := json.Unmarshal(run.events[1].Data, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["final_rev"] != float64(11) {
		t.Fatalf("final_rev=%v, want 11", payload["final_rev"])
	}
}

func TestEncodeTextDeltaPayloadMatchesJSONMarshalForInvalidUTF8(t *testing.T) {
	delta := string([]byte{'o', 'k', 0xff, '!'})
	data, err := encodeTextDeltaPayload(2, delta, 7)
	if err != nil {
		t.Fatalf("encodeTextDeltaPayload() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("encoded payload is invalid JSON: %v; data=%q", err, data)
	}
	if got["delta"] != "ok�!" {
		t.Fatalf("delta = %q, want replacement-char-normalized string", got["delta"])
	}
	if got["output_index"] != float64(2) || got["sequence_number"] != float64(7) {
		t.Fatalf("payload = %#v, want output_index=2 sequence_number=7", got)
	}
}

func TestResponseRunAppendEventMarshalErrorDoesNotBurnSequence(t *testing.T) {
	run := newResponseRun("resp_marshal_gap", "sess_test", "", "mock", time.Now().Unix(), func() {})

	if err := run.appendEvent("response.created", map[string]any{"ok": true}); err != nil {
		t.Fatalf("append initial event: %v", err)
	}
	if err := run.appendEvent("response.bad", map[string]any{"bad": math.Inf(1)}); err == nil {
		t.Fatal("appendEvent with non-marshalable payload succeeded, want error")
	}
	if err := run.appendEvent("response.phase", map[string]any{"text": "after"}); err != nil {
		t.Fatalf("append event after marshal error: %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	if run.lastSequenceNumber != 2 {
		t.Fatalf("lastSequenceNumber = %d, want 2", run.lastSequenceNumber)
	}
	if len(run.events) != 2 {
		t.Fatalf("events = %d, want 2", len(run.events))
	}
	for i, ev := range run.events {
		want := int64(i + 1)
		if ev.Sequence != want {
			t.Fatalf("event[%d].Sequence = %d, want %d", i, ev.Sequence, want)
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("event[%d] payload unmarshal: %v", i, err)
		}
		if payload["sequence_number"] != float64(want) {
			t.Fatalf("event[%d] payload sequence_number = %#v, want %d", i, payload["sequence_number"], want)
		}
	}
}

func TestAppendResponseRunEventEmitsPhase(t *testing.T) {
	run := newResponseRun("resp_phase", "sess_test", "", "mock", time.Now().Unix(), func() {})
	server := &serveServer{}
	state := &responseRunStreamState{}

	if err := server.appendResponseRunEvent(nil, run, state, llm.Event{Type: llm.EventPhase, Text: "Compacting context..."}); err != nil {
		t.Fatalf("appendResponseRunEvent() error = %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.events) != 1 {
		t.Fatalf("events = %d, want 1", len(run.events))
	}
	ev := run.events[0]
	if ev.Event != "response.phase" {
		t.Fatalf("event name = %q, want response.phase", ev.Event)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["text"] != "Compacting context..." {
		t.Fatalf("payload text = %#v", payload["text"])
	}
}

func TestAppendResponseRunEventEmitsRetry(t *testing.T) {
	run := newResponseRun("resp_retry", "sess_test", "", "mock", time.Now().Unix(), func() {})
	server := &serveServer{}
	state := &responseRunStreamState{}

	if err := server.appendResponseRunEvent(nil, run, state, llm.Event{
		Type:             llm.EventRetry,
		RetryAttempt:     2,
		RetryMaxAttempts: 6,
		RetryWaitSecs:    0.5,
	}); err != nil {
		t.Fatalf("appendResponseRunEvent() error = %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.events) != 1 {
		t.Fatalf("events = %d, want 1", len(run.events))
	}
	ev := run.events[0]
	if ev.Event != "response.retry" {
		t.Fatalf("event name = %q, want response.retry", ev.Event)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["attempt"] != float64(2) || payload["max_attempts"] != float64(6) || payload["wait_seconds"] != 0.5 {
		t.Fatalf("payload retry fields = %#v", payload)
	}
	if !strings.Contains(payload["message"].(string), "reconnecting (2/6)") {
		t.Fatalf("payload message = %#v", payload["message"])
	}
}

func TestPersistResponseRunErrorEventStoresDurableNonLLMMarker(t *testing.T) {
	store := newServeRuntimeTestStore()
	runtime := &serveRuntime{store: store}
	server := &serveServer{}

	server.persistResponseRunErrorEvent(runtime, "sess_error", "resp_error", "timeout_error", "Responses WebSocket retries exhausted")

	store.mu.Lock()
	messages := append([]session.Message(nil), store.messages["sess_error"]...)
	store.mu.Unlock()
	if len(messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(messages))
	}
	msg := messages[0]
	if msg.Role != llm.RoleEvent {
		t.Fatalf("role = %q, want event", msg.Role)
	}
	marker, ok := llm.ParseRunErrorMarker(msg.ToLLMMessage())
	if !ok {
		t.Fatalf("missing run error marker in %#v", msg.Parts)
	}
	if marker.ResponseID != "resp_error" || marker.ErrorType != "timeout_error" || marker.Message != "Responses WebSocket retries exhausted" {
		t.Fatalf("marker = %#v", marker)
	}
	filtered := llm.FilterConversationMessages([]llm.Message{msg.ToLLMMessage(), llm.UserText("continue")})
	if len(filtered) != 1 || filtered[0].Role != llm.RoleUser {
		t.Fatalf("filtered messages = %#v, want only user message", filtered)
	}
}

func TestAppendResponseRunEventEmitsModelSwitchAndUpdatesSnapshot(t *testing.T) {
	run := newResponseRun("resp_model_switch", "sess_test", "", "gpt-5.4", time.Now().Unix(), func() {})
	server := &serveServer{}
	state := newResponseRunStreamState("gpt-5.4", "medium")

	if err := server.appendResponseRunEvent(nil, run, state, llm.Event{
		Type:            llm.EventModelSwitch,
		Model:           "gpt-5.4",
		ReasoningEffort: "high",
	}); err != nil {
		t.Fatalf("appendResponseRunEvent() error = %v", err)
	}

	run.mu.Lock()
	if len(run.events) != 1 {
		run.mu.Unlock()
		t.Fatalf("events = %d, want 1", len(run.events))
	}
	ev := run.events[0]
	run.mu.Unlock()
	if ev.Event != "response.model_switch" {
		t.Fatalf("event name = %q, want response.model_switch", ev.Event)
	}
	var payload map[string]any
	if err := json.Unmarshal(ev.Data, &payload); err != nil {
		t.Fatalf("payload unmarshal: %v", err)
	}
	if payload["model"] != "gpt-5.4" || payload["reasoning_effort"] != "high" {
		t.Fatalf("payload runtime = %#v/%#v, want gpt-5.4/high", payload["model"], payload["reasoning_effort"])
	}
	if state.model != "gpt-5.4" || state.reasoningEffort != "high" || !state.reasoningEffortSet {
		t.Fatalf("stream state = %#v, want gpt-5.4/high set", state)
	}
	snapshot := run.snapshot()
	if snapshot["model"] != "gpt-5.4" || snapshot["reasoning_effort"] != "high" {
		t.Fatalf("snapshot runtime = %#v/%#v, want gpt-5.4/high", snapshot["model"], snapshot["reasoning_effort"])
	}
}

func TestAppendResponseRunEventSuppressesServerToolMetadata(t *testing.T) {
	registry := llm.NewToolRegistry()
	registry.Register(&echoTool{})
	runtime := &serveRuntime{engine: llm.NewEngine(llm.NewMockProvider("mock"), registry)}
	server := &serveServer{cfg: serveServerConfig{suppressServerTools: true}}
	run := newResponseRun("resp_suppress_tools", "sess_test", "", "mock", time.Now().Unix(), func() {})
	state := &responseRunStreamState{}

	if err := server.appendResponseRunEvent(runtime, run, state, llm.Event{
		Type:       llm.EventToolExecStart,
		ToolCallID: "call-1",
		ToolName:   "echo",
		ToolArgs:   json.RawMessage(`{"path":"/secret/file.txt"}`),
	}); err != nil {
		t.Fatalf("append start: %v", err)
	}
	if err := server.appendResponseRunEvent(runtime, run, state, llm.Event{
		Type:        llm.EventToolExecEnd,
		ToolCallID:  "call-1",
		ToolName:    "echo",
		ToolSuccess: true,
		ToolFileChanges: []llm.FileChange{{
			Path: "/secret/file.txt",
			Kind: "modify",
			Seq:  1,
		}},
	}); err != nil {
		t.Fatalf("append end: %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	if len(run.events) != 0 {
		var names []string
		for _, ev := range run.events {
			names = append(names, ev.Event)
		}
		t.Fatalf("events = %v, want none for suppressed server tool", names)
	}
}

func TestAppendResponseRunEventKeepsClientToolFileChanges(t *testing.T) {
	registry := llm.NewToolRegistry()
	runtime := &serveRuntime{engine: llm.NewEngine(llm.NewMockProvider("mock"), registry)}
	server := &serveServer{cfg: serveServerConfig{suppressServerTools: true}}
	run := newResponseRun("resp_client_tool", "sess_test", "", "mock", time.Now().Unix(), func() {})
	state := &responseRunStreamState{}

	if err := server.appendResponseRunEvent(runtime, run, state, llm.Event{
		Type:        llm.EventToolExecEnd,
		ToolCallID:  "call-client",
		ToolName:    "client_tool",
		ToolSuccess: true,
		ToolFileChanges: []llm.FileChange{{
			Path: "/client/file.txt",
			Kind: "modify",
			Seq:  1,
		}},
	}); err != nil {
		t.Fatalf("append end: %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()
	var sawFileChange bool
	for _, ev := range run.events {
		if ev.Event == "response.file_change" {
			sawFileChange = true
		}
	}
	if !sawFileChange {
		t.Fatalf("events = %+v, want response.file_change for non-server tool", run.events)
	}
}

func TestStreamResponseRunEventsReportsAuthoritativeReplayBoundary(t *testing.T) {
	run := newResponseRun("resp_replay_boundary", "sess_replay_boundary", "", "mock", time.Now().Unix(), func() {})
	if err := run.appendEvent("response.created", map[string]any{"response": map[string]any{"id": run.id}}); err != nil {
		t.Fatalf("append created event: %v", err)
	}
	if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "historical"}); err != nil {
		t.Fatalf("append text event: %v", err)
	}
	run.mu.Lock()
	run.status = "completed"
	run.mu.Unlock()

	for _, tc := range []struct {
		name  string
		after int64
		want  string
	}{
		{name: "replayed events", after: 0, want: "2"},
		{name: "empty replay", after: 2, want: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			server := &serveServer{}
			server.streamResponseRunEvents(context.Background(), recorder, run, tc.after)
			response := recorder.Result()
			defer response.Body.Close()
			if got := response.Header.Get("X-Term-LLM-Replay-Through"); got != tc.want {
				t.Fatalf("X-Term-LLM-Replay-Through = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponseRunSubscriberSurvivesUpToBufferLimit(t *testing.T) {
	run := newResponseRun("resp_test1", "sess_test", "", "mock", time.Now().Unix(), func() {})

	sub := run.subscribe(0)
	if sub.ch == nil {
		t.Fatal("expected live channel from subscribe")
	}
	defer run.unsubscribe(sub.ch)

	// Fill the subscriber buffer up to the limit (should not drop)
	for i := 0; i < defaultResponseRunSubscriberBuffer; i++ {
		err := run.appendEvent("response.output_text.delta", map[string]any{
			"delta": "x",
		})
		if err != nil {
			t.Fatalf("appendEvent failed at %d: %v", i, err)
		}
	}

	// Subscriber should still be registered
	run.mu.Lock()
	subCount := len(run.subscribers)
	run.mu.Unlock()

	if subCount != 1 {
		t.Fatalf("expected 1 subscriber, got %d", subCount)
	}

	// Drain all events from the channel
	for i := 0; i < defaultResponseRunSubscriberBuffer; i++ {
		select {
		case ev := <-sub.ch:
			if ev.Sequence != int64(i+1) {
				t.Fatalf("expected sequence %d, got %d", i+1, ev.Sequence)
			}
		default:
			t.Fatalf("expected event at index %d but channel was empty", i)
		}
	}
}

func TestResponseRunSubscriberDroppedWhenBufferFull(t *testing.T) {
	run := newResponseRun("resp_test2", "sess_test", "", "mock", time.Now().Unix(), func() {})

	sub := run.subscribe(0)
	if sub.ch == nil {
		t.Fatal("expected live channel from subscribe")
	}
	defer run.unsubscribe(sub.ch)

	// Fill buffer completely
	for i := 0; i < defaultResponseRunSubscriberBuffer; i++ {
		if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "x"}); err != nil {
			t.Fatalf("appendEvent failed at %d: %v", i, err)
		}
	}

	// One more should drop the subscriber
	if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "overflow"}); err != nil {
		t.Fatalf("appendEvent failed on overflow: %v", err)
	}

	run.mu.Lock()
	subCount := len(run.subscribers)
	run.mu.Unlock()
	if subCount != 0 {
		t.Fatalf("expected 0 subscribers after overflow, got %d", subCount)
	}
}

func TestResponseRunCompleteHonorsPendingCancellation(t *testing.T) {
	run := newResponseRun("resp_cancelled", "sess_test", "", "mock", time.Now().Unix(), func() {})

	if !run.cancelRun() {
		t.Fatal("expected cancelRun to succeed")
	}

	if err := run.complete(map[string]any{
		"response": map[string]any{
			"id":            run.id,
			"object":        "response",
			"created":       run.created,
			"model":         run.model,
			"status":        "completed",
			"usage":         usagePayload(llm.Usage{InputTokens: 3, OutputTokens: 4}),
			"session_usage": usagePayload(llm.Usage{InputTokens: 5, OutputTokens: 6}),
		},
	}, llm.Usage{InputTokens: 3, OutputTokens: 4}, llm.Usage{InputTokens: 5, OutputTokens: 6}); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	run.mu.Lock()
	defer run.mu.Unlock()

	if run.status != "cancelled" {
		t.Fatalf("run status = %q, want cancelled", run.status)
	}
	if run.cancelRequested {
		t.Fatal("cancelRequested should be cleared after terminal transition")
	}
	if len(run.events) != 1 {
		t.Fatalf("events = %d, want 1", len(run.events))
	}
	if run.events[0].Event != "response.cancelled" {
		t.Fatalf("event = %q, want response.cancelled", run.events[0].Event)
	}

	var payload map[string]any
	if err := json.Unmarshal(run.events[0].Data, &payload); err != nil {
		t.Fatalf("unmarshal terminal payload: %v", err)
	}
	response, ok := payload["response"].(map[string]any)
	if !ok {
		t.Fatalf("response payload type = %T", payload["response"])
	}
	if response["status"] != "cancelled" {
		t.Fatalf("response status = %v, want cancelled", response["status"])
	}
	if _, ok := response["usage"]; ok {
		t.Fatalf("cancelled payload unexpectedly retained usage: %#v", response["usage"])
	}
	if _, ok := response["session_usage"]; ok {
		t.Fatalf("cancelled payload unexpectedly retained session_usage: %#v", response["session_usage"])
	}
}

func TestResponseRunCompactionKeepsReplayWindowInOrder(t *testing.T) {
	run := newResponseRun("resp_compact", "sess_test", "", "mock", time.Now().Unix(), func() {})
	run.maxRetainedEvents = 3

	for i := 0; i < 8; i++ {
		if err := run.appendTextDeltaEvent(0, ""); err != nil {
			t.Fatalf("appendTextDeltaEvent failed at %d: %v", i, err)
		}
	}

	run.mu.Lock()
	activeLen := len(run.events) - run.eventStart
	storageLen := len(run.events)
	minReplayAfter := run.minReplayAfter
	run.mu.Unlock()

	if activeLen != 3 {
		t.Fatalf("active retained events = %d, want 3", activeLen)
	}
	if storageLen > 6 {
		t.Fatalf("storage length = %d, want bounded near replay window", storageLen)
	}
	if minReplayAfter != 5 {
		t.Fatalf("minReplayAfter = %d, want 5", minReplayAfter)
	}

	stale := run.subscribe(4)
	if !stale.snapshotRequired || stale.minReplayAfter != 5 {
		t.Fatalf("subscribe before replay window = %#v, want snapshot required at 5", stale)
	}

	fresh := run.subscribe(5)
	if fresh.snapshotRequired {
		t.Fatalf("subscribe at replay window unexpectedly required snapshot: %#v", fresh)
	}
	if len(fresh.replay) != 3 {
		t.Fatalf("replay length = %d, want 3", len(fresh.replay))
	}
	for i, ev := range fresh.replay {
		expected := int64(i + 6)
		if ev.Sequence != expected {
			t.Fatalf("replay[%d].Sequence = %d, want %d", i, ev.Sequence, expected)
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			t.Fatalf("replay[%d] payload is invalid JSON: %v", i, err)
		}
		if payload["sequence_number"] != float64(expected) {
			t.Fatalf("replay[%d] payload sequence_number = %v, want %d", i, payload["sequence_number"], expected)
		}
	}
	if fresh.ch != nil {
		run.unsubscribe(fresh.ch)
	}
}

func TestResponseRunRecoveryPreservesToolExecutionResults(t *testing.T) {
	run := newResponseRun("resp_plan_results", "sess_test", "", "mock", time.Now().Unix(), func() {})
	for _, tc := range []struct {
		callID  string
		success bool
	}{
		{callID: "call_plan_ok", success: true},
		{callID: "call_plan_failed", success: false},
	} {
		if err := run.appendEvent("response.output_item.added", map[string]any{
			"item": map[string]any{
				"type":      "function_call",
				"call_id":   tc.callID,
				"name":      "update_plan",
				"arguments": `{"plan":[]}`,
			},
		}); err != nil {
			t.Fatalf("append %s tool call: %v", tc.callID, err)
		}
		if err := run.appendEvent("response.tool_exec.end", map[string]any{
			"call_id":   tc.callID,
			"tool_name": "update_plan",
			"success":   tc.success,
		}); err != nil {
			t.Fatalf("append %s tool end: %v", tc.callID, err)
		}
	}

	recovery := run.recoveryPayloadLocked()
	messages, ok := recovery["messages"].([]map[string]any)
	if !ok || len(messages) != 1 {
		t.Fatalf("recovery messages = %#v", recovery["messages"])
	}
	tools, ok := messages[0]["tools"].([]map[string]any)
	if !ok || len(tools) != 2 {
		t.Fatalf("recovery tools = %#v", messages[0]["tools"])
	}
	if tools[0]["status"] != "done" || tools[0]["resultStatus"] != "success" {
		t.Fatalf("successful recovered tool = %#v", tools[0])
	}
	if tools[1]["status"] != "error" || tools[1]["resultStatus"] != "error" {
		t.Fatalf("failed recovered tool = %#v", tools[1])
	}
	if messages[0]["status"] != "done" {
		t.Fatalf("recovered group status = %v, want done", messages[0]["status"])
	}
}

func TestResponseRunRecoveryStoresToolImagesAsArtifacts(t *testing.T) {
	run := newResponseRun("resp_images", "sess_test", "", "mock", time.Now().Unix(), func() {})

	if err := run.appendEvent("response.output_item.added", map[string]any{
		"item": map[string]any{
			"type":      "function_call",
			"call_id":   "call_img",
			"name":      "image_generate",
			"arguments": `{"prompt":"cat"}`,
		},
	}); err != nil {
		t.Fatalf("append tool call: %v", err)
	}
	if err := run.appendEvent("response.tool_exec.end", map[string]any{
		"call_id":   "call_img",
		"tool_name": "image_generate",
		"success":   true,
		"images":    []string{"/ui/images/generated.png"},
	}); err != nil {
		t.Fatalf("append tool end: %v", err)
	}

	recovery := run.recoveryPayloadLocked()
	messages, ok := recovery["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("recovery messages type = %T", recovery["messages"])
	}
	if len(messages) != 1 {
		t.Fatalf("recovery message count = %d, want only tool-group", len(messages))
	}
	if messages[0]["role"] != "tool-group" {
		t.Fatalf("recovery role = %v, want tool-group", messages[0]["role"])
	}
	tools, ok := messages[0]["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("recovery tools = %#v, want one tool", messages[0]["tools"])
	}
	images, ok := tools[0]["images"].([]string)
	if !ok || len(images) != 1 || images[0] != "/ui/images/generated.png" {
		t.Fatalf("tool images = %#v", tools[0]["images"])
	}
	if _, hasContent := messages[0]["content"]; hasContent {
		t.Fatalf("tool artifact should not be injected as assistant markdown: %#v", messages[0]["content"])
	}
}

func TestResponseRunInteractiveRecoveryDoesNotDisableCompaction(t *testing.T) {
	run := newResponseRun("resp_interactive", "sess_test", "", "mock", time.Now().Unix(), func() {})
	run.maxRetainedEvents = 3

	if err := run.appendEvent("response.ask_user.prompt", map[string]any{
		"call_id": "call_ask",
		"questions": []any{
			map[string]any{"header": "Color", "question": "Pick one"},
		},
	}); err != nil {
		t.Fatalf("append ask_user prompt: %v", err)
	}
	if err := run.appendEvent("response.approval.prompt", map[string]any{
		"approval_id": "appr_1",
		"path":        "/tmp/file.txt",
		"options": []any{
			map[string]any{"index": 0, "choice": "once"},
		},
	}); err != nil {
		t.Fatalf("append approval prompt: %v", err)
	}

	promptRecovery := run.recoveryPayloadLocked()
	promptEvents, ok := promptRecovery["events"].([]map[string]any)
	if !ok {
		t.Fatalf("prompt recovery events type = %T", promptRecovery["events"])
	}
	if len(promptEvents) != 2 {
		t.Fatalf("prompt recovery event count = %d, want 2", len(promptEvents))
	}

	for i := 0; i < 6; i++ {
		if err := run.appendTextDeltaEvent(0, "x"); err != nil {
			t.Fatalf("appendTextDeltaEvent failed at %d: %v", i, err)
		}
	}

	run.mu.Lock()
	activeLen := len(run.events) - run.eventStart
	storageLen := len(run.events)
	minReplayAfter := run.minReplayAfter
	run.mu.Unlock()

	if activeLen != 3 {
		t.Fatalf("active retained events = %d, want 3", activeLen)
	}
	if storageLen > 6 {
		t.Fatalf("storage length = %d, want bounded near replay window", storageLen)
	}
	if minReplayAfter != 5 {
		t.Fatalf("minReplayAfter = %d, want 5", minReplayAfter)
	}

	recovery := run.recoveryPayloadLocked()
	events, ok := recovery["events"].([]map[string]any)
	if !ok {
		t.Fatalf("recovery events type = %T", recovery["events"])
	}
	if len(events) != 2 {
		t.Fatalf("recovery event count = %d, want 2", len(events))
	}
	if events[0]["event"] != "response.ask_user.prompt" {
		t.Fatalf("recovery events[0] = %#v, want ask_user prompt", events[0])
	}
	payload, ok := events[0]["payload"].(map[string]any)
	if !ok || payload["call_id"] != "call_ask" {
		t.Fatalf("ask_user recovery payload = %#v", events[0]["payload"])
	}
	if events[1]["event"] != "response.approval.prompt" {
		t.Fatalf("recovery events[1] = %#v, want approval prompt", events[1])
	}
}

func TestResponseRunInterjectionSplitsRecoveryMessages(t *testing.T) {
	run := newResponseRun("resp_interjection", "sess_test", "", "mock", time.Now().Unix(), func() {})

	if err := run.appendTextDeltaEvent(0, "before"); err != nil {
		t.Fatalf("appendTextDeltaEvent before: %v", err)
	}
	if err := run.appendEvent("response.interjection", map[string]any{"text": "check X"}); err != nil {
		t.Fatalf("append interjection: %v", err)
	}
	if err := run.appendTextDeltaEvent(0, "after"); err != nil {
		t.Fatalf("appendTextDeltaEvent after: %v", err)
	}

	recovery := run.recoveryPayloadLocked()
	messages, ok := recovery["messages"].([]map[string]any)
	if !ok {
		t.Fatalf("recovery messages type = %T", recovery["messages"])
	}
	if len(messages) != 3 {
		t.Fatalf("recovery message count = %d, want 3", len(messages))
	}
	if got := messages[0]["role"]; got != "assistant" {
		t.Fatalf("messages[0].role = %v, want assistant", got)
	}
	if got := messages[0]["content"]; got != "before" {
		t.Fatalf("messages[0].content = %v, want before", got)
	}
	if got := messages[1]["role"]; got != "user" {
		t.Fatalf("messages[1].role = %v, want user", got)
	}
	if got := messages[1]["content"]; got != "check X" {
		t.Fatalf("messages[1].content = %v, want check X", got)
	}
	if got := messages[1]["interruptState"]; got != "interject" {
		t.Fatalf("messages[1].interruptState = %v, want interject", got)
	}
	atts := []map[string]any{{"name": "image 1", "type": "image/png"}}
	if err := run.appendEvent("response.interjection", map[string]any{"text": "see image", "interjection_id": "img-1", "attachments": atts}); err != nil {
		t.Fatalf("append image interjection: %v", err)
	}
	recovery = run.recoveryPayloadLocked()
	messages = recovery["messages"].([]map[string]any)
	last := messages[len(messages)-1]
	if got := last["id"]; got != "img-1" {
		t.Fatalf("image interjection id = %v, want img-1", got)
	}
	gotAtts, ok := last["attachments"].([]map[string]any)
	if !ok || len(gotAtts) != 1 || gotAtts[0]["type"] != "image/png" {
		t.Fatalf("image interjection attachments = %#v, want image/png", last["attachments"])
	}
	if got := messages[2]["role"]; got != "assistant" {
		t.Fatalf("messages[2].role = %v, want assistant", got)
	}
	if got := messages[2]["content"]; got != "after" {
		t.Fatalf("messages[2].content = %v, want after", got)
	}
}

func TestResponseRunResolvedInteractivePromptIsRemovedFromRecovery(t *testing.T) {
	run := newResponseRun("resp_resolve", "sess_test", "", "mock", time.Now().Unix(), func() {})
	if err := run.appendEvent("response.ask_user.prompt", map[string]any{"call_id": "ask_1"}); err != nil {
		t.Fatalf("append ask prompt: %v", err)
	}
	if err := run.appendEvent("response.approval.prompt", map[string]any{"approval_id": "approval_1"}); err != nil {
		t.Fatalf("append approval prompt: %v", err)
	}

	run.resolveAskUserRecovery("ask_1")
	recovery := run.recoveryPayloadLocked()
	events, ok := recovery["events"].([]map[string]any)
	if !ok || len(events) != 1 {
		t.Fatalf("events after ask resolution = %#v, want only approval prompt", recovery["events"])
	}
	if events[0]["event"] != "response.approval.prompt" {
		t.Fatalf("remaining event = %#v, want approval prompt", events[0])
	}

	run.resolveApprovalRecovery("approval_1")
	recovery = run.recoveryPayloadLocked()
	if _, ok := recovery["events"]; ok {
		t.Fatalf("events after approval resolution = %#v, want none", recovery["events"])
	}
}

func TestResponseRunConcurrentAppendsPreserveOrder(t *testing.T) {
	const totalEvents = 200
	const numWriters = 4
	eventsPerWriter := totalEvents / numWriters

	run := newResponseRun("resp_order", "sess_test", "", "mock", time.Now().Unix(), func() {})

	sub := run.subscribe(0)
	if sub.ch == nil {
		t.Fatal("expected live channel from subscribe")
	}
	defer run.unsubscribe(sub.ch)

	// Launch concurrent writers, each appending eventsPerWriter events.
	var wg sync.WaitGroup
	for w := 0; w < numWriters; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < eventsPerWriter; i++ {
				_ = run.appendEvent("response.output_text.delta", map[string]any{"delta": "x"})
			}
		}()
	}

	// Collect all events from subscriber in a separate goroutine.
	received := make([]int64, 0, totalEvents)
	done := make(chan struct{})
	go func() {
		for ev := range sub.ch {
			received = append(received, ev.Sequence)
			if len(received) >= totalEvents {
				break
			}
		}
		close(done)
	}()

	wg.Wait()

	// Wait for all events to arrive (or timeout)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for events, got %d/%d", len(received), totalEvents)
	}

	// Verify strictly increasing sequence numbers
	for i := 1; i < len(received); i++ {
		if received[i] <= received[i-1] {
			t.Fatalf("out of order at index %d: seq %d followed by %d", i, received[i-1], received[i])
		}
	}

	// Verify no gaps
	for i, seq := range received {
		expected := int64(i + 1)
		if seq != expected {
			t.Fatalf("gap at index %d: expected seq %d, got %d", i, expected, seq)
		}
	}
}

type blockingResponseWriter struct {
	header http.Header
	gate   <-chan struct{}
	buf    bytes.Buffer
}

func (w *blockingResponseWriter) Header() http.Header {
	return w.header
}

func (w *blockingResponseWriter) WriteHeader(statusCode int) {}

func (w *blockingResponseWriter) Write(p []byte) (int, error) {
	<-w.gate
	return w.buf.Write(p)
}

func (w *blockingResponseWriter) Flush() {}

func waitForResponseRunCondition(t *testing.T, timeout time.Duration, fn func() bool, message string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(message)
}

func TestStreamResponseRunEventsWritesTerminalErrorWhenSubscriberOverflows(t *testing.T) {
	srv := &serveServer{shutdownCh: make(chan struct{})}
	run := newResponseRun("resp_overflow", "sess_test", "", "mock", time.Now().Unix(), func() {})
	gate := make(chan struct{})
	w := &blockingResponseWriter{header: make(http.Header), gate: gate}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	streamDone := make(chan struct{})
	go func() {
		srv.streamResponseRunEvents(ctx, w, run, 0)
		close(streamDone)
	}()

	waitForResponseRunCondition(t, time.Second, func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return len(run.subscribers) == 1
	}, "timed out waiting for stream subscriber")

	for i := 0; i < defaultResponseRunSubscriberBuffer+16; i++ {
		if err := run.appendEvent("response.output_text.delta", map[string]any{"delta": "x"}); err != nil {
			t.Fatalf("appendEvent failed at %d: %v", i, err)
		}
	}

	waitForResponseRunCondition(t, time.Second, func() bool {
		run.mu.Lock()
		defer run.mu.Unlock()
		return len(run.subscribers) == 0
	}, "timed out waiting for subscriber drop")

	if err := run.complete(map[string]any{
		"response": map[string]any{"id": run.id},
	}, llm.Usage{}, llm.Usage{}); err != nil {
		t.Fatalf("complete failed: %v", err)
	}

	close(gate)

	select {
	case <-streamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for overflowed stream to finish")
	}

	body := w.buf.String()
	if !strings.Contains(body, "event: response.output_text.delta\n") {
		t.Fatalf("stream body missing replayed delta events: %q", body)
	}
	streamErrorIndex := strings.Index(body, "event: response.stream_error\n")
	if streamErrorIndex < 0 {
		t.Fatalf("overflowed stream missing terminal stream_error event: %q", body)
	}
	if !strings.Contains(body, `"type":"stream_buffer_overflow"`) {
		t.Fatalf("overflowed stream missing structured overflow error: %q", body)
	}
	if !strings.Contains(body, `"min_replay_after"`) || !strings.Contains(body, `"recovery"`) {
		t.Fatalf("overflowed stream missing recovery fields: %q", body)
	}
	doneIndex := strings.Index(body, "data: [DONE]\n\n")
	if doneIndex < 0 {
		t.Fatalf("overflowed stream should emit [DONE], got: %q", body)
	}
	if streamErrorIndex > doneIndex {
		t.Fatalf("stream_error should be emitted before [DONE], got: %q", body)
	}
}
