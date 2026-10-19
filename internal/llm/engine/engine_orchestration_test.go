package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type mockTool struct {
	name   string
	result string
	err    error
}

func (m *mockTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: m.name}
}

func (m *mockTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	if m.err != nil {
		return core.ToolOutput{}, m.err
	}
	return core.TextOutput(m.result), nil
}

func (m *mockTool) Preview(args json.RawMessage) string {
	return ""
}

func eventTypes(events []core.Event) []core.EventType {
	types := make([]core.EventType, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	return types
}

type guardianSyncTool struct {
	err error
}

func (t guardianSyncTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "test_tool", Schema: map[string]any{"type": "object"}}
}
func (t guardianSyncTool) Execute(ctx context.Context, _ json.RawMessage) (core.ToolOutput, error) {
	core.RecordGuardianReview(ctx, core.GuardianReview{Outcome: "denied", Message: "guardian: denied", Command: "rm -rf build"})
	return core.TextOutput("blocked"), t.err
}
func (guardianSyncTool) Preview(json.RawMessage) string { return "" }

type inlineSyncToolProvider struct {
	inline      bool
	ordered     bool
	calls       int
	models      []string
	leadingText string
}

func (p *inlineSyncToolProvider) Name() string       { return "inline-sync-test" }
func (p *inlineSyncToolProvider) Credential() string { return "test" }
func (p *inlineSyncToolProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, InlineToolLoop: p.inline, OrderedInlineToolEvents: p.ordered}
}
func (p *inlineSyncToolProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.calls++
	p.models = append(p.models, req.Model)
	call := p.calls
	return corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		if len(req.Tools) == 0 {
			if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "pure text"}); err != nil {
				return err
			}
			return send.Send(core.Event{Type: core.EventDone})
		}
		if call == 1 {
			if p.leadingText != "" {
				if err := send.Send(core.Event{Type: core.EventTextDelta, Text: p.leadingText}); err != nil {
					return err
				}
			}
			response := make(chan core.ToolExecutionResponse, 1)
			if err := send.Send(core.Event{
				Type:         core.EventToolCall,
				ToolCallID:   "sync-1",
				ToolName:     "test_tool",
				Tool:         &core.ToolCall{ID: "sync-1", Name: "test_tool", Arguments: json.RawMessage(`{}`)},
				ToolResponse: response,
			}); err != nil {
				return err
			}
			select {
			case <-response:
			case <-ctx.Done():
				return ctx.Err()
			}
			if p.inline {
				if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "inline final"}); err != nil {
					return err
				}
			}
		} else {
			if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "continued final"}); err != nil {
				return err
			}
		}
		return send.Send(core.Event{Type: core.EventDone})
	}), nil
}

type inlineDynamicToolProvider struct {
	published chan core.ToolSpec
	calls     int
}

func (p *inlineDynamicToolProvider) Name() string       { return "inline-dynamic-test" }
func (p *inlineDynamicToolProvider) Credential() string { return "test" }
func (p *inlineDynamicToolProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, InlineToolLoop: true}
}
func (p *inlineDynamicToolProvider) PublishDynamicTools(tools []core.ToolSpec) error {
	for _, tool := range tools {
		p.published <- tool
	}
	return nil
}
func (p *inlineDynamicToolProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.calls++
	call := p.calls
	return corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		if call > 1 {
			if !hasToolNamed(req.Tools, "late_tool") {
				return errors.New("refreshed inline turn is missing late_tool")
			}
			foundContinuation := false
			for _, message := range req.Messages {
				if core.MessageText(message) == dynamicToolContinuationPrompt {
					foundContinuation = true
					break
				}
			}
			if !foundContinuation {
				return errors.New("refreshed inline turn is missing continuation prompt")
			}
			if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "dynamic ready"}); err != nil {
				return err
			}
			return send.Send(core.Event{Type: core.EventDone})
		}
		response := make(chan core.ToolExecutionResponse, 1)
		if err := send.Send(core.Event{
			Type:         core.EventToolCall,
			ToolCallID:   "activate",
			ToolName:     "activate_dynamic",
			Tool:         &core.ToolCall{ID: "activate", Name: "activate_dynamic", Arguments: json.RawMessage(`{}`)},
			ToolResponse: response,
		}); err != nil {
			return err
		}
		select {
		case result := <-response:
			if result.Err != nil {
				return result.Err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case published := <-p.published:
			if published.Name != "late_tool" {
				return fmt.Errorf("published tool = %q, want late_tool", published.Name)
			}
		default:
			return errors.New("dynamic tool was not published before inline execution resumed")
		}
		return send.Send(core.Event{Type: core.EventDone})
	}), nil
}

