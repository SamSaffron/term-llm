package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm"
)

// SetOutputTool captures structured output from an agent.
// This tool is dynamically created based on agent configuration and provides
// a way to force structured output via a tool call, eliminating verbose prose
// that LLMs often include even with explicit instructions.
type SetOutputTool struct {
	name        string // Configured tool name (e.g., "set_commit_message")
	paramName   string // Legacy string parameter name; empty for a typed object
	description string // Tool description
	schema      map[string]interface{}
	value       string // Captured value
	captured    bool   // Whether output was captured
}

// NewSetOutputTool creates an output tool. A nil schema uses the legacy
// single-string parameter; a non-nil schema captures the complete argument
// object as JSON.
func NewSetOutputTool(name, paramName, description string, schema map[string]interface{}) *SetOutputTool {
	if schema == nil {
		if paramName == "" {
			paramName = "content"
		}
		schema = map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				paramName: map[string]interface{}{
					"type":        "string",
					"description": "The output content",
				},
			},
			"required":             []string{paramName},
			"additionalProperties": false,
		}
	} else {
		paramName = ""
	}

	return &SetOutputTool{
		name:        name,
		paramName:   paramName,
		description: description,
		schema:      schema,
	}
}

func (t *SetOutputTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        t.name,
		Description: t.description,
		Schema:      t.schema,
	}
}

func (t *SetOutputTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	if t.paramName == "" {
		var object map[string]interface{}
		if err := json.Unmarshal(args, &object); err != nil {
			return llm.ToolOutput{}, fmt.Errorf("decode structured output: %w", err)
		}
		if object == nil {
			return llm.ToolOutput{}, fmt.Errorf("structured output must be a JSON object")
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, args); err != nil {
			return llm.ToolOutput{}, fmt.Errorf("compact structured output: %w", err)
		}
		t.value = compact.String()
		t.captured = true
		return llm.TextOutput("Output captured."), nil
	}

	var params map[string]interface{}
	if err := json.Unmarshal(args, &params); err != nil {
		return llm.ToolOutput{}, err
	}
	if v, ok := params[t.paramName].(string); ok {
		t.value = v
		t.captured = true
	}
	return llm.TextOutput("Output captured."), nil
}

func (t *SetOutputTool) Preview(args json.RawMessage) string {
	return "" // No preview needed
}

// Value returns the captured output value.
func (t *SetOutputTool) Value() string {
	return t.value
}

// Captured returns true once output was captured.
func (t *SetOutputTool) Captured() bool {
	return t.captured
}

// Name returns the configured tool name.
func (t *SetOutputTool) Name() string {
	return t.name
}

// IsFinishingTool returns true because output tools signal agent completion.
func (t *SetOutputTool) IsFinishingTool() bool {
	return true
}
