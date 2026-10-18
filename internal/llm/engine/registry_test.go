package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/core"
)

type registryTestTool struct{ name string }

func (tool registryTestTool) Spec() core.ToolSpec { return core.ToolSpec{Name: tool.name} }
func (registryTestTool) Execute(context.Context, json.RawMessage) (core.ToolOutput, error) {
	return core.TextOutput("ok"), nil
}
func (registryTestTool) Preview(json.RawMessage) string { return "" }

func TestToolRegistryDeferredVisibilityDoesNotAffectExecution(t *testing.T) {
	registry := NewToolRegistry()
	registry.RegisterDeferred(registryTestTool{name: "deferred"})
	if _, ok := registry.Get("deferred"); !ok {
		t.Fatal("deferred tool must remain executable")
	}
	if got := registry.AllSpecs(); len(got) != 0 {
		t.Fatalf("deferred tool leaked into provider specs: %#v", got)
	}
	if !registry.SetVisibility("deferred", true) {
		t.Fatal("could not expose deferred tool")
	}
	if got := registry.AllSpecs(); len(got) != 1 || got[0].Name != "deferred" {
		t.Fatalf("visible specs = %#v", got)
	}
}