type dynamicActivationTool struct {
	engine *Engine
	late   core.Tool
}

func (t *dynamicActivationTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "activate_dynamic", Schema: map[string]any{"type": "object"}}
}
func (t *dynamicActivationTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	t.engine.AddDynamicTool(t.late)
	return core.TextOutput("activated"), nil
}
func (t *dynamicActivationTool) Preview(json.RawMessage) string { return "activate dynamic tool" }

func TestEngineOrchestration_InlineDynamicToolPublishesBeforeResume(t *testing.T) {
	provider := &inlineDynamicToolProvider{published: make(chan core.ToolSpec, 1)}
	registry := NewToolRegistry()
	activation := &dynamicActivationTool{late: &namedTestTool{name: "late_tool"}}
	registry.Register(activation)
	engine := NewEngine(provider, registry)
	activation.engine = engine

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("activate")},
		Tools:    []core.ToolSpec{activation.Spec()},
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var text strings.Builder
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventError {
			t.Fatal(event.Err)
		}
		if event.Type == core.EventTextDelta {
			text.WriteString(event.Text)
		}
	}
	if text.String() != "dynamic ready" {
		t.Fatalf("text = %q, want dynamic ready", text.String())
	}
}

func TestEngineOrchestration_InlineDynamicToolContinuationCarriesSteering(t *testing.T) {
	provider := &inlineDynamicToolProvider{published: make(chan core.ToolSpec, 1)}
	registry := NewToolRegistry()
	activation := &dynamicActivationTool{late: &namedTestTool{name: "late_tool"}}
	registry.Register(activation)
	engine := NewEngine(provider, registry)
	activation.engine = engine
	engine.Steer("use the new tool")

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("activate")},
		Tools:    []core.ToolSpec{activation.Spec()},
		MaxTurns: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var sawSteering bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventError {
			t.Fatal(event.Err)
		}
		if event.Type == core.EventSteering {
			sawSteering = true
		}
	}
	if !sawSteering {
		t.Fatal("expected steering to ride the dynamic-tool flush turn")
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("pending steering = %+v, want none", pending)
	}
}

func TestEngineOrchestration_InlineProviderPureTextCompletesOnce(t *testing.T) {
	provider := &inlineSyncToolProvider{inline: true}
	engine := NewEngine(provider, nil)
	stream, err := engine.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	done := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventDone {
			done++
		}
	}
	if provider.calls != 1 || done != 1 {
		t.Fatalf("provider calls/EventDone = %d/%d, want 1/1", provider.calls, done)
	}
}

func TestEngineOrchestration_InlineSyncToolLoopDoesNotReinvokeProvider(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{inline: true}
	engine := NewEngine(provider, registry)

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()

	var text strings.Builder
	done := 0
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("unexpected error: %v", event.Err)
		}
		if event.Type == core.EventTextDelta {
			text.WriteString(event.Text)
		}
		if event.Type == core.EventDone {
			done++
		}
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
	if done != 1 {
		t.Fatalf("EventDone count = %d, want 1", done)
	}
	if text.String() != "inline final" {
		t.Fatalf("text = %q, want inline final", text.String())
	}
}

