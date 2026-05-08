package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
)

type namedTestTool struct{ name string }

type finishingNamedTestTool struct{ namedTestTool }

func (t *namedTestTool) Spec() ToolSpec {
	return ToolSpec{Name: t.name, Description: t.name, Schema: map[string]any{"type": "object"}}
}

func (t *namedTestTool) Execute(ctx context.Context, args json.RawMessage) (ToolOutput, error) {
	return TextOutput(t.name), nil
}

func (t *namedTestTool) Preview(args json.RawMessage) string { return t.name }

func (t *finishingNamedTestTool) IsFinishingTool() bool { return true }

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

func TestToolRegistryConcurrentAccess(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&finishingNamedTestTool{namedTestTool{name: "base"}})

	const (
		writers    = 2
		readers    = 6
		iterations = 2000
	)

	start := make(chan struct{})
	var wg sync.WaitGroup

	for writerID := 0; writerID < writers; writerID++ {
		wg.Add(1)
		go func(writerID int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				name := fmt.Sprintf("writer-%d-tool-%d", writerID, i)
				registry.Register(&finishingNamedTestTool{namedTestTool{name: name}})
				if i%2 == 0 {
					registry.Unregister(name)
				}
			}
		}(writerID)
	}

	for readerID := 0; readerID < readers; readerID++ {
		wg.Add(1)
		go func(readerID int) {
			defer wg.Done()
			<-start
			for i := 0; i < iterations; i++ {
				if _, ok := registry.Get("base"); !ok {
					t.Fatalf("Get(base) = missing")
				}
				if !registry.IsFinishingTool("base") {
					t.Fatalf("IsFinishingTool(base) = false, want true")
				}
				_ = registry.AllSpecs()
			}
		}(readerID)
	}

	close(start)
	wg.Wait()
}

func TestToolRegistryAllSpecsReturnedSliceIsIndependent(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&namedTestTool{name: "alpha"})
	registry.Register(&namedTestTool{name: "beta"})

	specs := registry.AllSpecs()
	if len(specs) != 2 {
		t.Fatalf("len(AllSpecs()) = %d, want 2", len(specs))
	}
	specs[0].Name = "mutated"

	got := registry.AllSpecs()
	if got[0].Name != "alpha" {
		t.Fatalf("AllSpecs() reused caller-mutated slice, got first name %q", got[0].Name)
	}
}

func TestToolRegistryAllSpecsInvalidatesCache(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(&namedTestTool{name: "alpha"})

	_ = registry.AllSpecs()
	registry.Register(&namedTestTool{name: "beta"})
	got := registry.AllSpecs()
	if len(got) != 2 || got[0].Name != "alpha" || got[1].Name != "beta" {
		t.Fatalf("after Register, AllSpecs() = %#v, want alpha,beta", got)
	}

	registry.Unregister("alpha")
	got = registry.AllSpecs()
	if len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("after Unregister, AllSpecs() = %#v, want beta", got)
	}
}

func BenchmarkToolRegistryAllSpecs(b *testing.B) {
	registry := NewToolRegistry()
	const toolCount = 128
	for i := 0; i < toolCount; i++ {
		registry.Register(&namedTestTool{name: fmt.Sprintf("tool-%03d", i)})
	}

	b.ReportAllocs()
	if specs := registry.AllSpecs(); len(specs) != toolCount {
		b.Fatalf("len(AllSpecs()) = %d, want %d", len(specs), toolCount)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		specs := registry.AllSpecs()
		if len(specs) != toolCount {
			b.Fatalf("len(AllSpecs()) = %d, want %d", len(specs), toolCount)
		}
	}
}
