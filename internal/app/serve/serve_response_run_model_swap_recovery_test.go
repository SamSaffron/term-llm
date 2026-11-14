package serve

import (
	"testing"
	"time"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
)

func TestResponseRunSnapshotRecoversModelSwapProgress(t *testing.T) {
	run := newResponseRun("resp_swap", "sess_test", "", "target-model", time.Now().Unix(), func() {})
	run.maxRetainedEvents = 1
	appendModelSwapRecoveryTestEvent(t, run, "response.created", map[string]any{
		"response": map[string]any{"provider": "target-provider", "model": "target-model", "reasoning_effort": "high"},
	})
	appendModelSwapRecoveryTestEvent(t, run, "response.approval.prompt", map[string]any{"approval_id": "approval-1"})
	for _, stage := range []string{"naive_start", "handover_start", "handover_done"} {
		appendModelSwapRecoveryTestEvent(t, run, "response.model_swap.progress", map[string]any{
			"stage": stage, "message": "progress: " + stage,
			"previous_provider": "old-provider", "target_provider": "target-provider",
			"previous_model": "old-model", "target_model": "target-model",
		})
	}
	// Recovery must survive eviction from the bounded SSE replay window.
	appendModelSwapRecoveryTestEvent(t, run, "response.activity", map[string]any{"activity": "preparing"})

	snapshot := run.snapshot()
	if snapshot["status"] != "in_progress" || snapshot["provider"] != "target-provider" || snapshot["model"] != "target-model" || snapshot["reasoning_effort"] != "high" {
		t.Fatalf("snapshot runtime identity = %#v", snapshot)
	}
	events := modelSwapRecoveryTestEvents(t, snapshot)
	if len(events) != 2 || events[0]["event"] != "response.approval.prompt" || events[1]["event"] != "response.model_swap.progress" {
		t.Fatalf("recovery events = %#v, want approval and latest swap progress", events)
	}
	payload, ok := events[1]["payload"].(map[string]any)
	if !ok || payload["stage"] != "handover_done" || payload["message"] != "progress: handover_done" || payload["target_provider"] != "target-provider" {
		t.Fatalf("recovered progress = %#v", events[1]["payload"])
	}
	if payload["response_id"] != "resp_swap" || responseRunInt64Value(payload["sequence_number"], 0) != 5 {
		t.Fatalf("recovered progress identity = %#v", payload)
	}

}

func TestResponseRunModelSwitchSnapshotMetadata(t *testing.T) {
	run := newResponseRun("resp_switch", "sess_test", "", "old-model", time.Now().Unix(), func() {})
	appendModelSwapRecoveryTestEvent(t, run, "response.created", map[string]any{
		"response": map[string]any{"provider": "old", "model": "old-model", "reasoning_effort": "high"},
	})
	appendModelSwapRecoveryTestEvent(t, run, "response.model_switch", map[string]any{
		"provider": "old", "model": "old-model", "reasoning_effort": "high",
		"to_provider": "new", "to_model": "new-model", "to_reasoning_effort": "",
	})
	snapshot := run.snapshot()
	if snapshot["provider"] != "new" || snapshot["model"] != "new-model" || snapshot["reasoning_effort"] != "" {
		t.Fatalf("snapshot did not prefer destination metadata: %#v", snapshot)
	}
}

func TestResponseRunModelSwapProgressSnapshotMetadata(t *testing.T) {
	for _, tc := range []struct {
		stage, provider, model, effort string
	}{
		{stage: "naive_start", provider: "created-provider", model: "created-model", effort: "medium"},
		{stage: "handover_start", provider: "created-provider", model: "created-model", effort: "medium"},
		{stage: "handover_done", provider: "created-provider", model: "created-model", effort: "medium"},
		{stage: "complete", provider: "target-provider", model: "target-model", effort: ""},
		{stage: "failed", provider: "previous-provider", model: "previous-model", effort: "high"},
	} {
		t.Run(tc.stage, func(t *testing.T) {
			run := newResponseRun("resp_progress_metadata", "sess_test", "", "created-model", time.Now().Unix(), func() {})
			appendModelSwapRecoveryTestEvent(t, run, "response.created", map[string]any{
				"response": map[string]any{"provider": "created-provider", "model": "created-model", "reasoning_effort": "medium"},
			})
			appendModelSwapRecoveryTestEvent(t, run, "response.model_swap.progress", map[string]any{
				"stage":             tc.stage,
				"previous_provider": "previous-provider", "previous_model": "previous-model", "previous_effort": "high",
				"target_provider": "target-provider", "target_model": "target-model", "target_effort": "",
			})
			snapshot := run.snapshot()
			if snapshot["provider"] != tc.provider || snapshot["model"] != tc.model || snapshot["reasoning_effort"] != tc.effort {
				t.Fatalf("snapshot runtime identity = %#v, want %s/%s/%q", snapshot, tc.provider, tc.model, tc.effort)
			}
			if tc.stage == "failed" {
				if _, err := run.fail(map[string]any{"response": map[string]any{"model": "target-model"}}, "invalid_request_error", "swap failed"); err != nil {
					t.Fatal(err)
				}
				snapshot = run.snapshot()
				if snapshot["status"] != "failed" || snapshot["provider"] != tc.provider || snapshot["model"] != tc.model || snapshot["reasoning_effort"] != tc.effort {
					t.Fatalf("failed snapshot lost rolled-back identity: %#v", snapshot)
				}
				if events := modelSwapRecoveryTestEvents(t, snapshot); len(events) != 0 {
					t.Fatalf("failed snapshot retained transient progress: %#v", events)
				}
			}
		})
	}
}

