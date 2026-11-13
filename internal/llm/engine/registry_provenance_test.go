package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type provenanceTool struct {
	name    string
	origin  core.ToolOrigin
	binding string
}

func (t provenanceTool) Spec() core.ToolSpec {
	return core.ToolSpec{Name: t.name, Description: "provenance fixture", Schema: map[string]any{"type": "object"}}
}
func (t provenanceTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("ok"), nil
}
func (provenanceTool) Preview(json.RawMessage) string { return "" }
func (t provenanceTool) ToolOrigin() core.ToolOrigin  { return t.origin }
func (t provenanceTool) ToolBindingID() string        { return t.binding }

type plainProvenanceTool struct {
	name    string
	variant string
}

func (t plainProvenanceTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (t plainProvenanceTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("plain"), nil
}
func (plainProvenanceTool) Preview(json.RawMessage) string { return "" }

type otherPlainProvenanceTool struct{ name string }

func (t otherPlainProvenanceTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (t otherPlainProvenanceTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("other"), nil
}
func (otherPlainProvenanceTool) Preview(json.RawMessage) string { return "" }

func TestToolRegistryRejectsCrossOriginReplacement(t *testing.T) {
	registry := NewToolRegistry()
	builtin := provenanceTool{name: "spawn_agent", origin: core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: "spawn_agent"}, binding: "builtin"}
	if err := registry.Register(builtin); err != nil {
		t.Fatalf("register builtin: %v", err)
	}
	before, ok := registry.Binding("spawn_agent")
	if !ok {
		t.Fatal("builtin binding missing")
	}

	remote := provenanceTool{name: "spawn_agent", origin: core.ToolOrigin{Class: core.ToolOriginMCP, ID: "remote\x00spawn_agent"}, binding: "remote"}
	err := registry.Register(remote)
	if err == nil {
		t.Fatal("cross-origin replacement was accepted")
	}
	var conflict *ToolNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %T %v, want *ToolNameConflictError", err, err)
	}
	if conflict.Name != "spawn_agent" || conflict.ExistingOrigin.Class != core.ToolOriginBuiltin || conflict.IncomingOrigin.Class != core.ToolOriginMCP {
		t.Fatalf("conflict = %#v", conflict)
	}

	installed, ok := registry.Get("spawn_agent")
	if !ok || installed != builtin {
		t.Fatalf("existing binding changed after rejection: %#v", installed)
	}
	after, _ := registry.Binding("spawn_agent")
	if after != before {
		t.Fatalf("binding generation changed on rejected registration: before=%#v after=%#v", before, after)
	}
}

func TestToolRegistrySameOriginRefreshKeepsOrChangesGeneration(t *testing.T) {
	registry := NewToolRegistry()
	origin := core.ToolOrigin{Class: core.ToolOriginMCP, ID: "docs\x00search"}
	first := provenanceTool{name: "docs__search", origin: origin, binding: "schema-v1"}
	if err := registry.Register(first); err != nil {
		t.Fatalf("first register: %v", err)
	}
	generation, _ := registry.BindingGeneration("docs__search")

	if err := registry.Register(provenanceTool{name: "docs__search", origin: origin, binding: "schema-v1"}); err != nil {
		t.Fatalf("identical refresh: %v", err)
	}
	if got, _ := registry.BindingGeneration("docs__search"); got != generation {
		t.Fatalf("identical refresh changed generation: got %d want %d", got, generation)
	}

	if err := registry.Register(provenanceTool{name: "docs__search", origin: origin, binding: "schema-v2"}); err != nil {
		t.Fatalf("same-origin replacement: %v", err)
	}
	if got, _ := registry.BindingGeneration("docs__search"); got <= generation {
		t.Fatalf("same-origin new binding reused generation: got %d old %d", got, generation)
	}
}

