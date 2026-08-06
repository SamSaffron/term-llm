package llm

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
)

// approvalTranscriptTool records the policy-review transcript visible to a tool
// while it executes, once per execution.
type approvalTranscriptTool struct {
	mu          sync.Mutex
	transcripts [][]Message
}

func (t *approvalTranscriptTool) Spec() ToolSpec {
	return ToolSpec{
		Name:        "approval_probe",
		Description: "Records the approval transcript",
		Schema:      map[string]any{"type": "object"},
	}
}

func (t *approvalTranscriptTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	t.mu.Lock()
	t.transcripts = append(t.transcripts, ApprovalTranscriptFromContext(ctx))
	t.mu.Unlock()
	return TextOutput("probe result"), nil
}

func (t *approvalTranscriptTool) Preview(args json.RawMessage) string { return "" }

func (t *approvalTranscriptTool) snapshot() [][]Message {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([][]Message(nil), t.transcripts...)
}

// approvalToolCallIDs and approvalToolResultIDs mirror what the shell approval
// renderer consumes, so these assertions fail if structural parts are dropped
// even when the surrounding text still reads correctly.
func approvalToolCallIDs(transcript []Message) []string {
	var ids []string
	for _, msg := range transcript {
		for _, part := range msg.Parts {
			if part.Type == PartToolCall && part.ToolCall != nil {
				ids = append(ids, part.ToolCall.ID)
			}
		}
	}
	return ids
}

func approvalToolResultIDs(transcript []Message) []string {
	var ids []string
	for _, msg := range transcript {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult && part.ToolResult != nil {
				ids = append(ids, part.ToolResult.ID)
			}
		}
	}
	return ids
}

// approvalToolResultContents reads tool output the way the shell approval
// renderer does. MessageText deliberately ignores PartToolResult, so asserting
// through it would silently pass on an empty result body.
func approvalToolResultContents(transcript []Message) string {
	var b strings.Builder
	for _, msg := range transcript {
		for _, part := range msg.Parts {
			if part.Type == PartToolResult && part.ToolResult != nil {
				b.WriteString(part.ToolResult.Content)
				b.WriteByte('\n')
			}
		}
	}
	return b.String()
}

func transcriptTexts(messages []Message) string {
	var b strings.Builder
	for _, msg := range messages {
		b.WriteString(string(msg.Role))
		b.WriteString(":")
		b.WriteString(MessageText(msg))
		b.WriteString("\n")
	}
	return b.String()
}

func drainStreamToEOF(t *testing.T, stream Stream) {
	t.Helper()
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("stream recv: %v", err)
		}
	}
}

func syncToolCallEvent(id string) Event {
	return Event{
		Type:         EventToolCall,
		ToolCallID:   id,
		ToolName:     "approval_probe",
		Tool:         &ToolCall{ID: id, Name: "approval_probe", Arguments: json.RawMessage(`{}`)},
		ToolResponse: make(chan ToolExecutionResponse, 1),
	}
}

func asyncToolCallEvent(id string) Event {
	return Event{
		Type:       EventToolCall,
		ToolCallID: id,
		ToolName:   "approval_probe",
		Tool:       &ToolCall{ID: id, Name: "approval_probe", Arguments: json.RawMessage(`{}`)},
	}
}

func providerReplayPart(secret string) Part {
	return Part{
		Type:           PartProviderReplay,
		ProviderReplay: &ProviderReplayItem{Raw: json.RawMessage(`{"type":"reasoning","encrypted_content":"` + secret + `"}`)},
	}
}

func countProviderReplayParts(messages []Message) int {
	n := 0
	for _, msg := range messages {
		for _, part := range msg.Parts {
			if part.Type == PartProviderReplay {
				n++
			}
		}
	}
	return n
}