func TestResponseRunModelSwapProgressRecoveryCleanup(t *testing.T) {
	for _, tc := range []struct {
		name    string
		event   string
		payload map[string]any
	}{
		{name: "progress complete", event: "response.model_swap.progress", payload: map[string]any{"stage": "complete"}},
		{name: "progress failed", event: "response.model_swap.progress", payload: map[string]any{"stage": "failed"}},
		{name: "model switch", event: "response.model_switch", payload: map[string]any{"to_provider": "new-provider", "to_model": "new-model"}},
		{name: "text delta", event: "response.output_text.delta", payload: map[string]any{"delta": "answer"}},
		{name: "text segment", event: "response.output_text.new_segment"},
		{name: "reasoning delta", event: "response.reasoning.delta", payload: map[string]any{"delta": "thinking"}},
		{name: "output item added", event: "response.output_item.added"},
		{name: "output item done", event: "response.output_item.done"},
		{name: "tool start", event: "response.tool_exec.start"},
		{name: "tool end", event: "response.tool_exec.end"},
		{name: "attempt discard", event: "response.attempt.discard"},
		{name: "response completed", event: "response.completed"},
		{name: "response failed", event: "response.failed"},
		{name: "response cancelled", event: "response.cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := newResponseRun("resp_cleanup", "sess_test", "", "model", time.Now().Unix(), func() {})
			appendModelSwapRecoveryTestEvent(t, run, "response.approval.prompt", map[string]any{"approval_id": "approval-1"})
			appendModelSwapRecoveryTestEvent(t, run, "response.model_swap.progress", map[string]any{"stage": "handover_start"})
			before := modelSwapRecoveryTestEvents(t, run.snapshot())
			if len(before) != 2 {
				t.Fatalf("recovery events before cleanup = %#v, want approval and progress", before)
			}
			appendModelSwapRecoveryTestEvent(t, run, tc.event, tc.payload)
			events := modelSwapRecoveryTestEvents(t, run.snapshot())
			if len(events) != 1 || events[0]["event"] != "response.approval.prompt" {
				t.Fatalf("recovery events after cleanup = %#v, want only unrelated approval", events)
			}
		})
	}
}

func TestResponseRunCancelledModelSwapSnapshotMetadata(t *testing.T) {
	t.Run("committed target", func(t *testing.T) {
		f := startStoppableSwap(t, 0)
		f.stop(t)
		snapshot := f.run.snapshot()
		if snapshot["status"] != "cancelled" || snapshot["provider"] != "new" || snapshot["model"] != "new-model" || snapshot["reasoning_effort"] != "" {
			t.Fatalf("cancelled snapshot lost committed target identity: %#v", snapshot)
		}
		if events := modelSwapRecoveryTestEvents(t, snapshot); len(events) != 0 {
			t.Fatalf("cancelled snapshot retained transient progress: %#v", events)
		}
	})
	t.Run("rolled back before output", func(t *testing.T) {
		summarizing := make(chan struct{})
		f := newSwapFixture(t, 1, func() core.Provider {
			return &cancelledSwapProvider{MockProvider: mockprovider.NewMockProvider("old"), started: summarizing}
		})
		f.start(t)
		select {
		case <-summarizing:
		case <-time.After(5 * time.Second):
			t.Fatal("handover summary did not start")
		}
		f.stop(t)
		snapshot := f.run.snapshot()
		if snapshot["status"] != "cancelled" || snapshot["provider"] != "old" || snapshot["model"] != "old-model" || snapshot["reasoning_effort"] != "" {
			t.Fatalf("cancelled snapshot lost rolled-back identity: %#v", snapshot)
		}
		if events := modelSwapRecoveryTestEvents(t, snapshot); len(events) != 0 {
			t.Fatalf("cancelled snapshot retained transient progress: %#v", events)
		}
	})
}

func appendModelSwapRecoveryTestEvent(t *testing.T, run *responseRun, event string, payload map[string]any) {
	t.Helper()
	if err := run.appendEvent(event, payload); err != nil {
		t.Fatalf("append %s: %v", event, err)
	}
}

func modelSwapRecoveryTestEvents(t *testing.T, snapshot map[string]any) []map[string]any {
	t.Helper()
	recovery, ok := snapshot["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("snapshot recovery = %#v", snapshot["recovery"])
	}
	if recovery["events"] == nil {
		return nil
	}
	events, ok := recovery["events"].([]map[string]any)
	if !ok {
		t.Fatalf("recovery events = %#v", recovery["events"])
	}
	return events
}
