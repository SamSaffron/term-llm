package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/live"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

type liveContextProbeTool struct{ calls atomic.Int32 }

func (*liveContextProbeTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "live_probe", Description: "Test probe", Schema: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (t *liveContextProbeTool) Execute(context.Context, json.RawMessage) (llm.ToolOutput, error) {
	t.calls.Add(1)
	return llm.TextOutput("probe result"), nil
}
func (*liveContextProbeTool) Preview(json.RawMessage) string { return "probe" }

func TestLiveDelegationUsesConfiguredToolsAndCleanTranscript(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "allowed", true: "denied"}[denied], func(t *testing.T) {
			s := newTestServeServer()
			rt, _, err := s.runtimeForRequest(context.Background(), "live-tools")
			if err != nil {
				t.Fatal(err)
			}
			rt.platform = "web"
			probe := &liveContextProbeTool{}
			rt.engine.RegisterTool(probe)
			if denied {
				rt.engine.SetAllowedToolsFilter([]string{})
			}
			provider := rt.provider.(*llm.MockProvider)
			provider.AddToolCall("probe-1", "live_probe", map[string]any{}).AddTextResponse("```go\npackage main\n```")
			record := newLiveSession("call", "live-tools")
			record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
			d := &serveLiveDelegator{server: s, sessionID: "live-tools", live: record}
			var output strings.Builder
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.Run(ctx, live.DelegationRequest{Input: "list the files", TranscriptDelta: "user: list the files"}, func(chunk live.DelegationChunk) {
				if chunk.Channel == live.ChannelSpeakable {
					output.WriteString(chunk.Text)
				}
			}); err != nil {
				t.Fatal(err)
			}
			run, ok := s.ensureResponseRuns().get(rt.getLastResponseID())
			if !ok {
				t.Fatal("missing response run")
			}
			select {
			case <-run.settled:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			wantCalls := int32(1)
			if denied {
				wantCalls = 0
			}
			if probe.calls.Load() != wantCalls {
				t.Fatalf("tool calls = %d, want %d", probe.calls.Load(), wantCalls)
			}
			requests := provider.RecordedRequests()
			if len(requests) != 2 {
				t.Fatalf("model turns = %d", len(requests))
			}
			if !denied {
				found := false
				for _, spec := range requests[0].Tools {
					if spec.Name == "live_probe" {
						found = true
					}
				}
				if !found {
					t.Fatal("configured tools were omitted from voice request")
				}
			}
			if !strings.Contains(output.String(), "```go") {
				t.Fatalf("visual output stripped: %s", output.String())
			}
			rt.mu.Lock()
			history := append([]llm.Message(nil), rt.history...)
			rt.mu.Unlock()
			var rows []session.Message
			for i, msg := range history {
				rows = append(rows, *session.NewMessage("live-tools", msg, i))
			}
			entries := s.sessionMessageEntries(rows)
			foundUser := false
			for _, entry := range entries {
				if entry.Role == "user" {
					foundUser = true
					if len(entry.Parts) != 1 || entry.Parts[0].Text != "list the files" {
						t.Fatalf("visible user bubble = %+v", entry)
					}
				}
			}
			if !foundUser {
				t.Fatal("no visible request")
			}
			contextMessage, ok := llm.PlatformContextFrom(history)
			if !ok || !strings.Contains(llm.MessageText(contextMessage), live.ExecutionInstructions) || !strings.Contains(llm.MessageText(contextMessage), "cove") {
				t.Fatalf("missing mode/capabilities: %+v", contextMessage)
			}
			if !requests[0].IncludeDeveloperInContinuation {
				t.Fatal("live developer transition omitted from continuation")
			}
			// The registered settings schema does not confer authority outside this run.
			control, _ := rt.engine.Tools().Get(tools.LiveSettingsToolName)
			if _, err := control.Execute(llm.ContextWithSessionID(context.Background(), "live-tools"), json.RawMessage(`{}`)); err == nil {
				t.Fatal("ordinary turn acquired live authority")
			}
			for _, spec := range rt.selectTools(nil) {
				if spec.Name == tools.LiveSettingsToolName {
					t.Fatal("live schema leaked into ordinary request")
				}
			}
		})
	}
}

