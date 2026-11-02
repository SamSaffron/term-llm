package engine

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
	corestream "github.com/samsaffron/term-llm/internal/llm/core/stream"
	"github.com/samsaffron/term-llm/internal/llm/providers/responses"
)

type reasoningInlineProvider struct {
	events      []core.Event
	terminalErr error
	ordered     bool
}

func (*reasoningInlineProvider) Name() string       { return "reasoning-inline-test" }
func (*reasoningInlineProvider) Credential() string { return "test" }
func (p *reasoningInlineProvider) Capabilities() core.Capabilities {
	return core.Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: p.ordered}
}
func (p *reasoningInlineProvider) Stream(ctx context.Context, _ core.Request) (core.Stream, error) {
	return corestream.New(ctx, func(ctx context.Context, send corestream.Sender) error {
		for _, event := range p.events {
			var response chan core.ToolExecutionResponse
			if event.Type == core.EventToolCall {
				response = make(chan core.ToolExecutionResponse, 1)
				event.ToolResponse = response
			}
			if err := send.Send(event); err != nil {
				return err
			}
			if response != nil {
				select {
				case <-response:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return p.terminalErr
	}), nil
}

func reasoningEvent(text string) core.Event {
	return core.Event{Type: core.EventReasoningDelta, Text: text, ReasoningKind: core.ReasoningKindRaw}
}
func reasoningToolEvent(id string) core.Event {
	return core.Event{Type: core.EventToolCall, Tool: &core.ToolCall{ID: id, Name: "test_tool", Arguments: json.RawMessage(`{}`)}}
}

func displayedInlineParts(msg core.Message) []string {
	var out []string
	for _, part := range msg.Parts {
		if display := core.ReasoningForDisplay(part); display != nil {
			out = append(out, "thought:"+display.Content)
		}
		if part.Text != "" {
			out = append(out, "text:"+part.Text)
		}
		if part.ToolCall != nil {
			out = append(out, "tool:"+part.ToolCall.ID)
		}
	}
	return out
}

func TestInlineReasoningPersistsBurstOrder(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "failure journal"}[fail], func(t *testing.T) {
			provider := &reasoningInlineProvider{ordered: true, events: []core.Event{
				reasoningEvent("before"), reasoningToolEvent("one"),
				reasoningEvent("after"), {Type: core.EventTextDelta, Text: "middle"}, reasoningToolEvent("two"),
				reasoningEvent("tail"), {Type: core.EventTextDelta, Text: "final"},
				{Type: core.EventReasoningDelta, ReasoningItemID: "rs_final", ReasoningEncryptedContent: "encrypted", ReasoningKind: core.ReasoningKindEncrypted},
			}}
			if fail {
				provider.terminalErr = errors.New("terminal failure")
			}
			registry := NewToolRegistry()
			registry.Register(&mockTool{name: "test_tool", result: "result"})
			engine := NewEngine(provider, registry)
			rec := &snapshotRecorder{}
			engine.SetAssistantSnapshotCallback(rec.callback())
			var completed []core.Message
			engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []core.Message, _ core.TurnMetrics) error {
				completed = append(completed, messages...)
				return nil
			})
			stream, err := engine.Stream(context.Background(), core.Request{Messages: []core.Message{core.UserText("use tools")}, Tools: []core.ToolSpec{{Name: "test_tool"}}})
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Close()
			if err := drainStreamErr(t, stream); (err != nil) != fail {
				t.Fatalf("stream error = %v, fail = %v", err, fail)
			}
			if len(completed) != 3 {
				t.Fatalf("completed = %#v", completed)
			}
			want := []string{"thought:before", "tool:one", "thought:after", "text:middle", "tool:two", "thought:tail", "text:final"}
			if got := displayedInlineParts(completed[0]); !reflect.DeepEqual(got, want) {
				t.Fatalf("display = %v, want %v", got, want)
			}
			if rec.count() != 2 {
				t.Fatalf("snapshots = %d", rec.count())
			}
			for i, want := range [][]string{want[:2], want[:5]} {
				if got := displayedInlineParts(rec.at(i)); !reflect.DeepEqual(got, want) {
					t.Fatalf("snapshot %d = %v, want %v", i, got, want)
				}
			}
			var aggregate *core.Part
			for i := range completed[0].Parts {
				part := &completed[0].Parts[i]
				if part.ReasoningContent != "" {
					if aggregate != nil {
						t.Fatal("duplicate replay reasoning")
					}
					aggregate = part
				}
			}
			if aggregate == nil || !aggregate.ReasoningReplayOnly || aggregate.ReasoningContent != "beforeaftertail" || aggregate.ReasoningEncryptedContent != "encrypted" || aggregate.ReasoningItemID != "rs_final" || aggregate.ReasoningKind != core.ReasoningKindRaw {
				t.Fatalf("aggregate = %#v", aggregate)
			}
			// JSON save/reload must keep the same visible sequence.
			raw, err := json.Marshal(completed[0])
			if err != nil {
				t.Fatal(err)
			}
			var restored core.Message
			if err := json.Unmarshal(raw, &restored); err != nil {
				t.Fatal(err)
			}
			if got := displayedInlineParts(restored); !reflect.DeepEqual(got, want) {
				t.Fatalf("reloaded = %v", got)
			}
		})
	}
}

