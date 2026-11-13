package tooldiscovery

import (
	"fmt"

	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/runtimeoutput"
)

// AttachEngine moves planner delivery/registry wiring to a replacement engine
// while preserving durable session activation state.
func (p *Planner) AttachEngine(engine *engine.Engine) {
	if p == nil || engine == nil {
		return
	}
	p.registrationMu.Lock()
	defer p.registrationMu.Unlock()
	p.engineMu.Lock()
	oldEngine := p.engine
	if oldEngine == engine {
		p.engineMu.Unlock()
		return
	}
	p.engine = engine
	p.engineMu.Unlock()

	p.mu.Lock()
	oldRegistered := p.registered
	p.registered = make(map[string]*mcp.MCPTool)
	p.mu.Unlock()
	if oldEngine != nil && oldEngine.ClearToolSurfacePlanner(p) {
		for name, owned := range oldRegistered {
			unregisterDiscoveryTool(oldEngine, name, owned)
		}
		if owned, ok := oldEngine.Tools().Get(ToolSearchName); ok {
			if search, ok := owned.(*SearchTool); ok && search.planner == p {
				unregisterDiscoveryTool(oldEngine, ToolSearchName, owned)
			}
		}
	}
	p.syncCatalogueLocked(p.manager.CatalogueSnapshot())
	engine.SetToolSurfacePlanner(p)
}

func (p *Planner) syncCatalogue(snapshot *mcp.CatalogueSnapshot) {
	p.registrationMu.Lock()
	defer p.registrationMu.Unlock()
	p.syncCatalogueLocked(snapshot)
}

func (p *Planner) syncCatalogueLocked(snapshot *mcp.CatalogueSnapshot) {
	if snapshot == nil {
		return
	}
	engine := p.currentEngine()
	if engine == nil {
		return
	}
	current := make(map[string]mcp.CatalogTool, len(snapshot.Tools))
	registered := make(map[string]*mcp.MCPTool, len(snapshot.Tools))
	installed := make([]mcp.CatalogTool, 0, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		wrapper := mcp.NewCatalogMCPTool(p.manager, tool)
		if err := engine.Tools().RegisterDeferred(wrapper); err != nil {
			runtimeoutput.Warn("MCP discovery tool registration rejected", "tool", tool.Name, "error", err)
			continue
		}
		if !p.catalogueToolInstalled(engine, tool) {
			continue
		}
		current[tool.Name] = tool
		registered[tool.Name] = wrapper
		installed = append(installed, tool)
	}

	p.mu.Lock()
	for name, owned := range p.registered {
		if _, ok := current[name]; !ok {
			unregisterDiscoveryTool(engine, name, owned)
		}
	}
	for _, state := range p.sessions {
		for name := range state.evicted {
			if _, exists := current[name]; !exists {
				delete(state.evicted, name)
			}
		}
		for name, active := range state.active {
			tool, exists := current[name]
			if !exists {
				delete(state.active, name)
				if _, sent := state.sent[name]; sent {
					state.resetRequired = "active MCP tool was removed from the catalogue"
				}
				continue
			}
			if active.SchemaHash != tool.SchemaHash {
				active.SchemaHash = tool.SchemaHash
				state.active[name] = active
				if _, sent := state.sent[name]; sent {
					state.resetRequired = "an active MCP tool schema changed"
				}
			}
		}
	}
	p.registered = registered
	p.mu.Unlock()
	p.index.replace(installed, snapshot.Generation)
}

func (p *Planner) authorizedCatalogueTools(engine *engine.Engine, tools []mcp.CatalogTool) map[string]mcp.CatalogTool {
	authorized := make(map[string]mcp.CatalogTool, len(tools))
	for _, tool := range tools {
		if p.catalogueToolAvailable(engine, tool) {
			authorized[tool.Name] = tool
		}
	}
	return authorized
}

func (p *Planner) applyToolSurfaceSelection(req *core.Request, engine *engine.Engine, snapshot *mcp.CatalogueSnapshot, initialToolNames map[string]bool, base []core.ToolSpec, resolved Mode, strategy Strategy, selection toolSurfaceSelection) error {
	req.Tools = append(base, selection.activeSpecs...)
	req.NativeToolDiscovery = nil
	if resolved == ModeDeferred && selection.deferredCount > 0 && strategy == StrategyPortable {
		searchTool := &SearchTool{planner: p}
		if err := engine.Tools().RegisterDeferred(searchTool); err != nil {
			return fmt.Errorf("register portable tool_search: %w", err)
		}
		if !p.searchToolInstalled(engine) {
			return fmt.Errorf("portable tool_search lost its planner-owned execution binding")
		}
		req.Tools = append(req.Tools, searchTool.Spec())
	} else if resolved == ModeDeferred && strategy == StrategyNative {
		req.NativeToolDiscovery = &core.NativeToolDiscoveryRequest{Search: (&SearchTool{planner: p}).Spec()}
	}
	if resolved == ModeDeferred && selection.deferredCount > 0 {
		// The vocabulary notice is a discovery surface too: never suggest a
		// rejected or denied remote name just because its server has other tools.
		advertised := *snapshot
		advertised.Tools = nil
		for _, tool := range snapshot.Tools {
			if p.catalogueToolAvailable(engine, tool) {
				advertised.Tools = append(advertised.Tools, tool)
			}
		}
		ensureDeferredToolSearchInstruction(req, &advertised, selection.serverDetails)
	}
	toolsAdded := false
	for _, spec := range req.Tools {
		if !initialToolNames[spec.Name] {
			toolsAdded = true
			break
		}
	}
	if len(req.Tools) == 0 && req.NativeToolDiscovery == nil {
		req.ToolChoice = core.ToolChoice{}
	} else if req.ToolChoice.Mode == "" && (toolsAdded || req.NativeToolDiscovery != nil) {
		req.ToolChoice = core.ToolChoice{Mode: core.ToolChoiceAuto}
	}
	return nil
}

// catalogueToolInstalled rejects name-only matches, another manager/server, and
// stale schemas. Only the real MCP adapter may satisfy this ownership check;
// annotations or an unrelated tool's self-declared provenance cannot grant it.
func (p *Planner) catalogueToolInstalled(eng *engine.Engine, tool mcp.CatalogTool) bool {
	if eng == nil {
		return false
	}
	registered, ok := eng.Tools().Get(tool.Name)
	if !ok {
		return false
	}
	installed, ok := registered.(*mcp.MCPTool)
	if !ok || installed == nil {
		return false
	}
	intended := mcp.NewCatalogMCPTool(p.manager, tool)
	return installed.ToolOrigin() == intended.ToolOrigin() && installed.ToolBindingID() != "" && installed.ToolBindingID() == intended.ToolBindingID()
}

func (p *Planner) catalogueToolAvailable(eng *engine.Engine, tool mcp.CatalogTool) bool {
	return eng != nil && eng.IsToolAllowed(tool.Name) && p.catalogueToolInstalled(eng, tool)
}

func (p *Planner) searchToolInstalled(eng *engine.Engine) bool {
	if eng == nil {
		return false
	}
	tool, ok := eng.Tools().Get(ToolSearchName)
	owned, isSearch := tool.(*SearchTool)
	return ok && isSearch && owned != nil && owned.planner == p
}

func unregisterDiscoveryTool(eng *engine.Engine, name string, owned core.Tool) {
	// Compare and delete atomically: a concurrent replacement owns the name.
	eng.Tools().UnregisterIf(name, owned)
}
