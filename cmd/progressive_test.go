package cmd

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

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