func TestToolRegistryUnclassifiedFallbackRequiresCrossTypeOverride(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(plainProvenanceTool{name: "fixture", variant: "first"}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	first, _ := registry.BindingGeneration("fixture")
	if err := registry.Register(plainProvenanceTool{name: "fixture", variant: "second"}); err != nil {
		t.Fatalf("same concrete type refresh: %v", err)
	}
	if got, _ := registry.BindingGeneration("fixture"); got <= first {
		t.Fatalf("new unclassified instance reused generation: got %d old %d", got, first)
	}
	if err := registry.Register(otherPlainProvenanceTool{name: "fixture"}); err == nil {
		t.Fatal("unrelated unclassified type silently replaced the binding")
	}
	if err := registry.RegisterOverride(otherPlainProvenanceTool{name: "fixture"}, "intentional fixture override"); err != nil {
		t.Fatalf("explicit unclassified override rejected: %v", err)
	}
	if got, _ := registry.BindingGeneration("fixture"); got <= first {
		t.Fatalf("unclassified replacement reused generation: got %d old %d", got, first)
	}
}

func TestToolRegistryOverrideRestoresPreviousBinding(t *testing.T) {
	registry := NewToolRegistry()
	custom := provenanceTool{name: "check_build", origin: core.ToolOrigin{Class: core.ToolOriginCustomAgent, ID: "/agent"}, binding: "custom"}
	if err := registry.Register(custom); err != nil {
		t.Fatalf("register custom: %v", err)
	}
	customGeneration, _ := registry.BindingGeneration("check_build")

	skill := provenanceTool{name: "check_build", origin: core.ToolOrigin{Class: core.ToolOriginSkill, ID: "/skill"}, binding: "skill"}
	if err := registry.RegisterDeferredOverride(skill, "skill activation"); err != nil {
		t.Fatalf("override: %v", err)
	}
	if got, ok := registry.Get("check_build"); !ok || got != skill {
		t.Fatalf("override not installed: %#v", got)
	}
	if registry.IsVisible("check_build") {
		t.Fatal("deferred override leaked into provider-visible specs")
	}
	overrideGeneration, _ := registry.BindingGeneration("check_build")
	if overrideGeneration <= customGeneration {
		t.Fatalf("override did not change generation: override=%d custom=%d", overrideGeneration, customGeneration)
	}
	binding, _ := registry.Binding("check_build")
	if binding.Origin.Class != core.ToolOriginSkill {
		t.Fatalf("override origin = %#v", binding.Origin)
	}

	registry.Unregister("check_build")
	if got, ok := registry.Get("check_build"); !ok || got != custom {
		t.Fatalf("restore failed: %#v", got)
	}
	if !registry.IsVisible("check_build") {
		t.Fatal("restore did not recover provider visibility")
	}
	restoredGeneration, _ := registry.BindingGeneration("check_build")
	if restoredGeneration <= overrideGeneration {
		t.Fatalf("restore reused generation: restored=%d override=%d", restoredGeneration, overrideGeneration)
	}
	if binding, _ := registry.Binding("check_build"); binding.Origin.Class != core.ToolOriginCustomAgent {
		t.Fatalf("restore origin = %#v", binding.Origin)
	}

	registry.Unregister("check_build")
	if _, ok := registry.Get("check_build"); ok {
		t.Fatal("unregister left a restored tool behind")
	}
}

func TestToolRegistrySkillOriginScopesOverCustomAndRestores(t *testing.T) {
	registry := NewToolRegistry()
	custom := provenanceTool{name: "check_build", origin: core.ToolOrigin{Class: core.ToolOriginCustomAgent, ID: "/agent"}, binding: "custom"}
	if err := registry.Register(custom); err != nil {
		t.Fatalf("register custom: %v", err)
	}
	otherAgent := provenanceTool{name: "check_build", origin: core.ToolOrigin{Class: core.ToolOriginCustomAgent, ID: "/other-agent"}, binding: "other"}
	if err := registry.Register(otherAgent); err == nil {
		t.Fatal("a second agent custom tool replaced the first")
	}

	skill := provenanceTool{name: "check_build", origin: core.ToolOrigin{Class: core.ToolOriginSkill}, binding: "skill"}
	if err := registry.Register(skill); err != nil {
		t.Fatalf("skill did not scope over custom: %v", err)
	}
	if got, _ := registry.Get("check_build"); got != skill {
		t.Fatalf("skill binding = %#v", got)
	}
	registry.Unregister("check_build")
	if got, _ := registry.Get("check_build"); got != custom {
		t.Fatalf("custom binding was not restored: %#v", got)
	}
}

func TestToolRegistryBindingGenerationNeverReused(t *testing.T) {
	registry := NewToolRegistry()
	first := plainProvenanceTool{name: "fixture"}
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	firstGeneration, _ := registry.BindingGeneration("fixture")
	registry.Unregister("fixture")
	if _, ok := registry.Binding("fixture"); ok {
		t.Fatal("binding remained after unregister")
	}
	if err := registry.Register(first); err != nil {
		t.Fatal(err)
	}
	secondGeneration, _ := registry.BindingGeneration("fixture")
	if secondGeneration <= firstGeneration {
		t.Fatalf("generation reused after remove/re-register: first=%d second=%d", firstGeneration, secondGeneration)
	}
}

func TestToolRegistryRejectsNilAndNamelessTools(t *testing.T) {
	registry := NewToolRegistry()
	if err := registry.Register(nil); err == nil {
		t.Fatal("nil tool accepted")
	}
	if err := registry.Register(plainProvenanceTool{}); err == nil {
		t.Fatal("empty tool name accepted")
	}
}

func TestToolRegistryRepeatedOverrideRestoresOriginalOnce(t *testing.T) {
	registry := NewToolRegistry()
	original := provenanceTool{name: "shared", origin: core.ToolOrigin{Class: core.ToolOriginBuiltin, ID: "original"}}
	override := provenanceTool{name: "shared", origin: core.ToolOrigin{Class: core.ToolOriginHost, ID: "override"}}
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := registry.RegisterOverride(override, "scoped replacement"); err != nil {
			t.Fatal(err)
		}
	}
	registry.Unregister("shared")
	if current, _ := registry.Get("shared"); current != original {
		t.Fatalf("repeated override restored itself: %#v", current)
	}
	registry.Unregister("shared")
	if _, ok := registry.Get("shared"); ok {
		t.Fatal("repeated override left an extra shadow")
	}
}

