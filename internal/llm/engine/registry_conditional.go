package engine

import "github.com/samsaffron/term-llm/internal/llm/core"

// UnregisterIf atomically removes the expected instance or an idempotent refresh
// of its explicit origin/binding. A different owner or binding is never deleted.
// Identity callbacks run before the lock, as they may consult host registries.
// Overrides restore their previous binding.
func (r *ToolRegistry) UnregisterIf(name string, expected core.Tool) bool {
	origin, bindingID := core.ToolOriginOf(expected), core.ToolBindingIDOf(expected)
	r.mu.Lock()
	defer r.mu.Unlock()
	current, ok := r.registrations[name]
	if !ok {
		return false
	}
	same := sameToolInstance(current.tool, expected) ||
		(bindingID != "" && bindingID == current.bindingID && origin.SameOrigin(current.origin))
	if !same {
		return false
	}
	r.unregisterLocked(name)
	return true
}