func TestEngineOrchestration_InlineSyncToolLoopPersistsGuardianReviews(t *testing.T) {
	for _, tc := range []struct {
		name    string
		toolErr error
	}{
		{name: "success"},
		{name: "error", toolErr: errors.New("guardian denied execution")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			registry := NewToolRegistry()
			registry.Register(guardianSyncTool{err: tc.toolErr})
			provider := &inlineSyncToolProvider{inline: true}
			engine := NewEngine(provider, registry)
			var persisted []core.Message
			engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []core.Message, _ core.TurnMetrics) error {
				persisted = append(persisted, messages...)
				return nil
			})

			stream, err := engine.Stream(context.Background(), core.Request{
				Messages: []core.Message{core.UserText("use tool")}, Tools: []core.ToolSpec{{Name: "test_tool"}},
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			defer stream.Close()
			for {
				event, recvErr := stream.Recv()
				if recvErr == io.EOF {
					break
				}
				if recvErr != nil {
					t.Fatalf("Recv: %v", recvErr)
				}
				if event.Type == core.EventError {
					t.Fatalf("unexpected stream error: %v", event.Err)
				}
			}

			var result *core.ToolResult
			for _, message := range persisted {
				for _, part := range message.Parts {
					if part.Type == core.PartToolResult && part.ToolResult != nil && part.ToolResult.ID == "sync-1" {
						result = part.ToolResult
					}
				}
			}
			if result == nil || len(result.GuardianReviews) != 1 || result.GuardianReviews[0].Command != "rm -rf build" {
				t.Fatalf("persisted sync guardian result = %#v; messages = %#v", result, persisted)
			}
			if got, want := result.IsError, tc.toolErr != nil; got != want {
				t.Fatalf("result IsError = %v, want %v", got, want)
			}
		})
	}
}

func TestEngineOrchestration_InlineSyncToolLoopPersistsEventOrder(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{inline: true, ordered: true, leadingText: "before tool"}
	engine := NewEngine(provider, registry)
	var persisted []core.Message
	engine.SetTurnCompletedCallback(func(ctx context.Context, turn int, messages []core.Message, metrics core.TurnMetrics) error {
		persisted = append(persisted, messages...)
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("unexpected error: %v", event.Err)
		}
	}

	if len(persisted) < 1 || persisted[0].Role != core.RoleAssistant {
		t.Fatalf("persisted messages = %+v", persisted)
	}
	parts := persisted[0].Parts
	if len(parts) != 3 {
		t.Fatalf("assistant parts = %+v, want text/tool/text", parts)
	}
	if parts[0].Type != core.PartText || parts[0].Text != "before tool" || parts[1].Type != core.PartToolCall || parts[2].Type != core.PartText || parts[2].Text != "inline final" {
		t.Fatalf("assistant parts = %+v, want text/tool/text", parts)
	}
	for i, part := range parts {
		if part.CreatedAt <= 0 || (i > 0 && part.CreatedAt < parts[i-1].CreatedAt) {
			t.Fatalf("inline part %d has missing or decreasing timestamp: %d", i, part.CreatedAt)
		}
	}
}

func TestEngineOrchestration_InlineSyncToolLoopOrderedEventsPreservesToolThenFinalText(t *testing.T) {
	// Grok-bin shape from session #3751: tools first, then one final assistant segment.
	// Without OrderedInlineToolEvents the final rebuild puts all text above tool calls.
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{inline: true, ordered: true}
	engine := NewEngine(provider, registry)
	var persisted []core.Message
	engine.SetTurnCompletedCallback(func(ctx context.Context, turn int, messages []core.Message, metrics core.TurnMetrics) error {
		persisted = append(persisted, messages...)
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("unexpected error: %v", event.Err)
		}
	}

	if len(persisted) < 1 || persisted[0].Role != core.RoleAssistant {
		t.Fatalf("persisted messages = %+v", persisted)
	}
	parts := persisted[0].Parts
	if len(parts) != 2 {
		t.Fatalf("assistant parts = %+v, want tool/text", parts)
	}
	if parts[0].Type != core.PartToolCall || parts[1].Type != core.PartText || parts[1].Text != "inline final" {
		t.Fatalf("assistant parts = %+v, want tool then final text", parts)
	}
}

