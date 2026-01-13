package testutil

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm"
)

// MockTool is a configurable tool for testing.
type MockTool struct {
	SpecData    llm.ToolSpec
	ExecuteFn   func(ctx context.Context, args json.RawMessage) (string, error)
	PreviewFn   func(args json.RawMessage) string
	Invocations []MockToolInvocation
}

// MockToolInvocation records a single tool invocation.
type MockToolInvocation struct {
	Args   json.RawMessage
	Result string
	Error  error
}

// Spec implements llm.Tool.
func (m *MockTool) Spec() llm.ToolSpec {
	return m.SpecData
}

// Execute implements llm.Tool.
func (m *MockTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	if m.ExecuteFn == nil {
		return "", nil
	}
	result, err := m.ExecuteFn(ctx, args)
	m.Invocations = append(m.Invocations, MockToolInvocation{
		Args:   args,
		Result: result,
		Error:  err,
	})
	return result, err
}

// Preview implements llm.Tool.
func (m *MockTool) Preview(args json.RawMessage) string {
	if m.PreviewFn == nil {
		return ""
	}
	return m.PreviewFn(args)
}

// NewMockTool creates a mock tool with the given name that returns a fixed result.
func NewMockTool(name string, result string) *MockTool {
	return &MockTool{
		SpecData: llm.ToolSpec{
			Name:        name,
			Description: "Mock tool: " + name,
			Schema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		ExecuteFn: func(ctx context.Context, args json.RawMessage) (string, error) {
			return result, nil
		},
	}
}

// NewMockToolWithSchema creates a mock tool with a custom schema.
func NewMockToolWithSchema(name, description string, schema map[string]interface{}, executeFn func(ctx context.Context, args json.RawMessage) (string, error)) *MockTool {
	return &MockTool{
		SpecData: llm.ToolSpec{
			Name:        name,
			Description: description,
			Schema:      schema,
		},
		ExecuteFn: executeFn,
	}
}

// InvocationCount returns the number of times the tool was invoked.
func (m *MockTool) InvocationCount() int {
	return len(m.Invocations)
}

// LastArgs returns the arguments from the last invocation, or nil if never invoked.
func (m *MockTool) LastArgs() json.RawMessage {
	if len(m.Invocations) == 0 {
		return nil
	}
	return m.Invocations[len(m.Invocations)-1].Args
}