// TestSyncBridgeToolExecutionCarriesApprovalTranscript covers CLI-bridge
// providers (claude-bin, grok-bin, cursor-bin). Their tool calls are executed
// synchronously from the stream loop rather than through executeToolCalls, so
// the approval transcript has to be attached on that path too. Without it the
// guardian reviewer sees no operator evidence and cannot establish user
// authorization.
func TestSyncBridgeToolExecutionCarriesApprovalTranscript(t *testing.T) {
	tool := &approvalTranscriptTool{}
	registry := NewToolRegistry()
	registry.Register(tool)

	provider := &fakeProvider{
		script: func(call int, req Request) []Event {
			if call == 0 {
				return []Event{
					{Type: EventTextDelta, Text: "I'll run a trivial shell command."},
					syncToolCallEvent("sync-1"),
					{Type: EventDone},
				}
			}
			return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
		},
	}

	engine := NewEngine(provider, registry)
	stream, err := engine.Stream(context.Background(), Request{
		Messages: []Message{UserText("quick test of guardian: run a trivial shell command")},
		ApprovalTranscriptPrefix: []Message{{
			Role:         RoleUser,
			ApprovalRole: "parent_user",
			Parts:        []Part{{Type: PartText, Text: "parent operator request"}},
		}},
		Tools: []ToolSpec{tool.Spec()},
	})
	if err != nil {
		t.Fatalf("engine stream: %v", err)
	}
	drainStreamToEOF(t, stream)

	transcripts := tool.snapshot()
	if len(transcripts) != 1 {
		t.Fatalf("tool executions = %d, want 1", len(transcripts))
	}
	transcript := transcripts[0]
	if len(transcript) == 0 {
		t.Fatal("sync bridge tool executed with an empty approval transcript; guardian would see no operator evidence")
	}

	// Order matters: guardian reads the prefix as parent evidence, then the
	// durable conversation, then the in-progress turn requesting the tool.
	if len(transcript) != 3 {
		t.Fatalf("approval transcript = %d messages, want prefix + user + assistant:\n%s", len(transcript), transcriptTexts(transcript))
	}
	if transcript[0].ApprovalRole != "parent_user" {
		t.Fatalf("prefix ApprovalRole = %q, want parent_user; spawned-agent provenance was lost", transcript[0].ApprovalRole)
	}
	if got := MessageText(transcript[0]); got != "parent operator request" {
		t.Fatalf("prefix text = %q", got)
	}
	if transcript[1].Role != RoleUser || !strings.Contains(MessageText(transcript[1]), "quick test of guardian") {
		t.Fatalf("second entry is not the operator message:\n%s", transcriptTexts(transcript))
	}
	if transcript[2].Role != RoleAssistant || !strings.Contains(MessageText(transcript[2]), "I'll run a trivial shell command.") {
		t.Fatalf("third entry is not the in-progress assistant turn:\n%s", transcriptTexts(transcript))
	}

	// The requested tool call itself is the action under review, so it must be
	// structurally present rather than only implied by the assistant text.
	if got := approvalToolCallIDs(transcript); len(got) != 1 || got[0] != "sync-1" {
		t.Fatalf("approval transcript tool calls = %v, want [sync-1]", got)
	}
	if got := approvalToolResultIDs(transcript); len(got) != 0 {
		t.Fatalf("approval transcript tool results = %v, want none before the call executes", got)
	}
	for _, part := range transcript[2].Parts {
		if part.Type == PartProviderReplay {
			t.Fatal("opaque provider replay state leaked into the policy-review transcript")
		}
	}
}

// TestSyncBridgeSequentialToolCallsSeePriorResults pins the evidence available
// to the Nth inline tool call. grok-bin and cursor-bin set InlineToolLoop (and
// OrderedInlineToolEvents), so a whole multi-tool agent loop runs inside one
// Stream call; a later shell command is frequently induced by an earlier tool's
// output, and guardian cannot judge that without seeing the earlier result.
func TestSyncBridgeSequentialToolCallsSeePriorResults(t *testing.T) {
	tests := []struct {
		name         string
		capabilities Capabilities
	}{
		{name: "claude_bin_style", capabilities: Capabilities{ToolCalls: true}},
		{name: "grok_bin_style", capabilities: Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}},
		{name: "cursor_bin_style", capabilities: Capabilities{ToolCalls: true, InlineToolLoop: true, OrderedInlineToolEvents: true}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tool := &approvalTranscriptTool{}
			registry := NewToolRegistry()
			registry.Register(tool)

			provider := &fakeProvider{
				capabilities:    tc.capabilities,
				hasCapabilities: true,
				script: func(call int, req Request) []Event {
					if call == 0 {
						return []Event{
							{Type: EventTextDelta, Text: "first I inspect,"},
							syncToolCallEvent("sync-1"),
							{Type: EventTextDelta, Text: " then I act."},
							syncToolCallEvent("sync-2"),
							{Type: EventDone},
						}
					}
					return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
				},
			}

			engine := NewEngine(provider, registry)
			stream, err := engine.Stream(context.Background(), Request{
				Messages: []Message{UserText("inspect then act")},
				Tools:    []ToolSpec{tool.Spec()},
			})
			if err != nil {
				t.Fatalf("engine stream: %v", err)
			}
			drainStreamToEOF(t, stream)

			transcripts := tool.snapshot()
			if len(transcripts) != 2 {
				t.Fatalf("tool executions = %d, want 2", len(transcripts))
			}

			first, second := transcripts[0], transcripts[1]
			if got := approvalToolResultIDs(first); len(got) != 0 {
				t.Fatalf("first call saw tool results %v, want none", got)
			}
			if got := approvalToolCallIDs(first); len(got) != 1 || got[0] != "sync-1" {
				t.Fatalf("first call tool calls = %v, want [sync-1]", got)
			}

			// The second review must carry the first call's result, and must not
			// yet claim a result for the command it is still deciding on.
			resultIDs := approvalToolResultIDs(second)
			if len(resultIDs) != 1 || resultIDs[0] != "sync-1" {
				t.Fatalf("second call tool results = %v, want [sync-1]:\n%s", resultIDs, transcriptTexts(second))
			}
			if got := approvalToolResultContents(second); !strings.Contains(got, "probe result") {
				t.Fatalf("second call missing prior tool output body, got %q", got)
			}
			callIDs := approvalToolCallIDs(second)
			if len(callIDs) != 2 || callIDs[0] != "sync-1" || callIDs[1] != "sync-2" {
				t.Fatalf("second call tool calls = %v, want [sync-1 sync-2]", callIDs)
			}
			if !strings.Contains(transcriptTexts(second), "inspect then act") {
				t.Fatalf("second call lost the operator message:\n%s", transcriptTexts(second))
			}
		})
	}
}