func TestEngineOrchestration_InlineSyncToolLoopWithoutOrderedEventsKeepsLegacyPersistence(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{inline: true, leadingText: "before tool"}
	engine := NewEngine(provider, registry)
	var persisted []core.Message
	engine.SetTurnCompletedCallback(func(ctx context.Context, turn int, messages []core.Message, metrics core.TurnMetrics) error {
		persisted = append(persisted, messages...)
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("unexpected error: %v", event.Err)
		}
	}

	if len(persisted) < 1 || persisted[0].Role != core.RoleAssistant {
		t.Fatalf("persisted messages = %+v", persisted)
	}
	parts := persisted[0].Parts
	if len(parts) != 2 {
		t.Fatalf("assistant parts = %+v, want legacy text/tool ordering", parts)
	}
	if parts[0].Type != core.PartText || parts[0].Text != "before toolinline final" || parts[1].Type != core.PartToolCall {
		t.Fatalf("assistant parts = %+v, want legacy text/tool ordering", parts)
	}
}

func TestEngineOrchestration_InlineSyncToolLoopLeavesSteeringForFollowUp(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{inline: true}
	engine := NewEngine(provider, registry)
	var persisted []core.Message
	engine.SetTurnCompletedCallback(func(ctx context.Context, turn int, messages []core.Message, metrics core.TurnMetrics) error {
		persisted = append(persisted, messages...)
		return nil
	})
	engine.Steer("follow-up instruction")

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	steeringIndex, doneIndex, index := -1, -1, 0
	var lateStatus SteeringQueueStatus
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventSteering {
			steeringIndex = index
		}
		if event.Type == core.EventDone {
			doneIndex = index
			_, lateStatus = engine.QueueSteeringWithStatus(QueuedSteering{ID: "late-inline", Message: core.UserText("too late")})
		}
		index++
	}
	if steeringIndex != -1 || doneIndex < 0 {
		t.Fatalf("steering/done indexes = %d/%d, want no committed steering before done", steeringIndex, doneIndex)
	}
	if lateStatus != SteeringQueueRunFinished {
		t.Fatalf("late inline status = %q, want %q", lateStatus, SteeringQueueRunFinished)
	}
	if len(persisted) == 0 || persisted[len(persisted)-1].Role == core.RoleUser {
		t.Fatalf("inline terminal path persisted steering: %+v", persisted)
	}
	pending := engine.ListPendingSteering()
	if len(pending) != 1 || core.MessageText(pending[0].Message) != "follow-up instruction" {
		t.Fatalf("pending steering = %+v, want only pre-boundary follow-up", pending)
	}
	if provider.calls != 1 {
		t.Fatalf("provider calls = %d, want 1", provider.calls)
	}
}

type inlineFlushingToolProvider struct {
	inlineSyncToolProvider
	flushed int
}

func (p *inlineFlushingToolProvider) RequestInlineFlush() {
	p.flushed++
}
func (p *inlineFlushingToolProvider) SupportsInlineFlush() bool { return true }

func TestEngineOrchestration_InlineFlushDeliversSteering(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineFlushingToolProvider{inlineSyncToolProvider: inlineSyncToolProvider{inline: true}}
	engine := NewEngine(provider, registry)
	var persisted []core.Message
	engine.SetTurnCompletedCallback(func(ctx context.Context, turn int, messages []core.Message, metrics core.TurnMetrics) error {
		persisted = append(persisted, messages...)
		return nil
	})

	engine.Steer("steer mid-loop")

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
		MaxTurns: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var text strings.Builder
	var sawSteering bool
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		switch event.Type {
		case core.EventTextDelta:
			text.WriteString(event.Text)
		case core.EventSteering:
			sawSteering = true
			if event.Text != "steer mid-loop" {
				t.Fatalf("steering text = %q", event.Text)
			}
		}
	}
	if !sawSteering {
		t.Fatal("expected EventSteering after inline flush")
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.calls)
	}
	if provider.flushed == 0 {
		t.Fatal("queued steering did not request an inline flush")
	}
	if text.String() != "inline finalcontinued final" {
		t.Fatalf("text = %q, want flushed continuation", text.String())
	}
	if pending := engine.ListPendingSteering(); len(pending) != 0 {
		t.Fatalf("pending steering = %+v, want none", pending)
	}
	foundCommitted := false
	for _, message := range persisted {
		if message.Role == core.RoleUser && core.MessageText(message) == "steer mid-loop" {
			foundCommitted = true
			break
		}
	}
	if !foundCommitted {
		t.Fatalf("persisted messages = %+v, want committed steering", persisted)
	}
}

