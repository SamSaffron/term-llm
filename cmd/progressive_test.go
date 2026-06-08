package cmd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

type progressiveTestTool struct{}

func (t *progressiveTestTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{Name: "progressive_test_tool", Description: "test tool", Schema: map[string]any{"type": "object"}}
}

func (t *progressiveTestTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	return llm.TextOutput("tool ok"), nil
}

func (t *progressiveTestTool) Preview(args json.RawMessage) string { return "" }

func (t *progressiveTestTool) IsFinishingTool() bool { return true }

func TestValidateAskProgressiveOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    askProgressiveOptions
		wantErr bool
	}{
		{
			name:    "stop_when without progressive",
			opts:    askProgressiveOptions{StopWhen: progressiveStopWhenTimeout},
			wantErr: true,
		},
		{
			name:    "continue_with without progressive",
			opts:    askProgressiveOptions{ContinueWith: "keep going"},
			wantErr: true,
		},
		{
			name:    "timeout mode requires timeout",
			opts:    askProgressiveOptions{Enabled: true, StopWhen: progressiveStopWhenTimeout},
			wantErr: true,
		},
		{
			name:    "continue_with requires timeout mode",
			opts:    askProgressiveOptions{Enabled: true, ContinueWith: "keep going"},
			wantErr: true,
		},
		{
			name: "valid progressive timeout mode",
			opts: askProgressiveOptions{
				Enabled:      true,
				Timeout:      30 * time.Second,
				StopWhen:     progressiveStopWhenTimeout,
				ContinueWith: "verify more",
			},
		},
		{
			name: "progressive with timeout defaults stop_when to timeout",
			opts: askProgressiveOptions{
				Enabled: true,
				Timeout: 5 * time.Minute,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAskProgressiveOptions(&tt.opts)
			if tt.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestValidateProgressiveDefaultsStopWhenToTimeout(t *testing.T) {
	opts := askProgressiveOptions{
		Enabled: true,
		Timeout: 5 * time.Minute,
	}
	if err := validateAskProgressiveOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.StopWhen != progressiveStopWhenTimeout {
		t.Fatalf("StopWhen = %q, want %q", opts.StopWhen, progressiveStopWhenTimeout)
	}
}

func TestValidateProgressiveDefaultsStopWhenToDoneWithoutTimeout(t *testing.T) {
	opts := askProgressiveOptions{
		Enabled: true,
	}
	if err := validateAskProgressiveOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.StopWhen != progressiveStopWhenDone {
		t.Fatalf("StopWhen = %q, want %q", opts.StopWhen, progressiveStopWhenDone)
	}
}

func TestRunProgressiveSessionFinalizesOnNaturalCompletion(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{
		ToolCalls:          true,
		SupportsToolChoice: true,
	})
	provider.AddToolCall("progress-1", "update_progress", map[string]any{
		"state": map[string]any{
			"step": "draft",
		},
		"reason":  "milestone",
		"message": "draft saved",
	})
	provider.AddTextResponse("draft answer")
	provider.AddToolCall("final-1", "finalize_progress", map[string]any{
		"state": map[string]any{
			"step": "final",
		},
		"message": "final saved",
	})

	engine := llm.NewEngine(provider, nil)
	result, err := runProgressiveSession(context.Background(), engine, llm.Request{
		Messages: []llm.Message{llm.UserText("Investigate X")},
		MaxTurns: 8,
		ToolChoice: llm.ToolChoice{
			Mode: llm.ToolChoiceAuto,
		},
	}, progressiveRunOptions{})
	if err != nil {
		t.Fatalf("runProgressiveSession error = %v", err)
	}

	if !result.Finalized {
		t.Fatal("expected finalized result")
	}
	if result.ExitReason != exitReasonNatural {
		t.Fatalf("exit reason = %q, want %q", result.ExitReason, exitReasonNatural)
	}
	if got := result.Progress["step"]; got != "final" {
		t.Fatalf("progress step = %#v, want %q", got, "final")
	}
	if len(provider.Requests) != 3 {
		t.Fatalf("provider saw %d requests, want %d", len(provider.Requests), 3)
	}
}

func TestRunProgressiveSessionTimeoutDoesNotStartDetachedFinalizationAfterDeadline(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{Delay: 50 * time.Millisecond, Text: "too slow"})
	provider.AddTextResponse("finalization should not run")

	engine := llm.NewEngine(provider, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, err := runProgressiveSession(ctx, engine, llm.Request{
		Messages: []llm.Message{llm.UserText("Investigate X")},
		MaxTurns: 2,
	}, progressiveRunOptions{StopWhen: progressiveStopWhenTimeout})
	if err != nil {
		t.Fatalf("runProgressiveSession error = %v", err)
	}
	if result.ExitReason != exitReasonTimeout {
		t.Fatalf("exit reason = %q, want %q", result.ExitReason, exitReasonTimeout)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want timeout to skip detached finalization pass", len(provider.Requests))
	}
}

func TestProgressiveFinalizationContextNaturalCompletionDetachesFromParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parent, time.Second, exitReasonNatural)
	if finalizeCtx == nil {
		t.Fatal("expected finalization context")
	}
	defer finalizeCancel()

	deadline, ok := finalizeCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline on finalization context")
	}
	if remaining := time.Until(deadline); remaining < progressiveDefaultFinalizeGrace-time.Second {
		t.Fatalf("natural finalization remaining = %v, want about %v", remaining, progressiveDefaultFinalizeGrace)
	}

	cancelParent()
	select {
	case <-finalizeCtx.Done():
		t.Fatal("natural finalization context should not be canceled with parent")
	default:
	}
}

