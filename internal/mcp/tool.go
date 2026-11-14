package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/engine"
)

// ErrToolRemovedFromCatalogue is returned by CodeModeIdentity when the tool is
// no longer present in the current server catalogue. Code mode callers must treat
// this as a schema_changed condition and discard any cached spec or hash.
var ErrToolRemovedFromCatalogue = errors.New("MCP tool removed from server catalogue")

// MCPTool wraps an MCP server tool as an llm.Tool.
type MCPTool struct {
	manager      *Manager
	toolSpec     core.ToolSpec
	server       string
	originalName string
}

// NewMCPTool creates a new MCP tool wrapper from the legacy projected shape.
func NewMCPTool(manager *Manager, spec ToolSpec) *MCPTool {
	return &MCPTool{
		manager: manager,
		toolSpec: core.ToolSpec{
			Name:        spec.Name,
			Description: spec.Description,
			Schema:      spec.Schema,
		},
	}
}

// NewCatalogMCPTool creates a wrapper retaining structured output metadata.
func NewCatalogMCPTool(manager *Manager, tool CatalogTool) *MCPTool {
	return &MCPTool{
		manager:      manager,
		toolSpec:     tool.ToolSpec(),
		server:       tool.Server,
		originalName: tool.OriginalName,
	}
}

// Spec returns the tool specification for the LLM.
func (t *MCPTool) Spec() core.ToolSpec {
	return t.toolSpec
}

// Preview returns empty string for MCP tools - the engine falls back to extractToolInfo().
func (t *MCPTool) Preview(args json.RawMessage) string {
	return ""
}

// Execute invokes the tool on the MCP server.
func (t *MCPTool) Execute(ctx context.Context, args json.RawMessage) (core.ToolOutput, error) {
	if t.server != "" && t.originalName != "" {
		if err := t.manager.policyError(t.server); err != nil {
			return core.ToolOutput{}, err
		}
		return t.manager.CallCatalogTool(ctx, t.server, t.originalName, t.toolSpec.Name, args)
	}
	return t.manager.CallTool(ctx, t.toolSpec.Name, args)
}

// CodeModeIdentity returns the current exact spec and schema hash for this tool
// from the live server catalogue. It is an optional code-mode admission interface:
// no concrete interface is defined; the engine and child-call path use a type
// assertion to detect schema_changed without relying on the stale observed spec
// that was present at registration time.
//
// Returns a [PermissionError] when the server is currently denied by policy, and
// [ErrToolRemovedFromCatalogue] when the tool has been removed from the live
// catalogue. ctx is reserved for future async refresh; it is not used today.
func (t *MCPTool) CodeModeIdentity(_ context.Context) (core.ToolSpec, string, error) {
	if t.server != "" && t.originalName != "" {
		// Catalogue tool: direct per-server lookup by original name.
		if err := t.manager.policyError(t.server); err != nil {
			return core.ToolSpec{}, "", err
		}
		t.manager.mu.RLock()
		snapshot := t.manager.catalogues[t.server]
		t.manager.mu.RUnlock()
		if snapshot == nil {
			return core.ToolSpec{}, "", fmt.Errorf("MCP server %s catalogue not ready", t.server)
		}
		idx, ok := snapshot.ByOriginal[t.originalName]
		if !ok {
			return core.ToolSpec{}, "", ErrToolRemovedFromCatalogue
		}
		ct := snapshot.Tools[idx]
		return ct.ToolSpec(), ct.SchemaHash, nil
	}

	// Legacy tool: resolve by flattened projected name from the aggregate
	// catalogue without name-splitting. This avoids the parseToolName heuristic
	// and works for single-server configurations that do not namespace tools.
	agg := t.manager.CatalogueSnapshot()
	if agg == nil {
		return core.ToolSpec{}, "", fmt.Errorf("MCP catalogue not ready")
	}
	for _, ct := range agg.Tools {
		if ct.Name == t.toolSpec.Name {
			if err := t.manager.policyError(ct.Server); err != nil {
				return core.ToolSpec{}, "", err
			}
			return ct.ToolSpec(), ct.SchemaHash, nil
		}
	}
	return core.ToolSpec{}, "", ErrToolRemovedFromCatalogue
}

// RegisterMCPTools registers all MCP tools from the manager into the tool registry.
func RegisterMCPTools(manager *Manager, registry *engine.ToolRegistry) {
	for _, tool := range manager.CatalogueSnapshot().Tools {
		registry.Register(NewCatalogMCPTool(manager, tool))
	}
}

// RegisterMCPToolsDeferred registers all MCP wrappers for execution while keeping
// their schemas hidden until a discovery planner selects them.
func RegisterMCPToolsDeferred(manager *Manager, registry *engine.ToolRegistry) {
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
func GetMCPToolSpecs(manager *Manager) []core.ToolSpec {
	mcpTools := manager.AllTools()
	specs := make([]core.ToolSpec, 0, len(mcpTools))
	for _, t := range mcpTools {
		specs = append(specs, core.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Schema:      t.Schema,
		})
	}
	return specs
}

// CodeModeExposure is independent of remote names and untrusted annotations.
// Admission and dispatch still validate exact catalogue identity and live policy.
func (*MCPTool) CodeModeExposure() core.CodeModeExposure {
	return core.CodeModeExposure{Allowed: true}
}