func TestInlineReasoningToolThenThoughtAndLegacy(t *testing.T) {
	for _, ordered := range []bool{true, false} {
		provider := &reasoningInlineProvider{ordered: ordered, events: []core.Event{reasoningToolEvent("one"), reasoningEvent("after")}}
		registry := NewToolRegistry()
		registry.Register(&mockTool{name: "test_tool", result: "result"})
		engine := NewEngine(provider, registry)
		var assistant core.Message
		engine.SetTurnCompletedCallback(func(_ context.Context, _ int, messages []core.Message, _ core.TurnMetrics) error {
			assistant = messages[0]
			return nil
		})
		stream, err := engine.Stream(context.Background(), core.Request{Tools: []core.ToolSpec{{Name: "test_tool"}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := drainStreamErr(t, stream); err != nil {
			t.Fatal(err)
		}
		stream.Close()
		want := []string{"tool:one", "thought:after"}
		if !ordered {
			want = []string{"thought:after", "tool:one"}
		}
		if got := displayedInlineParts(assistant); !reflect.DeepEqual(got, want) {
			t.Fatalf("ordered=%v: display=%v, want %v", ordered, got, want)
		}
	}
}

func TestInlineReasoningBoundariesAndSummarySnapshots(t *testing.T) {
	for _, boundary := range []core.EventType{core.EventToolCall, core.EventToolActivity, core.EventToolExecStart, core.EventToolExecEnd} {
		t.Run(string(boundary), func(t *testing.T) {
			turn := &providerTurnFrame{}
			turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, Text: "first", ReasoningKind: core.ReasoningKindSummary, ReasoningItemID: "rs"})
			turn.observeInlineReasoning(core.Event{Type: boundary})
			turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningSummaryParts: []string{"firstsecond"}, ReasoningKind: core.ReasoningKindSummary, ReasoningItemID: "rs"})
			turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningEncryptedContent: "secret", ReasoningKind: core.ReasoningKindEncrypted})
			if len(turn.inlineSyncParts) != 2 {
				t.Fatalf("parts = %#v", turn.inlineSyncParts)
			}
			if got := turn.inlineSyncParts[0].ReasoningDisplay.Content; got != "first" {
				t.Fatalf("first = %q", got)
			}
			if got := turn.inlineSyncParts[1].ReasoningDisplay.Content; got != "second" {
				t.Fatalf("second = %q", got)
			}
			if len(turn.inlineSyncParts[1].ReasoningDisplay.SummaryParts) != 0 {
				t.Fatal("cumulative summary leaked into second burst")
			}
		})
	}
}