func TestLiveModeContextTransitionsAndRestart(t *testing.T) {
	record := newLiveSession("call", "chat")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	provider := llm.NewMockProvider("mock")
	for range 6 {
		provider.AddTextResponse("answer")
	}
	rt := &serveRuntime{provider: provider, engine: llm.NewEngine(provider, nil), platform: "web", liveContext: record.executionContext}
	run := func() {
		t.Helper()
		if _, err := rt.Run(context.Background(), true, false, []llm.Message{llm.UserText("request")}, llm.Request{SessionID: "chat"}); err != nil {
			t.Fatal(err)
		}
	}
	count := func() int {
		n := 0
		for _, msg := range rt.history {
			if llm.IsPlatformContextMessage(msg) {
				n++
			}
		}
		return n
	}
	run()
	run()
	if count() != 1 {
		t.Fatalf("repeated mode context: %d", count())
	}
	record.appendEvent(liveEventEnded, nil)
	run()
	run()
	if count() != 2 {
		t.Fatalf("mode reset count = %d", count())
	}
	latest, _ := llm.PlatformContextFrom(rt.history)
	if llm.MessageText(latest) != liveTextModeContext {
		t.Fatalf("latest context=%q", llm.MessageText(latest))
	}
	// Restart after an active call: no callback can survive, but history does.
	rt.history = []llm.Message{llm.PlatformContextMessageForMode("Live voice mode is active. "+live.ExecutionInstructions, "live"), llm.UserText("old")}
	rt.liveContext = nil
	run()
	latest, _ = llm.PlatformContextFrom(rt.history)
	if llm.MessageText(latest) != liveTextModeContext {
		t.Fatalf("stale mode after restart: %q", llm.MessageText(latest))
	}
	rt.history = []llm.Message{llm.PlatformContextMessageForMode("previous live rules", "live"), llm.UserText("old")}
	rt.platformMessages.Web = "web presentation rules"
	run()
	latest, _ = llm.PlatformContextFrom(rt.history)
	if !strings.Contains(llm.MessageText(latest), liveTextModeContext) || !strings.Contains(llm.MessageText(latest), "web presentation rules") || llm.PlatformContextMode(latest) != "text" {
		t.Fatalf("normal platform context did not explicitly reset live mode: %+v", latest)
	}

}

type liveVoiceControlStub struct {
	voice string
	err   error
}

func (s *liveVoiceControlStub) CurrentVoice() string { return s.voice }
func (s *liveVoiceControlStub) SetVoice(_ context.Context, voice string) error {
	if s.err != nil {
		return s.err
	}
	s.voice = voice
	return nil
}

func TestLiveSettingsAreCallLocalAndReportFailures(t *testing.T) {
	record := newLiveSession("old-call", "chat")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	control := &liveVoiceControlStub{voice: "cove"}
	record.voiceSession = control
	caps, err := record.settings(context.Background(), "maple")
	if err != nil || caps.Voice != "maple" {
		t.Fatalf("settings=%+v, %v", caps, err)
	}
	if record.capabilities.Voice != "cove" {
		t.Fatal("changed configured default rather than acknowledged call state")
	}
	control.err = errors.New("provider refused")
	if _, err := record.settings(context.Background(), "spruce"); err == nil {
		t.Fatal("provider rejection hidden")
	}
	caps, err = record.settings(context.Background(), "")
	if err != nil || caps.Voice != "maple" {
		t.Fatalf("failed update changed voice: %+v, %v", caps, err)
	}
	record.appendEvent(liveEventEnded, nil)
	if _, err := record.settings(context.Background(), "cove"); err == nil {
		t.Fatal("ended call allowed control")
	}
	replacement := newLiveSession("new-call", "chat")
	replacement.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	replacement.voiceSession = &liveVoiceControlStub{voice: "cove"}
	caps, err = replacement.settings(context.Background(), "")
	if err != nil || caps.Voice != "cove" {
		t.Fatalf("old call changed replacement: %+v, %v", caps, err)
	}
	replacement.voiceSession = nil
	if _, err := replacement.settings(context.Background(), "maple"); err == nil {
		t.Fatal("unsupported provider allowed voice change")
	}
}