func TestEngineOrchestration_InlineLoopLeavesModelSwitchForNextStream(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{inline: true}
	engine := NewEngine(provider, registry)
	req := core.Request{Model: "grok-4.5-high", Messages: []core.Message{core.UserText("use tool")}, Tools: []core.ToolSpec{{Name: "test_tool"}}}

	stream, err := engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	sawSwitch := false
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventToolExecStart {
			engine.QueueRequestModelSwitch("grok-4.5-xhigh")
		}
		if event.Type == core.EventModelSwitch {
			sawSwitch = true
		}
	}
	stream.Close()
	if sawSwitch {
		t.Fatal("inline return consumed model switch in the completed stream")
	}

	stream, err = engine.Stream(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	stream.Close()
	if len(provider.models) < 2 || provider.models[1] != "grok-4.5-xhigh" {
		t.Fatalf("provider models = %v, want queued switch on second stream", provider.models)
	}
}

func TestEngineOrchestration_NonInlineSyncToolLoopContinues(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})
	provider := &inlineSyncToolProvider{}
	engine := NewEngine(provider, registry)

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if event.Type == core.EventError {
			t.Fatalf("unexpected error: %v", event.Err)
		}
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want 2", provider.calls)
	}
}

func TestEngineOrchestration_BasicToolLoop(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "test_tool", result: "tool output"})

	provider := mockprovider.NewMockProvider("test")
	provider.AddToolCall("id-1", "test_tool", map[string]any{"arg": "val"})
	provider.AddTextResponse("final answer")

	engine := NewEngine(provider, registry)
	req := core.Request{
		Messages: []core.Message{core.UserText("hello")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var events []core.Event
	for {
		event, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				t.Logf("Recv error: %v", err)
			}
			break
		}
		t.Logf("Received event: %+v", event)
		events = append(events, event)
	}

	// Verify events
	hasToolExecStart := false
	hasToolExecEnd := false
	var fullText strings.Builder
	for _, e := range events {
		if e.Type == core.EventToolExecStart && e.ToolName == "test_tool" {
			hasToolExecStart = true
		}
		if e.Type == core.EventToolExecEnd && e.ToolName == "test_tool" && e.ToolSuccess {
			hasToolExecEnd = true
		}
		if e.Type == core.EventTextDelta {
			fullText.WriteString(e.Text)
		}
	}

	if !hasToolExecStart {
		t.Error("Missing EventToolExecStart")
	}
	if !hasToolExecEnd {
		t.Error("Missing EventToolExecEnd")
	}
	if !strings.Contains(fullText.String(), "final answer") {
		t.Errorf("Missing final answer text, got: %q", fullText.String())
	}

	// Verify provider calls
	if len(provider.Requests) != 2 {
		t.Errorf("Expected 2 provider calls, got %d", len(provider.Requests))
	}

	// Check second request contains tool results
	secondReq := provider.Requests[1]
	foundResult := false
	for _, msg := range secondReq.Messages {
		for _, part := range msg.Parts {
			if part.Type == core.PartToolResult && part.ToolResult.Name == "test_tool" && part.ToolResult.Content == "tool output" {
				foundResult = true
			}
		}
	}
	if !foundResult {
		t.Error("Tool result not found in second request")
	}
}

