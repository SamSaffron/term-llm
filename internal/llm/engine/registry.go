package engine

import (
	"sort"
	"sync"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

// ToolRegistry stores tools by name for execution.
type ToolRegistry struct {
	mu         sync.RWMutex
	tools      map[string]core.Tool
	visible    map[string]bool
	specsCache []core.ToolSpec
	specsDirty bool
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{tools: make(map[string]core.Tool), visible: make(map[string]bool), specsDirty: true}
}

// Register makes a tool executable and provider-visible.
func (r *ToolRegistry) Register(tool core.Tool) {
	r.register(tool, true)
}

// RegisterDeferred makes a tool executable without exposing its schema through AllSpecs.
func (r *ToolRegistry) RegisterDeferred(tool core.Tool) {
	r.register(tool, false)
}

func (r *ToolRegistry) register(tool core.Tool, visible bool) {
	spec := tool.Spec()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[spec.Name] = tool
	r.visible[spec.Name] = visible
	r.specsDirty = true
}

// SetVisibility changes provider visibility without affecting execution lookup.
func (r *ToolRegistry) SetVisibility(name string, visible bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return false
	}
	if r.visible[name] != visible {
		r.visible[name] = visible
		r.specsDirty = true
	}
	return true
}

func (r *ToolRegistry) Get(name string) (core.Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

// IsFinishingTool returns true if the named tool is a finishing tool.
func (r *ToolRegistry) IsFinishingTool(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	if !ok {
		return false
	}
	if ft, ok := tool.(core.FinishingTool); ok {
		return ft.IsFinishingTool()
	}
	return false
}

func (r *ToolRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; ok {
		delete(r.tools, name)
		delete(r.visible, name)
		r.specsDirty = true
	}
}

// AllSpecs returns the specs for all registered tools in deterministic name order.
func (r *ToolRegistry) AllSpecs() []core.ToolSpec {
	r.mu.RLock()
	if !r.specsDirty {
		specs := copyToolSpecs(r.specsCache)
		r.mu.RUnlock()
		return specs
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.specsDirty {
		names := make([]string, 0, len(r.tools))
		for name := range r.tools {
			if r.visible[name] {
				names = append(names, name)
			}
		}
		sort.Strings(names)

		specs := make([]core.ToolSpec, 0, len(names))
		for _, name := range names {
			specs = append(specs, r.tools[name].Spec())
		}
		r.specsCache = specs
		r.specsDirty = false
	}
	return copyToolSpecs(r.specsCache)
}

// AllSpecsIncludingDeferred returns every executable tool spec for diagnostics
// and discovery planning, regardless of provider visibility.
func (r *ToolRegistry) AllSpecsIncludingDeferred() []core.ToolSpec {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	specs := make([]core.ToolSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, r.tools[name].Spec())
	}
	return specs
}

// IsVisible reports whether a registered tool is included by AllSpecs.
func (r *ToolRegistry) IsVisible(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.visible[name]
}

// copyToolSpecs protects callers from mutating the cached slice or top-level
// core.ToolSpec values. core.ToolSpec.Schema maps are intentionally shared by the
// immutability contract on core.ToolSpec.
func copyToolSpecs(specs []core.ToolSpec) []core.ToolSpec {
	out := make([]core.ToolSpec, len(specs))
	copy(out, specs)
	return out
}
