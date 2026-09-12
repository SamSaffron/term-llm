package cmd

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/tools"
)

func TestServeSubagentProgressParentDoesNotTimeoutWhileChildRuns(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(30*time.Minute, clock)
	defer timer.stop()
	var mu sync.Mutex
	var payloads []map[string]any
	progress := newServeSubagentProgress(clock, func(_ string, payload map[string]any) error {
		mu.Lock()
		payloads = append(payloads, cloneJSONMap(payload))
		mu.Unlock()
		return nil
	}, timer.holdUntil)
	defer progress.close()
	progress.begin("spawn-1", tools.SpawnAgentToolName)
	progress.observe("spawn-1", tools.SubagentEvent{Type: tools.SubagentEventInit, Timestamp: clock.Now(), Deadline: clock.Now().Add(time.Hour)})

	clock.Advance(45 * time.Minute)
	if ctx.Err() != nil {
		t.Fatalf("parent timed out under verified child deadline: %v", context.Cause(ctx))
	}
	progress.observe("spawn-1", tools.SubagentEvent{Type: tools.SubagentEventDone, Timestamp: clock.Now()})
	clock.Advance(29 * time.Minute)
	if ctx.Err() != nil {
		t.Fatalf("fresh parent window expired early: %v", context.Cause(ctx))
	}
	clock.Advance(2 * time.Minute)
	if !responseRunTimedOut(ctx) {
		t.Fatalf("parent remained unbounded after child completion: %v", context.Cause(ctx))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(payloads) == 0 {
		t.Fatal("no progress snapshots emitted")
	}
}

func TestServeSubagentProgressStuckChildCannotHoldPastDeadline(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(30*time.Minute, clock)
	defer timer.stop()
	progress := newServeSubagentProgress(clock, nil, timer.holdUntil)
	defer progress.close()
	progress.begin("spawn-1", tools.SpawnAgentToolName)
	progress.observe("spawn-1", tools.SubagentEvent{Type: tools.SubagentEventInit, Timestamp: clock.Now(), Deadline: clock.Now().Add(10 * time.Minute)})
	clock.Advance(10*time.Minute + responseRunHoldGrace)
	if ctx.Err() != nil {
		t.Fatalf("expiry should resume frozen budget, not immediately time out: %v", context.Cause(ctx))
	}
	clock.Advance(30 * time.Minute)
	if !responseRunTimedOut(ctx) {
		t.Fatalf("stuck child kept parent alive: %v", context.Cause(ctx))
	}
}

func TestServeSubagentProgressCountsDeduplicateAndSanitize(t *testing.T) {
	clock := newFakeResponseRunClock()
	var payloads []map[string]any
	progress := newServeSubagentProgress(clock, func(_ string, payload map[string]any) error {
		payloads = append(payloads, cloneJSONMap(payload))
		return nil
	}, nil)
	defer progress.close()
	progress.begin("outer", tools.SpawnAgentToolName)
	unsafe := tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "tool-1", ToolName: "shell", ToolArgs: []byte(`{"secret":true}`), ToolInfo: "secret info", Text: "secret reasoning"}
	progress.observe("outer", unsafe)
	progress.observe("outer", unsafe)
	progress.observe("outer/inner", tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "tool-2", ToolName: "grep"})
	progress.finish("outer", true, false)
	last := payloads[len(payloads)-1]
	if got := responseRunInt64Value(last["calls_started"], -1); got != 1 {
		t.Fatalf("direct calls_started = %d, want 1", got)
	}
	if got := responseRunInt64Value(last["calls_active"], -1); got != 0 {
		t.Fatalf("calls_active = %d, want 0", got)
	}
	for _, forbidden := range []string{"tool_arguments", "tool_info", "output", "text", "reasoning"} {
		if _, exists := last[forbidden]; exists {
			t.Fatalf("snapshot leaked %q: %#v", forbidden, last)
		}
	}
	children, ok := last["children"].([]map[string]any)
	if !ok || len(children) != 1 || children[0]["id"] != "outer/inner" || responseRunInt64Value(children[0]["calls_started"], 0) != 1 {
		t.Fatalf("children = %#v", last["children"])
	}
}

