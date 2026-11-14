package serve

import (
	"testing"

	"github.com/term-llm/term-llm/internal/llm/engine"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/mcp"
)

func TestMCPCleanupUsesServerOriginNotNamePrefix(t *testing.T) {
	rt := &serveRuntime{engine: engine.NewEngine(mockprovider.NewMockProvider("cleanup"), nil)}
	for _, name := range []string{"docs.test__local", "docs_test__local"} {
		if err := rt.engine.RegisterTool(serveMCPFakeTool{name: name}); err != nil {
			t.Fatal(err)
		}
	}
	remote := mcp.NewCatalogMCPTool(nil, mcp.CatalogTool{Server: "docs.test", OriginalName: "action", Name: "docs_test__action"})
	if err := rt.engine.RegisterTool(remote); err != nil {
		t.Fatal(err)
	}
	rt.unregisterMCPServerToolsLocked("docs.test")
	if _, ok := rt.engine.Tools().Get("docs_test__action"); ok {
		t.Fatal("sanitized remote tool survived server cleanup")
	}
	for _, name := range []string{"docs.test__local", "docs_test__local"} {
		if _, ok := rt.engine.Tools().Get(name); !ok {
			t.Fatalf("server cleanup deleted unrelated local tool %q", name)
		}
	}
}