func TestEngineOrchestration_ExternalSearch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: core.WebSearchToolName, result: "search results"})

	provider := mockprovider.NewMockProvider("test")
	provider.WithCapabilities(core.Capabilities{ToolCalls: true, NativeWebSearch: false})

	// Turn 0: LLM decides to search naturally during conversation
	provider.AddToolCall("search-1", core.WebSearchToolName, map[string]any{"query": "zig"})
	// Turn 1: LLM provides answer based on search results
	provider.AddTextResponse("zig is a language")

	engine := NewEngine(provider, registry)
	req := core.Request{
		Messages: []core.Message{core.UserText("what is zig")},
		Search:   true,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	for {
		_, err := stream.Recv()
		if err != nil {
			break
		}
	}

	// Verify provider calls - should be exactly 2 (search + answer)
	if len(provider.Requests) != 2 {
		t.Errorf("Expected 2 provider calls, got %d", len(provider.Requests))
	}

	// Verify first request has web_search tool injected
	firstReq := provider.Requests[0]
	hasSearchTool := false
	for _, tool := range firstReq.Tools {
		if tool.Name == core.WebSearchToolName {
			hasSearchTool = true
			break
		}
	}
	if !hasSearchTool {
		t.Error("First request should have web_search tool injected")
	}
}

func TestEngineOrchestration_MaxTurns(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: "loop_tool", result: "looping"})

	provider := mockprovider.NewMockProvider("test")
	// Add more than defaultMaxTurns (20) tool calls
	for i := 0; i < 25; i++ {
		provider.AddToolCall(fmt.Sprintf("id-%d", i), "loop_tool", nil)
	}

	engine := NewEngine(provider, registry)
	req := core.Request{
		Messages: []core.Message{core.UserText("loop")},
		Tools:    []core.ToolSpec{{Name: "loop_tool"}},
		MaxTurns: 3,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var gotErr error
	warningIndex := -1
	errorIndex := -1
	var events []core.Event
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		events = append(events, event)
		idx := len(events) - 1
		if event.Type == core.EventPhase && strings.Contains(event.Text, "agent is out of turns") {
			warningIndex = idx
			if !strings.HasPrefix(event.Text, core.WarningPhasePrefix) {
				t.Fatalf("out-of-turns phase should use warning prefix, got %q", event.Text)
			}
		}
		if event.Type == core.EventError {
			gotErr = event.Err
			errorIndex = idx
		}
	}

	if gotErr == nil || !strings.Contains(gotErr.Error(), "exceeded max turns") {
		t.Errorf("Expected max turns error, got %v", gotErr)
	}
	if !IsMaxTurnsExceeded(gotErr) {
		t.Errorf("Expected IsMaxTurnsExceeded to match %T %v", gotErr, gotErr)
	}
	var typedErr *MaxTurnsExceededError
	if !errors.As(gotErr, &typedErr) || typedErr.MaxTurns != 3 {
		t.Errorf("Expected typed max turns error with MaxTurns=3, got %T %v", gotErr, gotErr)
	}
	if warningIndex < 0 {
		t.Fatalf("Expected out-of-turns warning phase before error; events=%v", eventTypes(events))
	}
	if errorIndex < 0 {
		t.Fatalf("Expected error event; events=%v", eventTypes(events))
	}
	if warningIndex >= errorIndex {
		t.Fatalf("Expected out-of-turns warning before error, warningIndex=%d errorIndex=%d events=%v", warningIndex, errorIndex, eventTypes(events))
	}

	if len(provider.Requests) != 3 {
		t.Errorf("Expected 3 provider calls (MaxTurns), got %d", len(provider.Requests))
	}
}

func TestEngineOrchestration_UnregisteredToolPassthrough(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := NewToolRegistry()

	provider := mockprovider.NewMockProvider("test")
	provider.AddToolCall("id-1", "unregistered_tool", map[string]any{"arg": "val"})

	engine := NewEngine(provider, registry)
	req := core.Request{
		Messages: []core.Message{core.UserText("hello")},
		Tools:    []core.ToolSpec{{Name: "unregistered_tool"}},
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var toolCalls []core.ToolCall
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == core.EventToolCall {
			toolCalls = append(toolCalls, *event.Tool)
		}
	}

	if len(toolCalls) != 1 {
		t.Errorf("Expected 1 tool call event, got %d", len(toolCalls))
	} else if toolCalls[0].Name != "unregistered_tool" {
		t.Errorf("Expected unregistered_tool, got %s", toolCalls[0].Name)
	}
}

