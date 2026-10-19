package progressive

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/jobs"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	toolpkg "github.com/samsaffron/term-llm/internal/tools"
)

type progressiveTestTool struct{}

func (t *progressiveTestTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: "progressive_test_tool", Description: "test tool", Schema: map[string]any{"type": "object"}}
}

func (t *progressiveTestTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("tool ok"), nil
}

func (t *progressiveTestTool) Preview(args json.RawMessage) string { return "" }

func (t *progressiveTestTool) IsFinishingTool() bool { return true }

func TestValidateAskProgressiveOptions(t *testing.T) {
	tests := []struct {
		name    string
		opts    AskOptions
		wantErr bool
	}{
		{
			name:    "stop_when without progressive",
			opts:    AskOptions{StopWhen: StopWhenTimeout},
			wantErr: true,
		},
		{
			name:    "continue_with without progressive",
			opts:    AskOptions{ContinueWith: "keep going"},
			wantErr: true,
		},
		{
			name:    "timeout mode requires timeout",
			opts:    AskOptions{Enabled: true, StopWhen: StopWhenTimeout},
			wantErr: true,
		},
		{
			name:    "continue_with requires timeout mode",
			opts:    AskOptions{Enabled: true, ContinueWith: "keep going"},
			wantErr: true,
		},
		{
			name: "valid progressive timeout mode",
			opts: AskOptions{
				Enabled:      true,
				Timeout:      30 * time.Second,
				StopWhen:     StopWhenTimeout,
				ContinueWith: "verify more",
			},
		},
		{
			name: "progressive with timeout defaults stop_when to timeout",
			opts: AskOptions{
				Enabled: true,
				Timeout: 5 * time.Minute,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAskOptions(&tt.opts)
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
	opts := AskOptions{
		Enabled: true,
		Timeout: 5 * time.Minute,
	}
	if err := ValidateAskOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.StopWhen != StopWhenTimeout {
		t.Fatalf("StopWhen = %q, want %q", opts.StopWhen, StopWhenTimeout)
	}
}

func TestValidateProgressiveDefaultsStopWhenToDoneWithoutTimeout(t *testing.T) {
	opts := AskOptions{
		Enabled: true,
	}
	if err := ValidateAskOptions(&opts); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if opts.StopWhen != StopWhenDone {
		t.Fatalf("StopWhen = %q, want %q", opts.StopWhen, StopWhenDone)
	}
}

func TestRunProgressiveSessionFinalizesOnNaturalCompletion(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{
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

	eng := engine.NewEngine(provider, nil)
	result, err := RunSession(context.Background(), eng, core.Request{
		Messages: []core.Message{core.UserText("Investigate X")},
		MaxTurns: 8,
		ToolChoice: core.ToolChoice{
			Mode: core.ToolChoiceAuto,
		},
	}, RunOptions{})
	if err != nil {
		t.Fatalf("RunSession error = %v", err)
	}

	if !result.Finalized {
		t.Fatal("expected finalized result")
	}
	if result.ExitReason != jobs.ExitReasonNatural {
		t.Fatalf("exit reason = %q, want %q", result.ExitReason, jobs.ExitReasonNatural)
	}
	if got := result.Progress["step"]; got != "final" {
		t.Fatalf("progress step = %#v, want %q", got, "final")
	}
	if len(provider.Requests) != 3 {
		t.Fatalf("provider saw %d requests, want %d", len(provider.Requests), 3)
	}
}

func TestRunProgressiveSessionTimeoutDoesNotStartDetachedFinalizationAfterDeadline(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock")
	provider.AddTurn(mockprovider.MockTurn{Delay: 50 * time.Millisecond, Text: "too slow"})
	provider.AddTextResponse("finalization should not run")

	eng := engine.NewEngine(provider, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	result, err := RunSession(ctx, eng, core.Request{
		Messages: []core.Message{core.UserText("Investigate X")},
		MaxTurns: 2,
	}, RunOptions{StopWhen: StopWhenTimeout})
	if err != nil {
		t.Fatalf("RunSession error = %v", err)
	}
	if result.ExitReason != jobs.ExitReasonTimeout {
		t.Fatalf("exit reason = %q, want %q", result.ExitReason, jobs.ExitReasonTimeout)
	}
	if len(provider.Requests) != 1 {
		t.Fatalf("provider saw %d requests, want timeout to skip detached finalization pass", len(provider.Requests))
	}
}

func TestRunProgressiveSessionSyntheticContinuationPersistenceHonorsContext(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{
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

	eng := engine.NewEngine(provider, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5050*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := RunSession(ctx, eng, core.Request{
			Messages: []core.Message{core.UserText("Investigate X")},
			MaxTurns: 8,
			ToolChoice: core.ToolChoice{
				Mode: core.ToolChoiceAuto,
			},
		}, RunOptions{
			StopWhen: StopWhenTimeout,
			OnSyntheticUserMessage: func(ctx context.Context, msg core.Message) error {
				<-ctx.Done()
				return ctx.Err()
			},
		})
		done <- err
	}()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("RunSession error = %v, want deadline exceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("synthetic continuation persistence did not stop when its context expired")
	}
}

func TestAttemptProgressiveFinalizationSyntheticPersistenceHonorsContext(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock")
	eng := engine.NewEngine(provider, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		finalized, text := attemptProgressiveFinalization(ctx, eng, toolpkg.NewFinalizeProgressTool(), core.Request{
			Messages: []core.Message{core.UserText("Investigate X")},
		}, nil, RunOptions{
			OnSyntheticUserMessage: func(ctx context.Context, msg core.Message) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}, NewTracker(), 20*time.Millisecond, jobs.ExitReasonTimeout)
		if finalized {
			t.Errorf("finalized = true, want false")
		}
		if text != "" {
			t.Errorf("text = %q, want empty", text)
		}
		close(done)
	}()

	select {
	case <-done:
		if len(provider.Requests) != 0 {
			t.Fatalf("provider saw %d requests, want synthetic persistence failure to stop finalization before the model call", len(provider.Requests))
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("synthetic finalization persistence did not stop when its context expired")
	}
}

func TestProgressiveFinalizationContextNaturalCompletionDetachesFromParent(t *testing.T) {
	parent, cancelParent := context.WithCancel(context.Background())
	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parent, time.Second, jobs.ExitReasonNatural)
	if finalizeCtx == nil {
		t.Fatal("expected finalization context")
	}
	defer finalizeCancel()

	deadline, ok := finalizeCtx.Deadline()
	if !ok {
		t.Fatal("expected deadline on finalization context")
	}
	if remaining := time.Until(deadline); remaining < DefaultFinalizeGrace-time.Second {
		t.Fatalf("natural finalization remaining = %v, want about %v", remaining, DefaultFinalizeGrace)
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
	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parent, reserve, jobs.ExitReasonTimeout)
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

	finalizeCtx, finalizeCancel := progressiveFinalizationContext(parent, time.Second, jobs.ExitReasonCancelled)
	if finalizeCtx != nil || finalizeCancel != nil {
		t.Fatal("expected cancelled exit to skip finalization when parent is already canceled")
	}
}

func TestRunProgressivePassDoesNotDuplicateProducedAssistantWhenResponseCallbackFails(t *testing.T) {
	provider := mockprovider.NewMockProvider("mock").WithCapabilities(core.Capabilities{ToolCalls: true})
	provider.AddToolCall("call-1", "progressive_test_tool", map[string]any{})
	provider.AddTextResponse("done")

	tool := &progressiveTestTool{}
	registry := engine.NewToolRegistry()
	registry.Register(tool)
	eng := engine.NewEngine(provider, registry)

	result, err := runProgressivePass(context.Background(), eng, core.Request{
		Messages: []core.Message{core.UserText("test")},
		Tools:    []core.ToolSpec{tool.Spec()},
	}, RunOptions{
		OnResponseCompleted: func(context.Context, int, core.Message, core.TurnMetrics) error {
			return context.DeadlineExceeded
		},
	}, NewTracker())
	if err != nil {
		t.Fatalf("runProgressivePass error = %v", err)
	}

	if len(result.produced) != 2 {
		t.Fatalf("produced messages = %d, want assistant + tool result exactly once", len(result.produced))
	}
	if result.produced[0].Role != core.RoleAssistant {
		t.Fatalf("produced[0].Role = %s, want assistant", result.produced[0].Role)
	}
	if result.produced[1].Role != core.RoleTool {
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

	tracker := newProgressTrackerFromMessages([]core.Message{
		{
			Role: core.RoleAssistant,
			Parts: []core.Part{
				{
					Type: core.PartToolCall,
					ToolCall: &core.ToolCall{
						ID:        "progress-1",
						Name:      "update_progress",
						Arguments: callArgs,
					},
				},
			},
		},
		{
			Role: core.RoleTool,
			Parts: []core.Part{
				{
					Type: core.PartToolResult,
					ToolResult: &core.ToolResult{
						ID:      "progress-1",
						Name:    "update_progress",
						Content: "progress saved",
					},
				},
			},
		},
		{
			Role: core.RoleAssistant,
			Parts: []core.Part{
				{
					Type: core.PartToolCall,
					ToolCall: &core.ToolCall{
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