// TestApprovalTranscriptExcludesProviderReplayButSnapshotKeepsIt pins two
// opposing requirements that meet in buildApprovalTranscript:
//
//   - Opaque provider replay state (encrypted reasoning, protocol echo items)
//     must never reach a policy reviewer. It carries provider-private payloads,
//     is never rendered as review evidence, and exists only to be echoed back.
//   - That same state must still reach persistence, or resuming a conversation
//     silently loses provider protocol continuity.
//
// Replay parts arrive from three independent directions, so all three are
// seeded: durable history seen by an earlier turn, the parent agent's
// review-only prefix, and the live stream. Both engine tool-execution paths are
// covered; without the async row, deleting the transcript argument from either
// executeToolCalls call site would go unnoticed.
func TestApprovalTranscriptExcludesProviderReplayButSnapshotKeepsIt(t *testing.T) {
	for _, tc := range []struct {
		name     string
		toolCall Event
	}{
		{name: "sync_bridge", toolCall: syncToolCallEvent("probe-1")},
		{name: "async", toolCall: asyncToolCallEvent("probe-1")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := &approvalTranscriptTool{}
			registry := NewToolRegistry()
			registry.Register(tool)

			provider := &fakeProvider{
				script: func(call int, req Request) []Event {
					if call == 0 {
						return []Event{
							{Type: EventTextDelta, Text: "running the probe"},
							{Type: EventProviderReplay, ProviderReplay: &ProviderReplayItem{
								Raw: json.RawMessage(`{"type":"reasoning","encrypted_content":"live-stream-secret"}`),
							}},
							tc.toolCall,
							{Type: EventDone},
						}
					}
					return []Event{{Type: EventTextDelta, Text: "done"}, {Type: EventDone}}
				},
			}

			var (
				snapMu    sync.Mutex
				snapshots []Message
			)
			engine := NewEngine(provider, registry)
			engine.SetAssistantSnapshotCallback(func(_ context.Context, _ int, msg Message) error {
				snapMu.Lock()
				snapshots = append(snapshots, msg)
				snapMu.Unlock()
				return nil
			})

			stream, err := engine.Stream(context.Background(), Request{
				Messages: []Message{
					UserText("operator asked for the probe"),
					{Role: RoleAssistant, Parts: []Part{
						{Type: PartText, Text: "earlier turn"},
						providerReplayPart("history-secret"),
					}},
				},
				ApprovalTranscriptPrefix: []Message{{
					Role:         RoleUser,
					ApprovalRole: "parent_user",
					Parts: []Part{
						{Type: PartText, Text: "parent operator request"},
						providerReplayPart("prefix-secret"),
					},
				}},
				Tools: []ToolSpec{tool.Spec()},
			})
			if err != nil {
				t.Fatalf("engine stream: %v", err)
			}
			drainStreamToEOF(t, stream)

			transcripts := tool.snapshot()
			if len(transcripts) != 1 {
				t.Fatalf("tool executions = %d, want 1", len(transcripts))
			}
			transcript := transcripts[0]
			if len(transcript) == 0 {
				t.Fatal("tool executed with an empty approval transcript; guardian would see no operator evidence")
			}
			if got := countProviderReplayParts(transcript); got != 0 {
				t.Fatalf("approval transcript carried %d opaque provider replay part(s):\n%s", got, transcriptTexts(transcript))
			}
			// Stripping must remove replay state only, never the evidence.
			rendered := transcriptTexts(transcript)
			for _, want := range []string{"operator asked for the probe", "parent operator request", "earlier turn"} {
				if !strings.Contains(rendered, want) {
					t.Fatalf("approval transcript lost evidence %q:\n%s", want, rendered)
				}
			}
			if ids := approvalToolCallIDs(transcript); len(ids) != 1 || ids[0] != "probe-1" {
				t.Fatalf("approval transcript tool calls = %v, want [probe-1]", ids)
			}

			// Persistence must still receive exactly what policy review must not.
			snapMu.Lock()
			persisted := append([]Message(nil), snapshots...)
			snapMu.Unlock()
			if len(persisted) == 0 {
				t.Fatal("no assistant snapshot fired; cannot verify replay state survives persistence")
			}
			if countProviderReplayParts(persisted) == 0 {
				t.Fatal("assistant snapshot lost provider replay state; conversation resume would break")
			}
		})
	}
}