func TestEngineOrchestration_ExternalSearchMixedCalls(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	registry := NewToolRegistry()
	registry.Register(&mockTool{name: core.WebSearchToolName, result: "search results"})

	provider := mockprovider.NewMockProvider("test")
	provider.WithCapabilities(core.Capabilities{ToolCalls: true, NativeWebSearch: false})

	// Turn 0: LLM returns BOTH search AND an unregistered tool
	// This tests that search tools work alongside other tools naturally
	provider.AddTurn(mockprovider.MockTurn{
		ToolCalls: []core.ToolCall{
			{ID: "search-1", Name: core.WebSearchToolName, Arguments: json.RawMessage(`{"query":"zig"}`)},
			{ID: "unreg-1", Name: "suggest_something", Arguments: json.RawMessage(`{}`)},
		},
	})
	provider.AddTextResponse("done")

	engine := NewEngine(provider, registry)
	req := core.Request{
		Messages: []core.Message{core.UserText("what is zig")},
		Search:   true,
	}

	stream, err := engine.Stream(ctx, req)
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	}
	defer stream.Close()

	var gotErr error
	var unregisteredToolCalls []core.ToolCall
	for {
		event, err := stream.Recv()
		if err != nil {
			break
		}
		if event.Type == core.EventError {
			gotErr = event.Err
		}
		if event.Type == core.EventToolCall {
			unregisteredToolCalls = append(unregisteredToolCalls, *event.Tool)
		}
	}

	if gotErr != nil {
		t.Errorf("Did not expect error for mixed calls, got: %v", gotErr)
	}

	// All tool calls are now passed through as events to preserve interleaving order
	// We expect 2 events: 1 registered (web_search) + 1 unregistered (suggest_something)
	if len(unregisteredToolCalls) != 2 {
		t.Errorf("Expected 2 tool call events, got %d", len(unregisteredToolCalls))
	}
	// Verify unregistered tool call is present
	var foundUnregistered bool
	for _, call := range unregisteredToolCalls {
		if call.Name == "suggest_something" {
			foundUnregistered = true
		}
	}
	if !foundUnregistered {
		t.Errorf("Expected suggest_something tool call to be present")
	}
}

type immediateInterruptionProvider struct {
	calls       int
	interrupted chan struct{}
	once        sync.Once
}

func (p *immediateInterruptionProvider) Name() string       { return "immediate-interruption-test" }
func (p *immediateInterruptionProvider) Credential() string { return "test" }
func (p *immediateInterruptionProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}
}
func (p *immediateInterruptionProvider) SupportsInlineFlush() bool { return true }
func (p *immediateInterruptionProvider) SupportsImmediateInterruption() bool {
	return true
}
func (p *immediateInterruptionProvider) RequestInlineFlush() {
	p.once.Do(func() { close(p.interrupted) })
}
func (p *immediateInterruptionProvider) Stream(ctx context.Context, _ core.Request) (core.Stream, error) {
	p.calls++
	call := p.calls
	return corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		if call == 1 {
			if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "partial"}); err != nil {
				return err
			}
			select {
			case <-p.interrupted:
			case <-ctx.Done():
				return ctx.Err()
			}
		} else {
			if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "steered"}); err != nil {
				return err
			}
		}
		return send.Send(core.Event{Type: core.EventDone})
	}), nil
}

func TestImmediateInterruptionEnablesSteeringWithoutTools(t *testing.T) {
	provider := &immediateInterruptionProvider{interrupted: make(chan struct{})}
	engine := NewEngine(provider, nil)
	stream, err := engine.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("start")}, MaxTurns: 3})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var text strings.Builder
	committed := false
	queued := false
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatal(recvErr)
		}
		if event.Type == core.EventTextDelta {
			text.WriteString(event.Text)
			if !queued {
				_, status := engine.QueueSteeringWithStatus(QueuedSteering{ID: "no-tools-steer", Message: core.UserText("steer")})
				if status != SteeringQueueQueued {
					t.Fatalf("queue status = %q", status)
				}
				queued = true
			}
		}
		if event.Type == core.EventSteering && event.SteeringID == "no-tools-steer" {
			committed = true
		}
	}
	if provider.calls != 2 {
		t.Fatalf("provider calls = %d, want interrupted turn plus resumed steer", provider.calls)
	}
	if !committed {
		t.Fatal("queued steering was not committed")
	}
	if got := text.String(); got != "partialsteered" {
		t.Fatalf("text = %q", got)
	}
}

