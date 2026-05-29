package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/samsaffron/term-llm/internal/llm"
)

// JobHandoffFunc starts a durable background job for work that should leave the
// foreground chat response.
type JobHandoffFunc func(ctx context.Context, req JobHandoffRequest) (JobHandoffResult, error)

type jobHandoffContextKey struct{}

// ContextWithJobHandoffFunc stores a request-scoped job handoff handler in ctx.
func ContextWithJobHandoffFunc(ctx context.Context, fn JobHandoffFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, jobHandoffContextKey{}, fn)
}

func jobHandoffFuncFromContext(ctx context.Context) JobHandoffFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(jobHandoffContextKey{}).(JobHandoffFunc)
	return fn
}

// JobHandoffRequest is the tool payload for handoff_to_job.
type JobHandoffRequest struct {
	Instructions   string `json:"instructions"`
	AgentName      string `json:"agent_name,omitempty"`
	Name           string `json:"name,omitempty"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
}

// JobHandoffResult is returned by handoff_to_job.
type JobHandoffResult struct {
	Status    string `json:"status"`
	JobID     string `json:"job_id,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// JobHandoffTool implements the handoff_to_job tool.
type JobHandoffTool struct{}

func NewJobHandoffTool() *JobHandoffTool { return &JobHandoffTool{} }

func (t *JobHandoffTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: JobHandoffToolName,
		Description: `Hand long-running work off from the current foreground chat response to a durable background LLM job.

Use this when the task is likely to run for a long time, use many tool loops, or no longer needs to block the live chat response. The background job can continue independently and report its final result back to the originating chat session.

Provide concise but complete instructions: include the current goal, important findings so far, files/URLs/session context needed to continue, and the expected final deliverable.`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"instructions": map[string]interface{}{
					"type":        "string",
					"description": "Complete instructions for the background job, including current context and expected final deliverable.",
				},
				"agent_name": map[string]interface{}{
					"type":        "string",
					"description": "Optional agent to run as. Defaults to the current agent.",
				},
				"name": map[string]interface{}{
					"type":        "string",
					"description": "Optional short job name shown in jobs UI/CLI.",
				},
				"timeout_seconds": map[string]interface{}{
					"type":        "integer",
					"description": "Optional run timeout in seconds. Defaults to 7200 and is capped by server policy.",
				},
			},
			"required":             []string{"instructions"},
			"additionalProperties": false,
		},
	}
}

func (t *JobHandoffTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var req JobHandoffRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return llm.TextOutput(formatJobHandoffResult(JobHandoffResult{Status: "error", Error: fmt.Sprintf("failed to parse arguments: %v", err)})), nil
	}
	req.Instructions = strings.TrimSpace(req.Instructions)
	req.AgentName = strings.TrimSpace(req.AgentName)
	req.Name = strings.TrimSpace(req.Name)
	if req.Instructions == "" {
		return llm.TextOutput(formatJobHandoffResult(JobHandoffResult{Status: "error", Error: "instructions is required"})), nil
	}
	fn := jobHandoffFuncFromContext(ctx)
	if fn == nil {
		return llm.TextOutput(formatJobHandoffResult(JobHandoffResult{Status: "error", Error: "job handoff is not available"})), nil
	}
	res, err := fn(ctx, req)
	if err != nil {
		return llm.TextOutput(formatJobHandoffResult(JobHandoffResult{Status: "error", Error: err.Error()})), nil
	}
	if strings.TrimSpace(res.Status) == "" {
		res.Status = "started"
	}
	return llm.TextOutput(formatJobHandoffResult(res)), nil
}

func (t *JobHandoffTool) Preview(args json.RawMessage) string {
	var req JobHandoffRequest
	if err := json.Unmarshal(args, &req); err != nil {
		return ""
	}
	if name := strings.TrimSpace(req.Name); name != "" {
		return name
	}
	return "background job"
}

func formatJobHandoffResult(res JobHandoffResult) string {
	data, _ := json.Marshal(res)
	return string(data)
}