func TestInlineReasoningReplayAndSummarySanitization(t *testing.T) {
	tool := core.Part{Type: core.PartToolCall, ToolCall: &core.ToolCall{ID: "one", Name: "test_tool", Arguments: json.RawMessage(`{}`)}}
	for _, withText := range []bool{true, false} {
		var text string
		if withText {
			text = "answer"
		}
		burst := core.Part{Type: core.PartText, ReasoningDisplay: &core.ReasoningDisplay{Content: "summary", SummaryParts: []string{"summary"}, Kind: core.ReasoningKindSummary}}
		ordered := []core.Part{tool, burst}
		if text != "" {
			ordered = append(ordered, core.Part{Type: core.PartText, Text: text})
		}
		legacy := buildInterleavedAssistantMessageWithReasoningMetadata(append([]core.Part{tool}, ordered[2:]...), "summary", []string{"summary"}, "rs", "encrypted", core.ReasoningKindSummary)
		msg := buildInterleavedAssistantMessageWithReasoningMetadata(ordered, "summary", []string{"summary"}, "rs", "encrypted", core.ReasoningKindSummary)
		result := core.Message{Role: core.RoleTool, Parts: []core.Part{{Type: core.PartToolResult, ToolResult: &core.ToolResult{ID: "one", Name: "test_tool", Content: "ok"}}}}
		if got, want := responses.BuildResponsesInput([]core.Message{msg, result}), responses.BuildResponsesInput([]core.Message{legacy, result}); !reflect.DeepEqual(got, want) {
			t.Fatalf("replay changed:\ngot %#v\nwant %#v", got, want)
		}
		msg.Parts[len(msg.Parts)-1].ReasoningDisplay = nil // Inputs cannot mutate already-built bursts.
		if burst.ReasoningDisplay.Content != "summary" {
			t.Fatal("source mutated")
		}
		for _, part := range prepareMessagesForSummaryHelper([]core.Message{msg, result})[0].Parts {
			if part.ReasoningDisplay != nil || part.ReasoningReplayOnly || part.ReasoningContent != "" || part.ReasoningEncryptedContent != "" {
				t.Fatalf("reasoning leaked into summary input: %#v", part)
			}
		}
	}
}

func TestInlineReasoningStructuredSummaryWithEncryptedReplay(t *testing.T) {
	turn := &providerTurnFrame{}
	turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningItemID: "rs_one", ReasoningKind: core.ReasoningKindSummary, ReasoningSummaryParts: []string{"**First**", "details"}, ReasoningEncryptedContent: "secret"})
	if len(turn.inlineSyncParts) != 1 {
		t.Fatalf("parts = %#v", turn.inlineSyncParts)
	}
	display := turn.inlineSyncParts[0].ReasoningDisplay
	if display.Content != "**First**\n\ndetails" || !reflect.DeepEqual(display.SummaryParts, []string{"**First**", "details"}) || display.Kind != core.ReasoningKindSummary || display.SummaryTitle != "First" {
		t.Fatalf("display = %#v", display)
	}
	// A metadata-only new-item event cannot consume the paragraph boundary of
	// the first real delta from that item, or alter an already-published burst.
	turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningItemID: "rs_two"})
	turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningItemID: "rs_two", Text: "next", ReasoningKind: core.ReasoningKindRaw})
	if got := turn.inlineSyncParts[0].ReasoningDisplay; got.Content != "**First**\n\ndetails\n\nnext" || got.Kind != core.ReasoningKindRaw || got.SummaryTitle != "" || len(got.SummaryParts) != 0 {
		t.Fatalf("updated display = %#v", got)
	}
	if display.Content != "**First**\n\ndetails" {
		t.Fatal("later deltas mutated published burst")
	}
}

