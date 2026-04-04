package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm"
)

// InitiateHandoverResult is the result returned by the tool.
type InitiateHandoverResult struct {
	Status string `json:"status"` // "confirmed", "cancelled", "error"
	Error  string `json:"error,omitempty"`
}

// InitiateHandoverArgs are the arguments passed to the initiate_handover tool.
type InitiateHandoverArgs struct {
	Agent string `json:"agent"`
}

var (
	handoverMu     sync.Mutex
	HandoverUIFunc InitiateHandoverFunc
)

// SetHandoverUIFunc sets the function to call for initiate_handover prompts.
func SetHandoverUIFunc(fn InitiateHandoverFunc) {
	handoverMu.Lock()
	defer handoverMu.Unlock()
	HandoverUIFunc = fn
}

// ClearHandoverUIFunc removes the custom handover UI function.
func ClearHandoverUIFunc() {
	handoverMu.Lock()
	defer handoverMu.Unlock()
	HandoverUIFunc = nil
}

// InitiateHandoverTool implements the initiate_handover tool.
type InitiateHandoverTool struct{}

// NewInitiateHandoverTool creates a new initiate_handover tool.
func NewInitiateHandoverTool() *InitiateHandoverTool {
	return &InitiateHandoverTool{}
}

// Spec returns the tool specification.
func (t *InitiateHandoverTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name: InitiateHandoverToolName,
		Description: `Initiate a handover to another agent. This triggers the handover confirmation UI where the user can confirm, add instructions, or cancel. Use this when you want to hand off work to a different agent (e.g., from planner to developer).

Guidelines:
- Use the agent name without the @ prefix
- The user will see a confirmation dialog and can add instructions or cancel
- If confirmed, the session restarts with the target agent
- If cancelled, you will receive a cancellation result and should continue normally`,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"agent": map[string]interface{}{
					"type":        "string",
					"description": "The name of the target agent to hand over to (without @ prefix)",
				},
			},
			"required":             []string{"agent"},
			"additionalProperties": false,
		},
	}
}

// Execute runs the initiate_handover tool.
func (t *InitiateHandoverTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	var a InitiateHandoverArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return llm.TextOutput(formatHandoverResult("error", fmt.Sprintf("failed to parse arguments: %v", err))), nil
	}

	agent := strings.TrimSpace(strings.TrimPrefix(a.Agent, "@"))
	if agent == "" {
		return llm.TextOutput(formatHandoverResult("error", "agent name is required")), nil
	}

	// Check context-based handler first (for web serve / non-TUI use)
	ctxHandler := handoverFuncFromContext(ctx)

	// Fall back to global handler (for TUI)
	handoverMu.Lock()
	uiFunc := HandoverUIFunc
	handoverMu.Unlock()

	var fn InitiateHandoverFunc
	switch {
	case ctxHandler != nil:
		fn = ctxHandler
	case uiFunc != nil:
		fn = uiFunc
	default:
		return llm.TextOutput(formatHandoverResult("error", "handover UI not available")), nil
	}

	confirmed, err := fn(ctx, agent)
	if err != nil {
		return llm.TextOutput(formatHandoverResult("error", err.Error())), nil
	}
	if confirmed {
		return llm.TextOutput(formatHandoverResult("confirmed", "")), nil
	}
	return llm.TextOutput(formatHandoverResult("cancelled", "user declined the handover")), nil
}

// Preview returns a short description of the tool call.
func (t *InitiateHandoverTool) Preview(args json.RawMessage) string {
	var a InitiateHandoverArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return ""
	}
	return "@" + strings.TrimSpace(strings.TrimPrefix(a.Agent, "@"))
}

func formatHandoverResult(status, errMsg string) string {
	result := InitiateHandoverResult{Status: status, Error: errMsg}
	data, _ := json.Marshal(result)
	return string(data)
}