func TestLiveStartupContextIsBoundedVisibleHistoryAndHostFacts(t *testing.T) {
	store := newServeRuntimeTestStore()
	meta := &session.Session{ID: "history-chat", Model: "coding-model", Agent: "developer", ProjectName: "my project", CWD: "/workspace"}
	if err := store.Create(context.Background(), meta); err != nil {
		t.Fatal(err)
	}
	messages := []llm.Message{llm.SystemText("secret-system-instructions"), llm.PlatformContextMessage("private-developer-context")}
	for i := 0; i < 20; i++ {
		msg := llm.UserText("provider-wrapper-private")
		msg.DisplayText = strings.Repeat("🙂", 1024)
		messages = append(messages, msg)
	}
	messages = append(messages, llm.AssistantText("latest visible answer"))
	for i, msg := range messages {
		if err := store.AddMessage(context.Background(), meta.ID, session.NewMessage(meta.ID, msg, i)); err != nil {
			t.Fatal(err)
		}
	}
	s := &serveServer{store: store}
	opts := s.liveSessionOptions(context.Background(), meta.ID, live.ConfigCapabilities(config.LiveConfig{}))
	for _, fact := range []string{"term-llm", "cove", "coding-model", "my project", "/workspace"} {
		if !strings.Contains(opts.Context, fact) {
			t.Fatalf("missing %q: %s", fact, opts.Context)
		}
	}
	total := 0
	for _, item := range opts.InitialItems {
		total += len(item.Text)
		if len(item.Text) > 2048 || !utf8.ValidString(item.Text) {
			t.Fatal("invalid bounded item")
		}
		if strings.Contains(item.Text, "private") || strings.Contains(item.Text, "secret") {
			t.Fatalf("internal context leaked: %+v", item)
		}
	}
	if total > 8192 || len(opts.InitialItems) == 0 || len(opts.InitialItems) > 12 {
		t.Fatalf("history bounds: %d bytes/%d items", total, len(opts.InitialItems))
	}
	if opts.InitialItems[len(opts.InitialItems)-1].Text != "latest visible answer" {
		t.Fatal("history not chronological or latest item dropped")
	}
}

func TestLiveSettingsRunsThroughDelegatedEngineContext(t *testing.T) {
	s := newTestServeServer()
	rt, _, err := s.runtimeForRequest(context.Background(), "voice-settings")
	if err != nil {
		t.Fatal(err)
	}
	rt.platform = "web"
	provider := rt.provider.(*llm.MockProvider)
	provider.AddToolCall("settings", tools.LiveSettingsToolName, map[string]any{"voice": "maple"}).AddTextResponse("Selected Maple.")
	// A continuation or explicitly configured schema must not be duplicated.
	rt.engine.RegisterTool(&tools.LiveSettingsTool{})
	record := newLiveSession("call-settings", "voice-settings")
	record.capabilities = live.ConfigCapabilities(config.LiveConfig{})
	record.voiceSession = &liveVoiceControlStub{voice: "cove"}
	delegator := &serveLiveDelegator{server: s, sessionID: record.sessionID, live: record}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := delegator.Run(ctx, live.DelegationRequest{Input: "switch to Maple"}, func(live.DelegationChunk) {}); err != nil {
		t.Fatal(err)
	}
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider turns = %d", len(requests))
	}
	for _, request := range requests {
		count := 0
		for _, spec := range request.Tools {
			if spec.Name == tools.LiveSettingsToolName {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("live_settings schema count = %d", count)
		}
	}

	found := false
	for _, message := range requests[1].Messages {
		for _, part := range message.Parts {
			if result := part.ToolResult; result != nil && result.Name == tools.LiveSettingsToolName {
				found = true
				if result.IsError || !strings.Contains(result.Content, `"voice":"maple"`) {
					t.Fatalf("settings tool result = %+v", result)
				}
			}
		}
	}
	if !found {
		t.Fatal("missing live settings tool result")
	}
	caps, err := record.settings(ctx, "")
	if err != nil || caps.Voice != "maple" {
		t.Fatalf("acknowledged voice = %+v, %v", caps, err)
	}
}

func TestLiveDelegationRejectsMissingSpeech(t *testing.T) {
	d := &serveLiveDelegator{server: newTestServeServer(), sessionID: "empty"}
	if err := d.Run(context.Background(), live.DelegationRequest{}, func(live.DelegationChunk) {}); err == nil {
		t.Fatal("missing speech should not persist an internal wrapper as the user request")
	}
}

type liveSteeringGateTool struct{ entered, release chan struct{} }

