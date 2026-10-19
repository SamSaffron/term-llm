package engine

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"io"
	"testing"
)

func TestAttachProviderReplayPartsDeepCopiesOpaqueState(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"secret"}`)
	msg := core.Message{Role: core.RoleAssistant, Parts: []core.Part{{Type: core.PartText, Text: "visible"}}}
	got := attachProviderReplayParts(msg, []core.Part{{
		Type: core.PartProviderReplay,
		ProviderReplay: &core.ProviderReplayItem{
			Raw: raw,
		},
	}})
	if len(got.Parts) != 2 || got.Parts[1].Type != core.PartProviderReplay || got.Parts[1].ProviderReplay == nil {
		t.Fatalf("message parts = %#v", got.Parts)
	}
	raw[0] = '['
	if got.Parts[1].ProviderReplay.Raw[0] != '{' {
		t.Fatalf("replay raw aliases source: %q", got.Parts[1].ProviderReplay.Raw)
	}
}

func TestAttachProviderReplayPartsIncludesDisplayToolActivity(t *testing.T) {
	msg := core.AssistantText("answer")
	got := attachProviderReplayParts(msg, []core.Part{
		{Type: core.PartToolActivity, ToolActivity: &core.ToolActivity{
			ID:     "ws_1",
			Name:   core.WebSearchToolName,
			Info:   "(discourse news)",
			Status: core.ToolActivityCompleted,
		}},
		{Type: core.PartProviderReplay, ProviderReplay: &core.ProviderReplayItem{
			Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_1"}`),
		}},
	})
	if len(got.Parts) != 3 {
		t.Fatalf("message parts = %#v, want text, activity, and replay", got.Parts)
	}
	activityPart := got.Parts[1]
	if activityPart.Type != core.PartToolActivity || activityPart.ToolActivity == nil || activityPart.ToolActivity.Info != "(discourse news)" {
		t.Fatalf("activity part = %#v", activityPart)
	}
	if got.Parts[2].Type != core.PartProviderReplay || got.Parts[2].ProviderReplay == nil {
		t.Fatalf("replay part = %#v, want opaque raw state", got.Parts[2])
	}
}

func TestEnginePersistsProviderReplayToolActivity(t *testing.T) {
	activity := &core.ToolActivity{ID: "ws_1", Name: core.WebSearchToolName, Info: "(discourse news)", Status: core.ToolActivityCompleted}
	provider := &fakeProvider{script: func(int, core.Request) []core.Event {
		return []core.Event{
			{Type: core.EventTextDelta, Text: "answer"},
			{Type: core.EventToolActivity, ToolActivity: activity},
			{Type: core.EventProviderReplay, ProviderReplay: &core.ProviderReplayItem{
				Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_1","status":"completed"}`),
			}},
			{Type: core.EventDone},
		}
	}}
	engine := NewEngine(provider, NewToolRegistry())
	var completed []core.Message
	engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []core.Message, _ core.TurnMetrics) error {
		completed = append(completed, messages...)
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("search")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
	}

	if len(completed) != 1 || len(completed[0].Parts) != 3 {
		t.Fatalf("completed messages = %#v", completed)
	}
	if part := completed[0].Parts[1]; part.Type != core.PartToolActivity || part.ToolActivity == nil || part.ToolActivity.Info != "(discourse news)" {
		t.Fatalf("persisted activity part = %#v", part)
	}
	if part := completed[0].Parts[2]; part.Type != core.PartProviderReplay || part.ProviderReplay == nil {
		t.Fatalf("persisted replay part = %#v", part)
	}
}

func TestEngineSnapshotsToolActivityBeforeLaterStreamFailure(t *testing.T) {
	activity := &core.ToolActivity{ID: "ws_1", Name: core.WebSearchToolName, Info: "(discourse news)", Status: core.ToolActivityCompleted}
	provider := &fakeProvider{script: func(int, core.Request) []core.Event {
		return []core.Event{
			{Type: core.EventToolActivity, ToolActivity: activity},
			{Type: core.EventError, Err: errors.New("answer stream failed")},
		}
	}}
	engine := NewEngine(provider, NewToolRegistry())
	var snapshots []core.Message
	engine.SetAssistantSnapshotCallback(func(_ context.Context, _ int, msg core.Message) error {
		snapshots = append(snapshots, msg)
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("search")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer stream.Close()
	var streamErr error
	for {
		event, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv: %v", recvErr)
		}
		if event.Type == core.EventError {
			streamErr = event.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected terminal stream error")
	}
	if len(snapshots) != 1 || len(snapshots[0].Parts) != 1 || snapshots[0].Parts[0].Type != core.PartToolActivity {
		t.Fatalf("snapshots = %#v, want durable activity before stream failure", snapshots)
	}
}

type retryToActivityProvider struct {
	calls int
}

func (p *retryToActivityProvider) Name() string       { return "retry-to-activity" }
func (p *retryToActivityProvider) Credential() string { return "test" }
func (p *retryToActivityProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true}
}
func (p *retryToActivityProvider) Stream(context.Context, core.Request) (core.Stream, error) {
	p.calls++
	switch p.calls {
	case 1:
		return &errAfterEventsStream{err: &corestream.StreamIncompleteError{Transport: "test", Err: errors.New("disconnected")}}, nil
	case 2:
		return &sliceStream{events: []core.Event{
			{Type: core.EventToolActivity, ToolActivity: &core.ToolActivity{ID: "ws_retry", Name: core.WebSearchToolName, Status: core.ToolActivityCompleted}},
			{Type: core.EventProviderReplay, ProviderReplay: &core.ProviderReplayItem{Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_retry"}`)}},
			{Type: core.EventDone},
		}}, nil
	default:
		return &sliceStream{events: []core.Event{{Type: core.EventTextDelta, Text: "done"}, {Type: core.EventDone}}}, nil
	}
}

func TestEngineCommitsReplayOnlyActivityAfterUncommittedRetry(t *testing.T) {
	provider := &retryToActivityProvider{}
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)
	var turns [][]core.Message
	engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []core.Message, _ core.TurnMetrics) error {
		turns = append(turns, append([]core.Message(nil), messages...))
		return nil
	})

	stream, err := engine.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("search")}, Tools: []core.ToolSpec{tool.Spec()}, MaxTurns: 4})
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
	if len(turns) < 1 || len(turns[0]) != 1 {
		t.Fatalf("turns = %#v", turns)
	}
	foundActivity := false
	for _, part := range turns[0][0].Parts {
		foundActivity = foundActivity || part.Type == core.PartToolActivity
	}
	if !foundActivity {
		t.Fatalf("replay-only retry turn lost tool activity: %#v", turns[0][0])
	}
}
