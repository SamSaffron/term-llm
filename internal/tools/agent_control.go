package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
)

const (
	WaitAgentToolName     = "wait_agent"
	ContinueAgentToolName = "continue_agent"
	CancelAgentToolName   = "cancel_agent"
	ListAgentsToolName    = "list_agents"
)

type agentControlTool struct {
	name  string
	spawn *SpawnAgentTool
}

func (t *agentControlTool) Spec() llm.ToolSpec {
	props := map[string]any{}
	required := []string{}
	description := ""
	switch t.name {
	case WaitAgentToolName:
		props["agent_ids"] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Child session IDs to wait for"}
		props["max_wait"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 3600, "description": "Wait budget in seconds; 0 takes a snapshot"}
		required = []string{"agent_ids"}
		description = "Wait for agents without cancelling them; returns their current status."
	case ContinueAgentToolName:
		props["agent_id"] = map[string]any{"type": "string"}
		props["instructions"] = map[string]any{"type": "string", "description": "New instruction for a stopped agent, or steering for a running agent"}
		props["force"] = map[string]any{"type": "boolean", "description": "Force resumption despite possible concurrent work on another host; may duplicate side effects"}
		props["wait"] = map[string]any{"type": "integer", "minimum": 0, "maximum": 3600}
		required = []string{"agent_id"}
		description = "Steer a running agent or resume a stopped agent with a fresh turn budget."
	case CancelAgentToolName:
		props["agent_id"] = map[string]any{"type": "string"}
		required = []string{"agent_id"}
		description = "Cancel an agent and its active descendants; it may be resumed later."
	case ListAgentsToolName:
		props["status"] = map[string]any{
			"type":        "string",
			"description": "Optional status filter; omit or use \"all\" to list every agent.",
			"enum":        []string{"all", "queued", "running", "awaiting_approval", "completed", "turn_limit", "cancelled", "interrupted", "failed", "running_elsewhere"},
		}
		description = "List agents belonging to this parent session, including interrupted agents from earlier processes."
	}
	return llm.ToolSpec{Name: t.name, Description: description, Schema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
}

// Preview names the agents a call targets; the tool name is already shown.
func (t *agentControlTool) Preview(args json.RawMessage) string {
	var a agentControlArgs
	if json.Unmarshal(args, &a) != nil {
		return ""
	}
	switch t.name {
	case WaitAgentToolName:
		return strings.Join(a.AgentIDs, ", ")
	case ListAgentsToolName:
		return a.Status
	}
	return a.AgentID
}

type agentControlArgs struct {
	AgentID      string   `json:"agent_id"`
	AgentIDs     []string `json:"agent_ids"`
	Instructions string   `json:"instructions"`
	Force        bool     `json:"force"`
	Wait         *int     `json:"wait"`
	MaxWait      int      `json:"max_wait"`
	Status       string   `json:"status"`
}

func (t *agentControlTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a agentControlArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return agentControlError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	parent := agentParent(ctx)
	if parent == "" {
		if provider, ok := t.spawn.snapshotLocalSpawnPolicy().runner.(interface{ ParentAgentSessionID() string }); ok {
			parent = provider.ParentAgentSessionID()
		}
	}
	if parent == "" {
		return agentControlError("agent lifecycle requires a parent session"), nil
	}
	var out llm.ToolOutput
	switch t.name {
	case ListAgentsToolName:
		out = t.list(ctx, parent, a)
	case WaitAgentToolName:
		out = t.wait(ctx, parent, a)
	case CancelAgentToolName:
		out = t.cancel(ctx, parent, a)
	case ContinueAgentToolName:
		if notice, _ := ctx.Value(agentRecoveryNoticeKey{}).(bool); notice {
			return agentControlError("A host lifecycle notice cannot resume child work; obtain explicit user confirmation in a subsequent turn."), nil
		}
		out = t.continueRun(ctx, parent, a)
	default:
		out = agentControlError("unknown agent control operation")
	}
	return out, nil
}

func (t *agentControlTool) list(ctx context.Context, parent string, a agentControlArgs) llm.ToolOutput {
	records, err := t.spawn.manager.snapshot(ctx, parent)
	if err != nil {
		return agentControlError(err.Error())
	}
	filtered := make([]map[string]any, 0, len(records))
	for _, r := range records {
		if a.Status != "" && a.Status != "all" && r.Status != a.Status {
			continue
		}
		resumable := r.Status == "completed" || r.Status == "turn_limit" || r.Status == "cancelled" || r.Status == "interrupted"
		filtered = append(filtered, map[string]any{
			"agent_id": r.ID, "agent_name": r.AgentName, "prompt_summary": session.TruncateSummary(r.Prompt),
			"status": r.Status, "stop_reason": r.StopReason, "resumable": resumable, "turns_used": r.TurnsUsed,
			"turns_granted": r.TurnsGranted, "last_activity": r.UpdatedAt, "collected": !r.CollectedAt.IsZero(), "current_tool": r.CurrentTool,
		})
	}
	data, _ := json.Marshal(filtered)
	return llm.TextOutput(string(data))
}

func (t *agentControlTool) wait(ctx context.Context, parent string, a agentControlArgs) llm.ToolOutput {
	if len(a.AgentIDs) == 0 {
		return agentControlError("agent_ids is required")
	}
	if a.MaxWait < 0 || a.MaxWait > 3600 {
		return agentControlError("max_wait must be between 0 and 3600")
	}
	m := t.spawn.manager
	results := make([]json.RawMessage, 0, len(a.AgentIDs))
	// Media produced by detached children is only delivered when collected;
	// releaseCollected drops it afterwards, so carry it on this tool result.
	var media []llm.MediaArtifact
	// Resolve every ID before waiting or collecting anything: an unknown ID
	// must not discard results (and media) already collected for earlier IDs,
	// which cannot be delivered again once their entries are released.
	for _, id := range a.AgentIDs {
		if _, _, err := m.get(ctx, id, parent); err != nil {
			return agentControlError(fmt.Sprintf("%s: %v", id, err))
		}
	}
	deadline := time.Now().Add(time.Duration(a.MaxWait) * time.Second)
	for _, id := range a.AgentIDs {
		record, e, err := m.get(ctx, id, parent)
		if err != nil {
			// Results already collected above cannot be delivered again;
			// report this entry's failure alongside them instead of dropping
			// the whole response.
			failure, _ := json.Marshal(map[string]string{"agent_id": id, "error": err.Error()})
			results = append(results, failure)
			continue
		}
		if e != nil && a.MaxWait > 0 {
			attached := m.attach(e, SubagentEventCallbackFromContext(ctx), llm.CallIDFromContext(ctx))
			if remaining := time.Until(deadline); remaining > 0 {
				m.wait(ctx, e, remaining)
			}
			m.detach(e, attached)
			record, _, _ = m.get(ctx, id, parent)
		}
		out := m.deliver(ctx, record, e)
		results = append(results, json.RawMessage(out.Content))
		media = append(media, out.Media...)
	}
	data, _ := json.Marshal(results)
	out := llm.TextOutput(string(data))
	out.Media = llm.NormalizeMedia(media, nil)
	return out
}

func agentTerminal(status string) bool {
	switch status {
	case "completed", "turn_limit", "cancelled", "interrupted", "failed":
		return true
	}
	return false
}

func (t *agentControlTool) cancel(ctx context.Context, parent string, a agentControlArgs) llm.ToolOutput {
	m := t.spawn.manager
	record, e, err := m.get(ctx, a.AgentID, parent)
	if err != nil {
		return agentControlError(err.Error())
	}
	if e != nil {
		e.cancel()
		m.wait(ctx, e, 5*time.Second)
		record, _, _ = m.get(ctx, a.AgentID, parent)
	}
	return agentOutput(record)
}

func (t *agentControlTool) continueRun(ctx context.Context, parent string, a agentControlArgs) llm.ToolOutput {
	m := t.spawn.manager
	record, e, err := m.get(ctx, a.AgentID, parent)
	if err != nil {
		return agentControlError(err.Error())
	}
	if e != nil {
		switch record.Status {
		case "queued":
			out := agentOutput(record)
			var result SpawnAgentResult
			_ = json.Unmarshal([]byte(out.Content), &result)
			result.Next = "agent not started yet; retry continue_agent once running or wait_agent for completion"
			out.Content = marshalAgentResult(result)
			return out
		case "running", "awaiting_approval":
			return t.steer(record, e, a)
		}
	}
	if record.Status == "running_elsewhere" && !a.Force {
		return agentControlError("agent may still be running on another process; pass force:true to risk duplicate side effects")
	}
	if record.Status == "failed" {
		return agentControlError("failed agent cannot be resumed")
	}
	budget := t.spawn.config.DefaultTimeout
	if a.Wait != nil {
		budget = *a.Wait
	}
	if budget < 0 || budget > 3600 {
		return agentControlError("wait must be between 0 and 3600")
	}
	// Resumed and never-started runs take their depth allowance from the
	// resuming parent, not the manager that originally created the record.
	if _, _, err := t.spawn.localSpawnPolicy(record.AgentName); err != nil {
		return agentControlError(err.Error())
	}
	owner := m
	owner.mu.Lock()
	runner, depth, draining := owner.runner, owner.depth, owner.draining
	owner.mu.Unlock()
	if draining {
		owner = m
		owner.mu.Lock()
		runner, depth = owner.runner, owner.depth
		owner.mu.Unlock()
	}
	if runner == nil {
		return agentControlError("agent runner unavailable")
	}
	resume := record.Started
	prompt := record.Prompt
	if !resume && strings.TrimSpace(a.Instructions) != "" {
		prompt += "\n\nAdditional instructions: " + a.Instructions
	}
	entry, startErr := owner.start(ctx, record.AgentName, prompt, record.Model, llm.CallIDFromContext(ctx), SubagentEventCallbackFromContext(ctx), t.spawn.GetEventCallback(), runner, depth+1, resume, a.Instructions, record)
	if startErr != nil {
		return agentControlError(startErr.Error())
	}
	owner.wait(ctx, entry, time.Duration(budget)*time.Second)
	owner.detachInitial(entry)
	current, _, _ := owner.get(ctx, a.AgentID, parent)
	out := m.deliver(ctx, current, entry)
	if record.Status == "running_elsewhere" {
		out.Content = strings.TrimSuffix(out.Content, "}") + `,"warning":"possible duplicate side effects: another process may still be running"}`
	}
	return out
}

func (t *agentControlTool) steer(record session.AgentRun, e *agentEntry, a agentControlArgs) llm.ToolOutput {
	e.manager.mu.Lock()
	runner := e.manager.runner
	e.manager.mu.Unlock()
	continuation, ok := runner.(AgentContinuation)
	if !ok {
		return agentControlError("runner does not support steering")
	}
	if strings.TrimSpace(a.Instructions) == "" {
		return agentControlError("instructions required to steer a running agent")
	}
	id, disposition := continuation.SteerAgent(a.AgentID, a.Instructions)
	result := map[string]any{
		"agent_id": a.AgentID, "status": record.Status, "steering_id": id,
		"intervention_disposition": disposition, "resumable": false,
		"next": fmt.Sprintf("wait_agent({\"agent_ids\":[%q]})", a.AgentID),
	}
	if disposition == "undelivered" {
		result["next"] = fmt.Sprintf("instruction was NOT delivered; call continue_agent({\"agent_id\":%q,\"instructions\":%q}) again after the agent stops", a.AgentID, a.Instructions)
	}
	data, _ := json.Marshal(result)
	return llm.TextOutput(string(data))
}

func agentControlError(message string) llm.ToolOutput {
	out := llm.TextOutput(message)
	out.IsError = true
	return out
}