func (*liveSteeringGateTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "live_gate", Description: "Block until test releases", Schema: map[string]any{"type": "object"}}
}
func (*liveSteeringGateTool) Preview(json.RawMessage) string { return "gate" }
func (t *liveSteeringGateTool) Execute(ctx context.Context, _ json.RawMessage) (llm.ToolOutput, error) {
	close(t.entered)
	select {
	case <-t.release:
		return llm.TextOutput("released"), nil
	case <-ctx.Done():
		return llm.ToolOutput{}, ctx.Err()
	}
}
func TestLiveSteeringIsVisibleBeforeConsumptionAndConsumedBySameRun(t *testing.T) {
	s := newTestServeServer()
	defer s.sessionMgr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rt, _, err := s.runtimeForRequest(ctx, "live-steering")
	if err != nil {
		t.Fatal(err)
	}
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	s.store = store
	rt.store = store
	gate := &liveSteeringGateTool{entered: make(chan struct{}), release: make(chan struct{})}
	rt.engine.RegisterTool(gate)
	provider := rt.provider.(*llm.MockProvider)
	provider.AddToolCall("gate", "live_gate", map[string]any{}).AddTextResponse("the changeset is ready")
	d := &serveLiveDelegator{server: s, sessionID: "live-steering", live: newLiveSession("call", "live-steering")}
	idle, err := d.Steer(ctx, live.DelegationRequest{ID: "idle", Input: "not running"})
	if idle || err != nil {
		t.Fatalf("idle = %v, %v", idle, err)
	}
	done := make(chan error, 1)
	go func() {
		done <- d.Run(ctx, live.DelegationRequest{Input: "review this PR"}, func(live.DelegationChunk) {})
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	run := s.ensureResponseRuns().activeRun(d.sessionID)
	if run == nil {
		t.Fatal("missing run")
	}
	request := live.DelegationRequest{ID: "correction", Input: "actually, a changeset", TranscriptDelta: "user: private voice context"}
	for range 2 {
		admitted, err := d.Steer(ctx, request)
		if !admitted || err != nil {
			t.Fatalf("admission = %v, %v", admitted, err)
		}
	}
	pending := rt.engine.ListPendingSteering()
	if len(pending) != 1 || pending[0].DisplayText != request.Input {
		t.Fatalf("pending = %+v", pending)
	}
	durable, err := store.ListPendingSteering(ctx, d.sessionID)
	if err != nil || len(durable) != 1 || durable[0].DisplayText != request.Input {
		t.Fatalf("durable pending = %+v, %v", durable, err)
	}
	id := pending[0].ID
	snapshot := run.subscribe(0)
	if snapshot.ch != nil {
		defer run.unsubscribe(snapshot.ch)
	}
	queued := 0
	for _, event := range snapshot.replay {
		if event.Event == "response.steering" {
			t.Fatal("guidance consumed while tool blocked")
		}
		if event.Event == "response.steering.queued" {
			queued++
			var payload map[string]any
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["text"] != request.Input || payload["client_message_id"] != id || strings.Contains(string(event.Data), "private voice context") {
				t.Fatalf("queued payload = %s", event.Data)
			}
		}
	}
	if queued != 1 {
		t.Fatalf("queued events = %d", queued)
	}
	close(gate.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-run.settled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if s.ensureResponseRuns().latestRun(d.sessionID) != run {
		t.Fatal("correction spawned a separate response")
	}
	requests := provider.RecordedRequests()
	if len(requests) != 2 {
		t.Fatalf("provider requests = %d", len(requests))
	}
	found := 0
	for _, msg := range requests[1].Messages {
		if msg.ClientMessageID == id {
			found++
			if !strings.Contains(llm.MessageText(msg), "private voice context") {
				t.Fatal("lost provider context")
			}
		}
	}
	if found != 1 {
		t.Fatalf("same-run guidance = %d", found)
	}
	snapshot = run.subscribe(0)
	committed := 0
	for _, event := range snapshot.replay {
		if event.Event == "response.steering" {
			committed++
			if !strings.Contains(string(event.Data), request.Input) || strings.Contains(string(event.Data), "private voice context") {
				t.Fatalf("committed payload = %s", event.Data)
			}
		}
	}
	if committed != 1 {
		t.Fatalf("committed events = %d", committed)
	}
}