func TestServeRuntimeSubagentCallbackRejectsPreviousExecution(t *testing.T) {
	rt := &serveRuntime{}
	first := newServeSubagentProgress(nil, nil, nil)
	second := newServeSubagentProgress(nil, nil, nil)
	defer first.close()
	defer second.close()

	rt.subagentProgress, rt.subagentProgressOwner = first, 1
	first.begin("reused", tools.SpawnAgentToolName)
	staleCallback := rt.subagentProgressCallback()
	rt.subagentProgress, rt.subagentProgressOwner = second, 2
	second.begin("reused", tools.SpawnAgentToolName)

	staleCallback("reused", tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "old", ToolName: "shell"})
	if got := second.roots["reused"].callsStarted; got != 0 {
		t.Fatalf("later execution received stale callback: calls_started = %d", got)
	}
}

func TestServeSubagentProgressOverflowChildDoesNotCountAsRoot(t *testing.T) {
	progress := newServeSubagentProgress(nil, nil, nil)
	defer progress.close()
	progress.begin("outer", tools.SpawnAgentToolName)
	for i := 0; i < serveSubagentMaxChildren; i++ {
		progress.observe(fmt.Sprintf("outer/child-%d", i), tools.SubagentEvent{Type: tools.SubagentEventInit})
	}
	progress.observe("outer/overflow", tools.SubagentEvent{Type: tools.SubagentEventToolStart, ToolCallID: "nested", ToolName: "shell"})
	if root := progress.roots["outer"]; root.callsStarted != 0 || len(root.activeTools) != 0 {
		t.Fatalf("overflow child was aggregated as root: calls_started=%d active=%d", root.callsStarted, len(root.activeTools))
	}
	root := progress.roots["outer"]
	root.activeTools["outer/overflow\x00nested"] = "root-tool"
	root.currentTool = "root-tool"
	progress.observe("outer/overflow", tools.SubagentEvent{Type: tools.SubagentEventToolEnd, ToolCallID: "nested"})
	if len(root.activeTools) != 1 || root.currentTool != "root-tool" {
		t.Fatalf("overflow child tool end mutated root activity: active=%#v current=%q", root.activeTools, root.currentTool)
	}
}

func TestServeSubagentProgressStaleThenLiveSiblingAcquiresHold(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
	defer timer.stop()
	progress := newServeSubagentProgress(clock, nil, timer.holdUntil)
	defer progress.close()
	progress.begin("wait-1", tools.WaitForJobsToolName)
	progress.observe("wait-1/stale", tools.SubagentEvent{Type: tools.SubagentEventInit, Timestamp: clock.Now().Add(-time.Hour), RunID: "stale"})
	progress.observe("wait-1/live", tools.SubagentEvent{Type: tools.SubagentEventInit, Timestamp: clock.Now(), Deadline: clock.Now().Add(time.Hour), RunID: "live"})
	clock.Advance(time.Minute)
	if ctx.Err() != nil {
		t.Fatalf("live sibling failed to acquire hold after stale sibling: %v", context.Cause(ctx))
	}
}

func TestResponseRunTimerExpiredDeadlineCannotBeExtendedBeforeCallback(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
	defer timer.stop()
	hold := timer.holdUntil(clock.Now().Add(time.Minute))
	clock.mu.Lock()
	clock.now = clock.now.Add(time.Minute + responseRunHoldGrace)
	clock.mu.Unlock()
	hold.extend(clock.Now().Add(time.Hour))
	clock.Advance(0)
	clock.Advance(time.Minute)
	if !responseRunTimedOut(ctx) {
		t.Fatalf("expired hold was revived before delayed expiry callback: %v", context.Cause(ctx))
	}
}

