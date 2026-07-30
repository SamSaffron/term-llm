package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestAttachProviderReplayPartsDeepCopiesOpaqueState(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"secret"}`)
	msg := Message{Role: RoleAssistant, Parts: []Part{{Type: PartText, Text: "visible"}}}
	got := attachProviderReplayParts(msg, []Part{{
		Type: PartProviderReplay,
		ProviderReplay: &ProviderReplayItem{
			Raw: raw,
		},
	}})
	if len(got.Parts) != 2 || got.Parts[1].Type != PartProviderReplay || got.Parts[1].ProviderReplay == nil {
		t.Fatalf("message parts = %#v", got.Parts)
	}
	raw[0] = '['
	if got.Parts[1].ProviderReplay.Raw[0] != '{' {
		t.Fatalf("replay raw aliases source: %q", got.Parts[1].ProviderReplay.Raw)
	}
}

func TestAttachProviderReplayPartsIncludesDisplayToolActivity(t *testing.T) {
	msg := AssistantText("answer")
	got := attachProviderReplayParts(msg, []Part{
		{Type: PartToolActivity, ToolActivity: &ToolActivity{
			ID:     "ws_1",
			Name:   WebSearchToolName,
			Info:   "(discourse news)",
			Status: ToolActivityCompleted,
		}},
		{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{
			Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_1"}`),
		}},
	})
	if len(got.Parts) != 3 {
		t.Fatalf("message parts = %#v, want text, activity, and replay", got.Parts)
	}
	activityPart := got.Parts[1]
	if activityPart.Type != PartToolActivity || activityPart.ToolActivity == nil || activityPart.ToolActivity.Info != "(discourse news)" {
		t.Fatalf("activity part = %#v", activityPart)
	}
	if got.Parts[2].Type != PartProviderReplay || got.Parts[2].ProviderReplay == nil {
		t.Fatalf("replay part = %#v, want opaque raw state", got.Parts[2])
	}
}

func TestEnginePersistsProviderReplayToolActivity(t *testing.T) {
	activity := &ToolActivity{ID: "ws_1", Name: WebSearchToolName, Info: "(discourse news)", Status: ToolActivityCompleted}
	provider := &fakeProvider{script: func(int, Request) []Event {
		return []Event{
			{Type: EventTextDelta, Text: "answer"},
			{Type: EventToolActivity, ToolActivity: activity},
			{Type: EventProviderReplay, ProviderReplay: &ProviderReplayItem{
				Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_1","status":"completed"}`),
			}},
			{Type: EventDone},
		}
	}}
	engine := NewEngine(provider, NewToolRegistry())
	var completed []Message
	engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []Message, _ TurnMetrics) error {
		completed = append(completed, messages...)
		return nil
	})

	stream, err := engine.Stream(context.Background(), Request{Messages: []Message{UserText("search")}})
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
	if part := completed[0].Parts[1]; part.Type != PartToolActivity || part.ToolActivity == nil || part.ToolActivity.Info != "(discourse news)" {
		t.Fatalf("persisted activity part = %#v", part)
	}
	if part := completed[0].Parts[2]; part.Type != PartProviderReplay || part.ProviderReplay == nil {
		t.Fatalf("persisted replay part = %#v", part)
	}
}

func TestEngineSnapshotsToolActivityBeforeLaterStreamFailure(t *testing.T) {
	activity := &ToolActivity{ID: "ws_1", Name: WebSearchToolName, Info: "(discourse news)", Status: ToolActivityCompleted}
	provider := &fakeProvider{script: func(int, Request) []Event {
		return []Event{
			{Type: EventToolActivity, ToolActivity: activity},
			{Type: EventError, Err: errors.New("answer stream failed")},
		}
	}}
	engine := NewEngine(provider, NewToolRegistry())
	var snapshots []Message
	engine.SetAssistantSnapshotCallback(func(_ context.Context, _ int, msg Message) error {
		snapshots = append(snapshots, msg)
		return nil
	})

	stream, err := engine.Stream(context.Background(), Request{Messages: []Message{UserText("search")}})
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
		if event.Type == EventError {
			streamErr = event.Err
		}
	}
	if streamErr == nil {
		t.Fatal("expected terminal stream error")
	}
	if len(snapshots) != 1 || len(snapshots[0].Parts) != 1 || snapshots[0].Parts[0].Type != PartToolActivity {
		t.Fatalf("snapshots = %#v, want durable activity before stream failure", snapshots)
	}
}

type retryToActivityProvider struct {
	calls int
}

func (p *retryToActivityProvider) Name() string       { return "retry-to-activity" }
func (p *retryToActivityProvider) Credential() string { return "test" }
func (p *retryToActivityProvider) Capabilities() Capabilities {
	return Capabilities{ToolCalls: true}
}
func (p *retryToActivityProvider) Stream(context.Context, Request) (Stream, error) {
	p.calls++
	switch p.calls {
	case 1:
		return &errAfterEventsStream{err: &StreamIncompleteError{Transport: "test", Err: errors.New("disconnected")}}, nil
	case 2:
		return &sliceStream{events: []Event{
			{Type: EventToolActivity, ToolActivity: &ToolActivity{ID: "ws_retry", Name: WebSearchToolName, Status: ToolActivityCompleted}},
			{Type: EventProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(`{"type":"web_search_call","id":"ws_retry"}`)}},
			{Type: EventDone},
		}}, nil
	default:
		return &sliceStream{events: []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}}, nil
	}
}

func TestEngineCommitsReplayOnlyActivityAfterUncommittedRetry(t *testing.T) {
	provider := &retryToActivityProvider{}
	tool := &countingTool{}
	registry := NewToolRegistry()
	registry.Register(tool)
	engine := NewEngine(provider, registry)
	var turns [][]Message
	engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []Message, _ TurnMetrics) error {
		turns = append(turns, append([]Message(nil), messages...))
		return nil
	})

	stream, err := engine.Stream(context.Background(), Request{Messages: []Message{UserText("search")}, Tools: []ToolSpec{tool.Spec()}, MaxTurns: 4})
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
		if event.Type == EventError {
			t.Fatalf("unexpected stream error: %v", event.Err)
		}
	}
	if len(turns) < 1 || len(turns[0]) != 1 {
		t.Fatalf("turns = %#v", turns)
	}
	foundActivity := false
	for _, part := range turns[0][0].Parts {
		foundActivity = foundActivity || part.Type == PartToolActivity
	}
	if !foundActivity {
		t.Fatalf("replay-only retry turn lost tool activity: %#v", turns[0][0])
	}
}

func TestBuildResponsesAssistantItemsUsesOnlyOpaqueReplayWhenPresent(t *testing.T) {
	raw := json.RawMessage(`{"type":"reasoning","id":"rs_1","summary":[]}`)
	items := buildResponsesAssistantItems([]Part{
		{Type: PartText, Text: "derived text"},
		{Type: PartToolActivity, ToolActivity: &ToolActivity{ID: "ws_1", Name: WebSearchToolName, Status: ToolActivityCompleted}},
		{Type: PartToolCall, ToolCall: &ToolCall{ID: "call_1", Name: "shell", Arguments: json.RawMessage(`{}`)}},
		{Type: PartProviderReplay, ProviderReplay: &ProviderReplayItem{Raw: raw}},
	})
	if len(items) != 1 {
		t.Fatalf("items = %#v, want only opaque replay item", items)
	}
	encoded, err := json.Marshal(items[0])
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(encoded) != string(raw) {
		t.Fatalf("encoded replay = %s, want exact %s", encoded, raw)
	}
}
