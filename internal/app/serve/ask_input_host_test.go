package serve

import (
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/tools"
	"testing"
)

func TestSetupToolManagerKeepsAutoToolsScopedAndMergesExplicitTools(t *testing.T) {
	cfg := &config.Config{Tools: config.ToolsConfig{Enabled: []string{tools.GrepToolName}}}
	defaultsEngine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	defaults := SessionSettings{AutoTools: []string{tools.ReadFileToolName}}
	defaultsMgr, err := defaults.SetupToolManager(cfg, defaultsEngine)
	if err != nil {
		t.Fatalf("SetupToolManager defaults: %v", err)
	}
	defer defaultsMgr.ApprovalMgr.Close()
	if _, ok := defaultsMgr.Registry.Get(tools.ReadFileToolName); !ok {
		t.Fatalf("auto tool %q was not registered", tools.ReadFileToolName)
	}
	if _, ok := defaultsMgr.Registry.Get(tools.GrepToolName); ok {
		t.Fatal("auto tools unexpectedly activated the global default tool set")
	}

	engine := engine.NewEngine(mockprovider.NewMockProvider("mock"), nil)
	settings := SessionSettings{
		Tools:     tools.ShellToolName,
		AutoTools: []string{tools.ReadFileToolName, tools.ViewImageToolName},
	}
	toolMgr, err := settings.SetupToolManager(cfg, engine)
	if err != nil {
		t.Fatalf("SetupToolManager: %v", err)
	}
	defer toolMgr.ApprovalMgr.Close()
	for _, name := range []string{tools.ShellToolName, tools.ReadFileToolName, tools.ViewImageToolName} {
		if _, ok := toolMgr.Registry.Get(name); !ok {
			t.Fatalf("tool %q was not registered", name)
		}
	}
	if _, ok := toolMgr.Registry.Get(tools.GrepToolName); ok {
		t.Fatal("CLI tool selection should still replace config defaults")
	}
	if settings.Tools != tools.ShellToolName {
		t.Fatalf("raw configured Tools changed to %q", settings.Tools)
	}
}
