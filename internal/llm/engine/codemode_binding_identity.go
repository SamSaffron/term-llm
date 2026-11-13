package engine

import "github.com/samsaffron/term-llm/internal/llm/core"

// codeModeExecutionSnapshot captures instance and generation together. Reading
// them separately could pair an old tool's schema with a replacement's binding.
func (r *ToolRegistry) codeModeExecutionSnapshot() (map[string]core.Tool, map[string]uint64) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tools := make(map[string]core.Tool, len(r.tools))
	generations := make(map[string]uint64, len(r.tools))
	for name, tool := range r.tools {
		tools[name] = tool
		generations[name] = r.registrations[name].generation
	}
	return tools, generations
}

// codeModeExecutionBinding reads the current tool and generation atomically.
func (r *ToolRegistry) codeModeExecutionBinding(name string) (core.Tool, uint64, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, r.registrations[name].generation, ok
}
