package tools

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm"
)

// SetOutputTool captures structured output from an agent.
// This tool is dynamically created based on agent configuration and provides
// a way to force structured output via a tool call, eliminating verbose prose
// that LLMs often include even with explicit instructions.
type SetOutputTool struct {
	name        string // Configured tool name (e.g., "set_commit_message")
	paramName   string // Parameter name to capture (e.g., "message")
	description string // Tool description
	value       string // Captured value
}

// NewSetOutputTool creates a tool with custom name/description.
func NewSetOutputTool(name, paramName, description string) *SetOutputTool {
	return &SetOutputTool{
		name:        name,
		paramName:   paramName,
		description: description,
	}
}

func (t *SetOutputTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        t.name,
		Description: t.description,
		Schema: map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				t.paramName: map[string]interface{}{
					"type":        "string",
					"description": "The output content",
				},
			},
			"required":             []string{t.paramName},
			"additionalProperties": false,
		},
	}
}

func (t *SetOutputTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	var params map[string]interface{}
	if err := json.Unmarshal(args, &params); err != nil {
		return "", err
	}
	if v, ok := params[t.paramName].(string); ok {
		t.value = v
	}
	return "Output captured.", nil
}

func (t *SetOutputTool) Preview(args json.RawMessage) string {
	return "" // No preview needed
}

// Value returns the captured output value.
func (t *SetOutputTool) Value() string {
	return t.value
}

// Name returns the configured tool name.
func (t *SetOutputTool) Name() string {
	return t.name
}

// IsFinishingTool returns true because output tools signal agent completion.
func (t *SetOutputTool) IsFinishingTool() bool {
	return true
}
