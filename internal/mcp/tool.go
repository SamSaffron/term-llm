package mcp

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm"
)

// MCPTool wraps an MCP server tool as an llm.Tool.
type MCPTool struct {
	manager  *Manager
	toolSpec ToolSpec
}

// NewMCPTool creates a new MCP tool wrapper.
func NewMCPTool(manager *Manager, spec ToolSpec) *MCPTool {
	return &MCPTool{
		manager:  manager,
		toolSpec: spec,
	}
}

// Spec returns the tool specification for the LLM.
func (t *MCPTool) Spec() llm.ToolSpec {
	return llm.ToolSpec{
		Name:        t.toolSpec.Name,
		Description: t.toolSpec.Description,
		Schema:      t.toolSpec.Schema,
	}
}

// Preview returns empty string for MCP tools - the engine falls back to extractToolInfo().
func (t *MCPTool) Preview(args json.RawMessage) string {
	return ""
}

// Execute invokes the tool on the MCP server.
func (t *MCPTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	return t.manager.CallTool(ctx, t.toolSpec.Name, args)
}

// RegisterMCPTools registers all MCP tools from the manager into the tool registry.
func RegisterMCPTools(manager *Manager, registry *llm.ToolRegistry) {
	tools := manager.AllTools()
	for _, spec := range tools {
		tool := NewMCPTool(manager, spec)
		registry.Register(tool)
	}
}

// GetMCPToolSpecs returns LLM tool specs for all running MCP tools.
func GetMCPToolSpecs(manager *Manager) []llm.ToolSpec {
	mcpTools := manager.AllTools()
	specs := make([]llm.ToolSpec, 0, len(mcpTools))
	for _, t := range mcpTools {
		specs = append(specs, llm.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		})
	}
	return specs
}