func TestResponseRunRecoveryCarriesSubagentProgress(t *testing.T) {
	run := &responseRun{
		id: "response-1",
		recoveryMessages: []responseRunRecoveryMessage{{
			ID: "tools", Role: "tool-group", Tools: []responseRunRecoveryTool{{ID: "spawn-1", Name: tools.SpawnAgentToolName, Status: "running"}},
		}},
	}
	run.applyRecoveryEventLocked("response.tool_exec.progress", map[string]any{
		"call_id": "spawn-1", "tool_name": tools.SpawnAgentToolName, "seq": int64(4), "state": "running", "calls_started": 7, "calls_active": 1,
	})
	payload := run.recoveryPayloadLocked()
	messages := payload["messages"].([]map[string]any)
	toolsPayload := messages[0]["tools"].([]map[string]any)
	progress := toolsPayload[0]["subagentProgress"].(map[string]any)
	if responseRunInt64Value(progress["calls_started"], 0) != 7 {
		t.Fatalf("recovery progress = %#v", progress)
	}
}

func TestServeSubagentProgressHistoricalQueuedEventCannotReviveHold(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
	defer timer.stop()
	progress := newServeSubagentProgress(clock, nil, timer.holdUntil)
	defer progress.close()
	progress.begin("wait-1", tools.WaitForJobsToolName)
	progress.observe("wait-1/run-old", tools.SubagentEvent{Type: tools.SubagentEventToolStart, EventID: 10, Timestamp: clock.Now().Add(-time.Hour), Deadline: clock.Now().Add(time.Hour), RunID: "run-old", ToolCallID: "tool", ToolName: "shell"})
	clock.Advance(time.Minute)
	if !responseRunTimedOut(ctx) {
		t.Fatalf("historical queued event revived hold: %v", context.Cause(ctx))
	}
}

func TestResponseRunTimerHoldReleaseRefreshesAndStopIsFinal(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
	hold := timer.holdUntil(clock.Now().Add(time.Hour))
	clock.Advance(30 * time.Minute)
	if ctx.Err() != nil {
		t.Fatalf("timer expired under hold: %v", context.Cause(ctx))
	}
	hold.release()
	clock.Advance(59 * time.Second)
	if ctx.Err() != nil {
		t.Fatalf("released hold did not refresh window: %v", context.Cause(ctx))
	}
	timer.stop()
	hold.extend(clock.Now().Add(time.Hour))
	hold.release()
	if !errors.Is(context.Cause(ctx), context.Canceled) {
		t.Fatalf("stop cause = %v, want cancellation", context.Cause(ctx))
	}
}

func TestResponseRunTimerHoldHasFixedAbsoluteCap(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
	defer timer.stop()
	hold := timer.holdUntil(clock.Now().Add(time.Hour))
	clock.Advance(30 * time.Minute)
	hold.extend(clock.Now().Add(time.Hour))
	clock.Advance(maxResponseRunDelegationHold - 30*time.Minute)
	if ctx.Err() != nil {
		t.Fatalf("hold cap should resume frozen budget first: %v", context.Cause(ctx))
	}
	clock.Advance(time.Minute)
	if !responseRunTimedOut(ctx) {
		t.Fatalf("extended hold slid beyond acquisition cap: %v", context.Cause(ctx))
	}
}

func TestServeSubagentProgressOverflowDoneDoesNotReleaseSiblingHold(t *testing.T) {
	clock := newFakeResponseRunClock()
	ctx, timer := newResponseRunTimerWithClock(time.Minute, clock)
	defer timer.stop()
	progress := newServeSubagentProgress(clock, nil, timer.holdUntil)
	defer progress.close()
	progress.begin("wait", tools.WaitForJobsToolName)
	for i := 0; i < serveSubagentMaxChildren+1; i++ {
		id := fmt.Sprintf("run-%d", i)
		progress.observe("wait/"+id, tools.SubagentEvent{Type: tools.SubagentEventInit, Timestamp: clock.Now(), Deadline: clock.Now().Add(time.Hour), RunID: id})
	}
	progress.observe(fmt.Sprintf("wait/run-%d", serveSubagentMaxChildren), tools.SubagentEvent{Type: tools.SubagentEventDone})
	clock.Advance(2 * time.Minute)
	if ctx.Err() != nil {
		t.Fatalf("overflow child's completion released sibling protection: %v", context.Cause(ctx))
	}
}