func TestInlineReasoningRejectsUnsafeDisplayDeltas(t *testing.T) {
	for _, unsafeKind := range []core.ReasoningKind{core.ReasoningKindUnknown, "", "invalid", core.ReasoningKindEncrypted} {
		for _, safeKind := range []core.ReasoningKind{core.ReasoningKindSummary, core.ReasoningKindRaw} {
			for _, payload := range []string{"text", "summary parts", "text and summary parts"} {
				t.Run(string(unsafeKind)+"/"+string(safeKind)+"/"+payload, func(t *testing.T) {
					unsafe := core.Event{Type: core.EventReasoningDelta, ReasoningKind: unsafeKind, ReasoningItemID: "unsafe"}
					if payload != "summary parts" {
						unsafe.Text = "UNSAFE"
					}
					if payload != "text" {
						unsafe.ReasoningSummaryParts = []string{"UNSAFE"}
					}
					if unsafeKind == core.ReasoningKindEncrypted {
						unsafe.ReasoningEncryptedContent = "encrypted replay"
					}
					turn := &providerTurnFrame{}
					turn.observeInlineReasoning(unsafe)
					if len(turn.inlineSyncParts) != 0 || turn.inlineReasoning != (inlineReasoningState{}) {
						t.Fatalf("unsafe delta started a display burst: %#v, %#v", turn.inlineSyncParts, turn.inlineReasoning)
					}
					turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, Text: "safe", ReasoningKind: safeKind, ReasoningItemID: "safe"})
					// Keep a shallow snapshot, just as published message parts do.
					snapshot := append([]core.Part(nil), turn.inlineSyncParts...)
					state := turn.inlineReasoning
					turn.observeInlineReasoning(unsafe)
					if turn.inlineReasoning != state {
						t.Fatalf("unsafe delta changed display state: %#v, want %#v", turn.inlineReasoning, state)
					}
					turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, Text: " next", ReasoningKind: safeKind, ReasoningItemID: "safe"})
					turn.observeInlineReasoning(unsafe)
					if len(turn.inlineSyncParts) != 1 {
						t.Fatalf("parts = %#v", turn.inlineSyncParts)
					}
					if got := turn.inlineSyncParts[0].ReasoningDisplay; got.Content != "safe next" || got.Kind != safeKind || len(got.SummaryParts) != 0 {
						t.Fatalf("display = %#v", got)
					}
					if got := snapshot[0].ReasoningDisplay; got.Content != "safe" || got.Kind != safeKind {
						t.Fatalf("published snapshot mutated: %#v", got)
					}
				})
			}
		}
	}
}

func TestInlineReasoningSummaryPartsRemainImmutable(t *testing.T) {
	turn := &providerTurnFrame{}
	summary := []string{"**First**", "details"}
	turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, ReasoningSummaryParts: summary, ReasoningEncryptedContent: "encrypted replay"})
	snapshot := append([]core.Part(nil), turn.inlineSyncParts...)
	want := core.ReasoningDisplay{Content: "**First**\n\ndetails", SummaryParts: []string{"**First**", "details"}, Kind: core.ReasoningKindSummary, SummaryTitle: "First"}
	summary[0] = "mutated provider input"
	turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, Text: "UNSAFE", ReasoningKind: core.ReasoningKindUnknown, ReasoningSummaryParts: []string{"UNSAFE"}})
	if got := turn.inlineSyncParts[0].ReasoningDisplay; !reflect.DeepEqual(*got, want) {
		t.Fatalf("summary changed by input mutation or unknown delta: %#v", got)
	}
	turn.observeInlineReasoning(core.Event{Type: core.EventReasoningDelta, ReasoningKind: core.ReasoningKindSummary, ReasoningSummaryParts: []string{"**First**", "details continued"}})
	if got := turn.inlineSyncParts[0].ReasoningDisplay; got.Content != "**First**\n\ndetails continued" || !reflect.DeepEqual(got.SummaryParts, []string{"**First**", "details continued"}) {
		t.Fatalf("updated summary = %#v", got)
	}
	if got := snapshot[0].ReasoningDisplay; !reflect.DeepEqual(*got, want) {
		t.Fatalf("published summary snapshot mutated: %#v", got)
	}
}

func TestInlineReasoningDiscardResetsOrderedParts(t *testing.T) {
	turn := &providerTurnFrame{}
	turn.observeInlineReasoning(reasoningEvent("discarded"))
	turn.appendInlineText("discarded text")
	turn.observeInlineReasoning(core.Event{Type: core.EventAttemptDiscard})
	turn.observeInlineReasoning(reasoningEvent("kept"))
	turn.appendInlineText("kept text")
	if got, want := displayedInlineParts(core.Message{Parts: turn.inlineSyncParts}), []string{"thought:kept", "text:kept text"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("parts after discard = %v, want %v", got, want)
	}
}
