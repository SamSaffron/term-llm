package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

// ContinueAgent starts another execution on the same child session. The
// transcript is repaired before the runner can sanitize or send it to a model.
func (r *SpawnAgentRunner) ContinueAgent(ctx context.Context, id, name, instructions string, depth int, callID string, opts tools.SpawnAgentRunOptions, cb tools.SubagentEventCallback) (tools.SpawnAgentRunResult, error) {
	request := runpkg.ChildRunRequest{Kind: runpkg.ChildRunSpawnAgent, RunID: callID, ChildSessionID: id, AgentName: name, Resume: true, Instructions: instructions, Depth: depth, ModelOverride: opts.ModelOverride, BaseDir: opts.BaseDir}
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
	if r.store == nil {
		return nil, errors.New("agent transcript persistence is unavailable")
	}
	sess, err := r.store.Get(ctx, id)
	if err != nil || sess == nil {
		return nil, fmt.Errorf("load child session %q: %w", id, err)
	}
	stored, err := session.LoadActiveMessages(ctx, r.store, sess)
	if err != nil {
		return nil, fmt.Errorf("load child transcript: %w", err)
	}
	seen := make(map[string]bool)
	for i := range stored {
		if stored[i].Role == llm.RoleTool {
			for _, p := range stored[i].Parts {
				if p.Type == llm.PartToolResult && p.ToolResult != nil {
					seen[p.ToolResult.ID] = true
				}
			}
		}
	}
	var repaired []llm.Message
	history := make([]llm.Message, 0, len(stored))
	for i := range stored {
		msg := stored[i].ToLLMMessage()
		history = append(history, msg)
		if msg.Role != llm.RoleAssistant {
			continue
		}
		for _, p := range msg.Parts {
			if p.Type != llm.PartToolCall || p.ToolCall == nil || p.ToolCall.ID == "" || seen[p.ToolCall.ID] {
				continue
			}
			seen[p.ToolCall.ID] = true
			synthetic := llm.ToolErrorMessage(p.ToolCall.ID, p.ToolCall.Name, "interrupted; side effects unknown — verify before retrying", nil)
			repaired = append(repaired, synthetic)
		}
	}
	for _, msg := range repaired {
		if err := r.store.AddMessage(ctx, id, session.NewMessage(id, msg, -1)); err != nil {
			return nil, fmt.Errorf("persist child tool-call repair: %w", err)
		}
		history = append(history, msg)
	}
	return history, nil
}
