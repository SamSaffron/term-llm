package mcp

import (
	"context"
	"encoding/json"

	"github.com/samsaffron/term-llm/internal/llm"
)

// MCPTool wraps an MCP server tool as an llm.Tool.
type MCPTool struct {
	manager  *Manager
	toolSpec llm.ToolSpec
}

// NewMCPTool creates a new MCP tool wrapper from the legacy projected shape.
func NewMCPTool(manager *Manager, spec ToolSpec) *MCPTool {
	return &MCPTool{
		manager: manager,
		toolSpec: llm.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		},
	}
}

// NewCatalogMCPTool creates a wrapper retaining structured output metadata.
func NewCatalogMCPTool(manager *Manager, tool CatalogTool) *MCPTool {
	return &MCPTool{manager: manager, toolSpec: tool.ToolSpec()}
}

// Spec returns the tool specification for the LLM.
func (t *MCPTool) Spec() llm.ToolSpec {
	return t.toolSpec
}

// Preview returns empty string for MCP tools - the engine falls back to extractToolInfo().
func (t *MCPTool) Preview(args json.RawMessage) string {
	return ""
}

// Execute invokes the tool on the MCP server.
func (t *MCPTool) Execute(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
	return t.manager.CallTool(ctx, t.toolSpec.Name, args)
}

// RegisterMCPTools registers all MCP tools from the manager into the tool registry.
func RegisterMCPTools(manager *Manager, registry *llm.ToolRegistry) {
	for _, tool := range manager.CatalogueSnapshot().Tools {
		registry.Register(NewCatalogMCPTool(manager, tool))
	}
}

// RegisterMCPToolsDeferred registers all MCP wrappers for execution while keeping
// their schemas hidden until a discovery planner selects them.
func RegisterMCPToolsDeferred(manager *Manager, registry *llm.ToolRegistry) {
	if manager == nil || registry == nil {
		return
	}
	snapshot := manager.CatalogueSnapshot()
	if snapshot == nil {
		return
	}
	for _, tool := range snapshot.Tools {
		registry.RegisterDeferred(NewCatalogMCPTool(manager, tool))
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
