package engine

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
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

	registrations  map[string]toolRegistration
	shadows        map[string][]toolRegistration
	nextGeneration uint64
}

// toolRegistration is the immutable-enough execution binding for one name.
type toolRegistration struct {
	tool       core.Tool
	origin     core.ToolOrigin
	bindingID  string
	generation uint64
	visible    bool
	reason     string
}

// ToolBinding describes the current execution binding for an occupied name.
// Generation changes whenever the name resolves to a different binding; it is
// stable across visibility changes and idempotent same-binding refreshes.
type ToolBinding struct {
	Origin     core.ToolOrigin
	Generation uint64
}

// ToolNameConflictError reports an attempt to replace an occupied execution name
// with a tool from a different trusted origin. The existing tool remains active.
// Use RegisterOverride when a scoped replacement is intentional.
type ToolNameConflictError struct {
	Name           string
	ExistingOrigin core.ToolOrigin
	IncomingOrigin core.ToolOrigin
	ExistingType   string
	IncomingType   string
}

func (e *ToolNameConflictError) Error() string {
	if e == nil {
		return "tool name conflict"
	}
	return fmt.Sprintf("tool %q is already registered by %s (%s); refusing to replace it with %s (%s)",
		e.Name, e.ExistingOrigin, e.ExistingType, e.IncomingOrigin, e.IncomingType)
}

func (r *ToolRegistry) executionSnapshot() map[string]core.Tool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make(map[string]core.Tool, len(r.tools))
	for n, t := range r.tools {
		result[n] = t
	}
	return result
}

func NewToolRegistry() *ToolRegistry {
	return &ToolRegistry{
		tools:          make(map[string]core.Tool),
		visible:        make(map[string]bool),
		registrations:  make(map[string]toolRegistration),
		shadows:        make(map[string][]toolRegistration),
		nextGeneration: 1,
		specsDirty:     true,
	}
}

// Register makes a tool executable and provider-visible. It returns a
// [ToolNameConflictError] when name is owned by a different trusted origin; the
// occupied binding is unchanged. Same-origin refreshes remain valid.
func (r *ToolRegistry) Register(tool core.Tool) error {
	return reportToolRegistrationError(r.register(tool, true, false, ""))
}

// RegisterDeferred makes a tool executable without exposing its schema through AllSpecs.
func (r *ToolRegistry) RegisterDeferred(tool core.Tool) error {
	return reportToolRegistrationError(r.register(tool, false, false, ""))
}

// RegisterOverride intentionally shadows an occupied name and restores the
// previous binding when Unregister removes the override. reason is diagnostic
// only. Prefer this for scoped transitions such as skill-over-custom tools.
func (r *ToolRegistry) RegisterOverride(tool core.Tool, reason string) error {
	return reportToolRegistrationError(r.register(tool, true, true, reason))
}

// RegisterDeferredOverride is RegisterOverride with provider visibility disabled.
func (r *ToolRegistry) RegisterDeferredOverride(tool core.Tool, reason string) error {
	return reportToolRegistrationError(r.register(tool, false, true, reason))
}

func (r *ToolRegistry) register(tool core.Tool, visible, override bool, reason string) error {
	if tool == nil {
		return errors.New("tool registry: cannot register a nil tool")
	}
	spec := tool.Spec()
	if strings.TrimSpace(spec.Name) == "" {
		return fmt.Errorf("tool registry: %T has an empty tool name", tool)
	}
	if core.IsWebMCPToolName(spec.Name) {
		return fmt.Errorf("tool registry: %q is reserved for client/WebMCP tools", spec.Name)
	}
	origin := core.ToolOriginOf(tool)
	bindingID := core.ToolBindingIDOf(tool)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tools == nil {
		r.tools = make(map[string]core.Tool)
	}
	if r.visible == nil {
		r.visible = make(map[string]bool)
	}
	if r.registrations == nil {
		r.registrations = make(map[string]toolRegistration)
	}
	if r.shadows == nil {
		r.shadows = make(map[string][]toolRegistration)
	}
	if r.nextGeneration == 0 {
		r.nextGeneration = 1
	}

	existing, occupied := r.registrations[spec.Name]
	if occupied {
		sameBinding := sameToolInstance(existing.tool, tool) ||
			(bindingID != "" && bindingID == existing.bindingID && origin.SameOrigin(existing.origin))
		switch {
		case sameBinding:
			existing.tool = tool
			existing.visible = visible
			r.storeLocked(spec.Name, existing)
			return nil
		case override:
			r.shadows[spec.Name] = append(r.shadows[spec.Name], existing)
		case existing.origin.Class == core.ToolOriginCustomAgent && origin.Class == core.ToolOriginSkill:
			// Skill activation intentionally scopes over an agent custom tool.
			// The custom binding is restored when the skill override is removed.
			r.shadows[spec.Name] = append(r.shadows[spec.Name], existing)
		case origin.SameOrigin(existing.origin):
			// Same trusted origin replaced its binding (for example an MCP
			// catalogue refresh or a reconstructed builtin adapter).
		default:
			return &ToolNameConflictError{
				Name:           spec.Name,
				ExistingOrigin: existing.origin,
				IncomingOrigin: origin,
				ExistingType:   toolTypeName(existing.tool),
				IncomingType:   toolTypeName(tool),
			}
		}
	}

	registration := toolRegistration{
		tool:       tool,
		origin:     origin,
		bindingID:  bindingID,
		generation: r.nextGeneration,
		visible:    visible,
		reason:     reason,
	}
	r.nextGeneration++
	r.storeLocked(spec.Name, registration)
	return nil
}

func (r *ToolRegistry) storeLocked(name string, registration toolRegistration) {
	r.registrations[name] = registration
	r.tools[name] = registration.tool
	r.visible[name] = registration.visible
	r.specsDirty = true
}

func sameToolInstance(left, right core.Tool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	leftValue, rightValue := reflect.ValueOf(left), reflect.ValueOf(right)
	if leftValue.Type() != rightValue.Type() {
		return false
	}
	if !leftValue.Comparable() || !rightValue.Comparable() {
		return false
	}
	return leftValue.Interface() == rightValue.Interface()
}

func toolTypeName(tool core.Tool) string {
	if tool == nil {
		return "nil"
	}
	return reflect.TypeOf(tool).String()
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

// Binding returns the current provenance and binding generation for name.
func (r *ToolRegistry) Binding(name string) (ToolBinding, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	registration, ok := r.registrations[name]
	if !ok {
		return ToolBinding{}, false
	}
	return ToolBinding{Origin: registration.origin, Generation: registration.generation}, true
}

// BindingGeneration returns the current execution binding generation for name.
// It never reuses a generation for a previously removed name in this registry.
func (r *ToolRegistry) BindingGeneration(name string) (uint64, bool) {
	binding, ok := r.Binding(name)
	return binding.Generation, ok
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

// Unregister removes name. If the name was registered with RegisterOverride, the
// most recent shadowed binding is restored with a fresh binding generation.
func (r *ToolRegistry) Unregister(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.unregisterLocked(name)
}

func (r *ToolRegistry) unregisterLocked(name string) {
	if shadows := r.shadows[name]; len(shadows) > 0 {
		restored := shadows[len(shadows)-1]
		if len(shadows) == 1 {
			delete(r.shadows, name)
		} else {
			r.shadows[name] = shadows[:len(shadows)-1]
		}
		restored.generation = r.nextGeneration
		r.nextGeneration++
		r.storeLocked(name, restored)
		return
	}
	if _, ok := r.registrations[name]; ok {
		delete(r.registrations, name)
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