// inlineFlushToolProvider models the CLI providers that own their tool loop and
// can end the current prompt at a tool-result boundary (claude-bin, grok-bin,
// cursor-bin, agy-bin). The first prompt runs one tool then stops, as it would
// after seeing the inline flush notice.
type inlineFlushToolProvider struct {
	calls         int
	flushRequests atomic.Int64
}

func (p *inlineFlushToolProvider) Name() string       { return "inline-flush-test" }
func (p *inlineFlushToolProvider) Credential() string { return "test" }
func (p *inlineFlushToolProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}
}
func (p *inlineFlushToolProvider) RequestInlineFlush()       { p.flushRequests.Add(1) }
func (p *inlineFlushToolProvider) SupportsInlineFlush() bool { return true }

func (p *inlineFlushToolProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	p.calls++
	call := p.calls
	return corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		if call == 1 {
			response := make(chan core.ToolExecutionResponse, 1)
			if err := send.Send(core.Event{
				Type:         core.EventToolCall,
				ToolCallID:   "flush-1",
				ToolName:     "test_tool",
				Tool:         &core.ToolCall{ID: "flush-1", Name: "test_tool", Arguments: json.RawMessage(`{}`)},
				ToolResponse: response,
			}); err != nil {
				return err
			}
			select {
			case <-response:
			case <-ctx.Done():
				return ctx.Err()
			}
			return send.Send(core.Event{Type: core.EventDone})
		}
		if err := send.Send(core.Event{Type: core.EventTextDelta, Text: "after steering"}); err != nil {
			return err
		}
		return send.Send(core.Event{Type: core.EventDone})
	}), nil
}

type steeringTool struct {
	name string
	fn   func()
}

func (t *steeringTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (t *steeringTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	if t.fn != nil {
		t.fn()
	}
	return core.TextOutput("tool output"), nil
}
func (t *steeringTool) Preview(args json.RawMessage) string { return "" }

// TestInlineFlushCommitsSteeringExactlyOnce pins the persistence contract
// behind the inline flush path. Each committed steering is persisted by its
// consumer keyed on SteeringID, so emitting one twice writes the same
// client_message_id twice and trips the session store's unique index.
func TestInlineFlushCommitsSteeringExactlyOnce(t *testing.T) {
	provider := &inlineFlushToolProvider{}
	engine := NewEngine(provider, nil)
	registry := NewToolRegistry()
	registry.Register(&steeringTool{name: "test_tool", fn: func() {
		engine.QueueSteering(QueuedSteering{ID: "tui-steer-1", Message: core.UserText("steer me")})
	}})
	engine.tools = registry

	var turnUserMessages int
	engine.SetTurnCompletedCallback(func(ctx context.Context, _ int, msgs []core.Message, _ core.TurnMetrics) error {
		for _, msg := range msgs {
			if msg.Role == core.RoleUser {
				turnUserMessages++
			}
		}
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{
		Messages: []core.Message{core.UserText("use tool")},
		Tools:    []core.ToolSpec{{Name: "test_tool"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	commits := map[string]int{}
	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Type == core.EventSteering {
			commits[event.SteeringID]++
		}
	}

	if provider.flushRequests.Load() == 0 {
		t.Fatal("queued steering did not request an inline flush")
	}
	if got := commits["tui-steer-1"]; got != 1 {
		t.Fatalf("steering commit events = %d, want exactly 1 (duplicates collide on client_message_id)", got)
	}
	if turnUserMessages != 1 {
		t.Fatalf("turn callback user messages = %d, want exactly 1", turnUserMessages)
	}
}
