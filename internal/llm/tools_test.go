package llm

import (
	"context"
	"encoding/json"
	"testing"
)

type namedTestTool struct{ name string }

func (t *namedTestTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: t.name, Schema: map[string]any{"type": "object"}}
}

func (t *namedTestTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return TextOutput(t.name), nil
}

func (t *namedTestTool) Preview(args json.RawMessage) string { return t.name }

func TestToolRegistryAllSpecsSortedByName(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&namedTestTool{name: "zeta"})
	registry.Register(&namedTestTool{name: "alpha"})
	registry.Register(&namedTestTool{name: "middle"})

	specs := registry.AllSpecs()
	if len(specs) != 3 {
		t.Fatalf("len(AllSpecs()) = %d, want 3", len(specs))
	}
	got := []string{specs[0].Name, specs[1].Name, specs[2].Name}
	want := []string{"alpha", "middle", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AllSpecs() order = %v, want %v", got, want)
		}
	}
}

func TestToolSpecsForRequestPreservesSortedOrderAfterFiltering(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&namedTestTool{name: "zeta"})
	registry.Register(&namedTestTool{name: WebSearchToolName})
	registry.Register(&namedTestTool{name: "alpha"})
	registry.Register(&namedTestTool{name: ReadURLToolName})

	specs := ToolSpecsForRequest(registry, false)
	if len(specs) != 2 {
		t.Fatalf("len(ToolSpecsForRequest()) = %d, want 2", len(specs))
	}
	got := []string{specs[0].Name, specs[1].Name}
	want := []string{"alpha", "zeta"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ToolSpecsForRequest() order = %v, want %v", got, want)
		}
	}
}