func TestProgressiveFinalizationContextTimeoutUsesReserveAndParentCancellation(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	defer cancelParent()

	reserve := 50 * time.Millisecond
	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parent, reserve, exitReasonTimeout)
	if finalizeCtx == nil {
		t.Fatal("expected finalization context")
	}
	defer finalizeCancel()

	deadline, ok := finalizeCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline on finalization context")
	}
	remaining := time.Until(deadline)
	if remaining <= 0 || remaining > time.Second {
		t.Fatalf("timeout finalization remaining = %v, want reserve-sized budget without 5m grace", remaining)
	}

	cancelParent()
	select {
	case <-finalizeCtx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("timeout finalization context did not stop when parent was canceled")
	}
}

func TestProgressiveFinalizationContextCancelledExitSkipsWhenParentAlreadyCancelled(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()

	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parent, time.Second, exitReasonCancelled)
	if finalizeCtx != nil || finalizeCancel != nil {
		t.Fatal("expected cancelled exit to skip finalization when parent is already canceled")
	}
}

func TestRunProgressivePassDoesNotDuplicateProducedAssistantWhenResponseCallbackFails(t *testing.T) {
	provider := llm.NewMockProvider("mock").WithCapabilities(llm.Capabilities{ToolCalls: true})
	provider.AddToolCall("call-1", "progressive_test_tool", map[string]any{})
	provider.AddTextResponse("done")

	tool := &progressiveTestTool{}
	registry := llm.NewToolRegistry()
	registry.Register(tool)
	engine := llm.NewEngine(provider, registry)

	result, err := runProgressivePass(context.Background(), engine, llm.Request{
		Messages: []llm.Message{llm.UserText("test")},
		Tools:    []llm.ToolSpec{tool.Spec()},
	}, progressiveRunOptions{
		OnResponseCompleted: func(context.Context, int, llm.Message, llm.TurnMetrics) error {
			return context.DeadlineExceeded
		},
	}, newProgressTracker())
	if err != nil {
		t.Fatalf("runProgressivePass error = %v", err)
	}

	if len(result.produced) != 2 {
		t.Fatalf("produced messages = %d, want assistant + tool result exactly once", len(result.produced))
	}
	if result.produced[0].Role != llm.RoleAssistant {
		t.Fatalf("produced[0].Role = %s, want assistant", result.produced[0].Role)
	}
	if result.produced[1].Role != llm.RoleTool {
		t.Fatalf("produced[1].Role = %s, want tool", result.produced[1].Role)
	}
}

func TestNewProgressTrackerFromMessagesSeedsCommittedProgress(t *testing.T) {
	callArgs, err := json.Marshal(map[string]any{
		"state":  map[string]any{"step": "draft"},
		"reason": "milestone",
	})
	if err != nil {
		t.Fatalf("marshal call args: %v", err)
	}

	tracker := newProgressTrackerFromMessages([]llm.Message{
		{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        "progress-1",
						Name:      "update_progress",
						Arguments: callArgs,
					},
				},
			},
		},
		{
			Role: llm.RoleTool,
			Parts: []llm.Part{
				{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      "progress-1",
						Name:    "update_progress",
						Content: "progress saved",
					},
				},
			},
		},
		{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        "orphaned",
						Name:      "update_progress",
						Arguments: callArgs,
					},
				},
			},
		},
	})

	if tracker.latest == nil {
		t.Fatal("expected latest progress commit")
	}
	if got := tracker.latest.State["step"]; got != "draft" {
		t.Fatalf("latest progress step = %#v, want %q", got, "draft")
	}
	if tracker.latest.Sequence != 1 {
		t.Fatalf("latest sequence = %d, want 1", tracker.latest.Sequence)
	}
	if len(tracker.pending) != 0 {
		t.Fatalf("pending progress calls = %d, want 0", len(tracker.pending))
	}
}
