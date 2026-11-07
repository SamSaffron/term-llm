package mcpstart

import (
	"io"
	"os"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

func TestMCPStartAgentScope(t *testing.T) {
	for _, agent := range []string{"", "developer", "migration-data"} {
		t.Run("agent="+agent, func(t *testing.T) {
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"db": {Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}, Agents: []string{"migration-data"}}})
			eng := engine.NewEngine(nil, engine.NewToolRegistry())
			manager, err := EnableMCPServersWithFeedback(t.Context(), "db", eng, io.Discard, &MCPOptions{AgentName: agent})
			if agent == "migration-data" {
				if err != nil {
					t.Fatal(err)
				}
				defer manager.StopAll()
				if len(manager.AllTools()) == 0 {
					t.Fatal("allowed server has no tools")
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "restricted to agents") {
					t.Fatalf("startup = %v", err)
				}
				if len(eng.Tools().AllSpecsIncludingDeferred()) != 0 {
					t.Fatal("denied tool registered")
				}
			}
		})
	}
}

func TestMCPReloadRevokesAgentScope(t *testing.T) {
	server := mcp.ServerConfig{Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}}
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"db": server})
	eng := engine.NewEngine(nil, engine.NewToolRegistry())
	manager, err := EnableMCPServersWithFeedback(t.Context(), "db", eng, io.Discard, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.StopAll()
	server.Agents = []string{"migration-data"}
	if err := mcp.UpdateConfig(func(cfg *mcp.Config) error { cfg.AddServer("db", server); return nil }); err != nil {
		t.Fatal(err)
	}
	if err := manager.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if len(manager.AllTools()) != 0 || len(manager.EnabledServers()) != 0 {
		t.Fatal("revoked server still enabled")
	}
	if len(eng.Tools().AllSpecsIncludingDeferred()) != 0 {
		t.Fatal("revoked server tools still registered")
	}
	if _, err := manager.CallTool(t.Context(), "db__greet", nil); err == nil || !strings.Contains(err.Error(), "restricted to agents") {
		t.Fatalf("revoked tool call = %v", err)
	}
}
