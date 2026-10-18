package cmd

import (
	"context"
	"strings"

	"github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/tools"
)

// ContinueAgent starts another execution on the same child session. The
// transcript is repaired before the runner can sanitize or send it to a model.
func (r *SpawnAgentRunner) ContinueAgent(ctx context.Context, id, name, instructions string, depth int, callID string, opts tools.SpawnAgentRunOptions, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	request := runpkg.ChildRunRequest{Kind: runpkg.ChildRunSpawnAgent, RunID: callID, ChildSessionID: id, AgentName: name, Resume: true, Instructions: instructions, Depth: depth, RemainingDepth: opts.RemainingDepth, ModelOverride: opts.ModelOverride, BaseDir: opts.BaseDir}
	callback := func(runID string, event tools.SubagentEvent) { cb(runID, event) }
	result, err := r.runChildInternal(ctx, request, callback)
	return tools.SpawnAgentRunResult{Output: result.Output, SessionID: result.ChildSessionID, Interventions: result.Interventions, InterventionDisposition: result.InterventionDisposition, CancelledByUser: result.CancelledByUser}, err
}

func (r *SpawnAgentRunner) SteerAgent(id, instructions string) (string, string) {
	if strings.TrimSpace(instructions) == "" {
		return "", "undelivered"
	}
	r.enginesMu.Lock()
	engine := r.engines[id]
	r.enginesMu.Unlock()
	if engine == nil {
		return "", "undelivered"
	}
	steeringID, status := engine.QueueSteeringWithStatus(llm.QueuedSteering{Message: llm.UserText(instructions)})
	if status != llm.SteeringQueueQueued && status != llm.SteeringQueueAlreadyQueued && status != llm.SteeringQueueCommitted {
		return steeringID, "undelivered"
	}
	if status == llm.SteeringQueueCommitted {
		return steeringID, "consumed"
	}
	return steeringID, "queued"
}

func (r *SpawnAgentRunner) loadChildResumeHistory(ctx context.Context, id string) ([]llm.Message, error) {
	return runtime.RepairChildResumeHistory(ctx, r.store, id)
}