type interfaceComparisonTool struct {
	name string
	data any
}

func (t interfaceComparisonTool) Spec() core.ToolSpec { return core.ToolSpec{Name: t.name} }
func (interfaceComparisonTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("ok"), nil
}
func (interfaceComparisonTool) Preview(json.RawMessage) string { return "" }

func TestToolRegistryComparisonHandlesUncomparableInterfacePayload(t *testing.T) {
	registry := NewToolRegistry()
	for range 2 {
		if err := registry.Register(interfaceComparisonTool{name: "payload", data: map[string]string{"key": "value"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestToolRegistryReservesClientNamespace(t *testing.T) {
	registry := NewToolRegistry()
	for _, name := range []string{"webmcp__phone", "webmcp___action"} {
		if err := registry.Register(provenanceTool{name: name, origin: core.ToolOrigin{Class: core.ToolOriginMCP, ID: "remote"}}); err == nil {
			t.Fatalf("reserved client name %q registered as server tool", name)
		}
		if _, ok := registry.Get(name); ok {
			t.Fatalf("rejected name %q remains executable", name)
		}
	}
}

func TestToolRegistryConditionalCleanupCannotRemoveReplacement(t *testing.T) {
	registry := NewToolRegistry()
	original := &provenanceTool{name: "shared", origin: core.ToolOrigin{Class: core.ToolOriginMCP, ID: "catalogue"}, binding: "original"}
	replacement := &provenanceTool{name: "shared", origin: core.ToolOrigin{Class: core.ToolOriginHost, ID: "host"}, binding: "replacement"}
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	if err := registry.RegisterOverride(replacement, "new scoped owner"); err != nil {
		t.Fatal(err)
	}
	if registry.UnregisterIf("shared", original) {
		t.Fatal("stale owner deleted replacement")
	}
	if current, _ := registry.Get("shared"); current != replacement {
		t.Fatal("replacement changed during rejected cleanup")
	}
	if !registry.UnregisterIf("shared", replacement) {
		t.Fatal("actual owner could not clean up")
	}
	if current, _ := registry.Get("shared"); current != original {
		t.Fatal("conditional cleanup did not restore prior binding")
	}
}

func TestToolRegistryConditionalCleanupAcceptsIdempotentRefresh(t *testing.T) {
	registry := NewToolRegistry()
	origin := core.ToolOrigin{Class: core.ToolOriginMCP, ID: "same-owner"}
	original := &provenanceTool{name: "remote", origin: origin, binding: "stable"}
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&provenanceTool{name: "remote", origin: origin, binding: "stable"}); err != nil {
		t.Fatal(err)
	}
	if !registry.UnregisterIf("remote", original) {
		t.Fatal("idempotent wrapper refresh stranded owned cleanup")
	}
	if err := registry.Register(original); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(&provenanceTool{name: "remote", origin: origin, binding: "new-target"}); err != nil {
		t.Fatal(err)
	}
	if registry.UnregisterIf("remote", original) {
		t.Fatal("old binding deleted a new target")
	}
}
