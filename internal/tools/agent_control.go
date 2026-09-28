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
		props["status"] = map[string]any{"type": "string"}
		description = "List agents belonging to this parent session, including interrupted agents from earlier processes."
	}
	return llm.ToolSpec{Name: t.name, Description: description, Schema: map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
}
func (t *agentControlTool) Preview(args json.RawMessage) string { return t.name }
func (t *agentControlTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a struct {
		AgentID      string   `json:"agent_id"`
		AgentIDs     []string `json:"agent_ids"`
		Instructions string   `json:"instructions"`
		Force        bool     `json:"force"`
		Wait         *int     `json:"wait"`
		MaxWait      int      `json:"max_wait"`
		Status       string   `json:"status"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	m := t.spawn.manager
	parent := agentParent(ctx)
	if parent == "" {
		if provider, ok := m.runner.(interface{ ParentAgentSessionID() string }); ok {
			parent = provider.ParentAgentSessionID()
		}
	}
	if parent == "" {
		return llm.TextOutput("agent lifecycle requires a parent session"), nil
	}
	switch t.name {
	case ListAgentsToolName:
		records, err := m.snapshot(ctx, parent)
		if err != nil {
			return llm.TextOutput(err.Error()), nil
		}
		filtered := make([]session.AgentRun, 0, len(records))
		for _, r := range records {
			if a.Status == "" || r.Status == a.Status {
				filtered = append(filtered, r)
			}
		}
		data, _ := json.Marshal(filtered)
		return llm.TextOutput(string(data)), nil
	case WaitAgentToolName:
		if len(a.AgentIDs) == 0 {
			return llm.TextOutput("agent_ids is required"), nil
		}
		if a.MaxWait < 0 {
			return llm.TextOutput("max_wait must be nonnegative"), nil
		}
		results := make([]json.RawMessage, 0, len(a.AgentIDs))
		for _, id := range a.AgentIDs {
			record, e, err := m.get(ctx, id, parent)
			if err != nil {
				return llm.TextOutput(err.Error()), nil
			}
			if e != nil && a.MaxWait > 0 {
				m.attach(e, SubagentEventCallbackFromContext(ctx), llm.CallIDFromContext(ctx))
				m.wait(ctx, e, time.Duration(a.MaxWait)*time.Second)
				m.detach(e)
				record, _, _ = m.get(ctx, id, parent)
			}
			record.CollectedAt = time.Now()
			m.save(record)
			out := agentOutput(record)
			results = append(results, json.RawMessage(out.Content))
		}
		data, _ := json.Marshal(results)
		return llm.TextOutput(string(data)), nil
	case CancelAgentToolName:
		record, e, err := m.get(ctx, a.AgentID, parent)
		if err != nil {
			return llm.TextOutput(err.Error()), nil
		}
		if e != nil {
			e.cancel()
			m.wait(ctx, e, 5*time.Second)
			record, _, _ = m.get(ctx, a.AgentID, parent)
		}
		return agentOutput(record), nil
	case ContinueAgentToolName:
		record, e, err := m.get(ctx, a.AgentID, parent)
		if err != nil {
			return llm.TextOutput(err.Error()), nil
		}
		if e != nil && (record.Status == "running" || record.Status == "queued") {
			continuation, ok := m.runner.(AgentContinuation)
			if !ok {
				return llm.TextOutput("runner does not support steering"), nil
			}
			id, disposition := continuation.SteerAgent(a.AgentID, a.Instructions)
			if strings.TrimSpace(a.Instructions) == "" {
				return llm.TextOutput("instructions required to steer a running agent"), nil
			}
			result := map[string]string{"agent_id": a.AgentID, "status": record.Status, "steering_id": id, "intervention_disposition": disposition}
			if disposition == "undelivered" {
				result["next"] = "instruction was NOT delivered; call continue_agent again with it after the agent stops"
			}
			data, _ := json.Marshal(result)
			return llm.TextOutput(string(data)), nil
		}
		if record.Status == "running_elsewhere" && !a.Force {
			return llm.TextOutput("agent may still be running on another process; pass force:true to risk duplicate side effects"), nil
		}
		if record.Status == "failed" {
			return llm.TextOutput("failed agent cannot be resumed"), nil
		}
		m.mu.Lock()
		runner, depth := t.spawn.runner, t.spawn.depth
		m.mu.Unlock()
		if runner == nil {
			return llm.TextOutput("agent runner unavailable"), nil
		}
		entry := m.start(ctx, record.AgentName, record.Prompt, "", llm.CallIDFromContext(ctx), SubagentEventCallbackFromContext(ctx), runner, depth+1, true, a.Instructions, record)
		budget := t.spawn.config.DefaultTimeout
		if a.Wait != nil {
			budget = *a.Wait
		}
		if budget < 0 {
			return llm.TextOutput("wait must be nonnegative"), nil
		}
		m.wait(ctx, entry, time.Duration(budget)*time.Second)
		m.detach(entry)
		current, _, _ := m.get(ctx, a.AgentID, parent)
		out := agentOutput(current)
		if record.Status == "running_elsewhere" {
			out.Content = strings.TrimSuffix(out.Content, "}") + `,"warning":"possible duplicate side effects: another process may still be running"}`
		}
		return out, nil
	}
	return llm.TextOutput("unknown agent control operation"), nil
}
