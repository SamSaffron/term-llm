package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

func TestRunnerDefaultMCPAgentScope(t *testing.T) {
	for _, child := range []bool{false, true} {
		for _, agentName := range []string{"migration-data", "developer"} {
			t.Run(agentName+"/child="+map[bool]string{false: "false", true: "true"}[child], func(t *testing.T) {
				writeServeMCPConfig(t, map[string]mcp.ServerConfig{"db": {Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}, Agents: []string{"migration-data"}}})
				root := t.TempDir()
				for _, name := range []string{"migration-data", "developer"} {
					dir := filepath.Join(root, name)
					if err := os.Mkdir(dir, 0700); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: "+name+"\nskills: none\nmcp:\n  - name: db\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}, Agents: config.AgentsConfig{SearchPaths: []string{root}}}
				runner := newCmdRunner(cfg, cmdRunnerOptions{}).(*cmdRunner)
				req := runpkg.Request{Platform: runpkg.PlatformWeb, AgentName: agentName, ProviderInstance: mockprovider.NewMockProvider("mock"), Cwd: t.TempDir(), DeferSession: true}
				if child {
					spawn := &SpawnAgentRunner{}
					req = spawn.buildChildExecutionRequest(t.Context(), runpkg.ChildRunRequest{Kind: runpkg.ChildRunSpawnAgent, AgentName: agentName, Prompt: "synthetic child"}, "child", false)
					req.ProviderInstance = mockprovider.NewMockProvider("mock")
					req.Cwd = t.TempDir()
					req.DeferSession = true
				}
				env, err := runner.prepare(t.Context(), req, nil)
				if agentName != "migration-data" {
					if err == nil {
						env.Close()
						t.Fatal("disallowed agent default started")
					}
					if !strings.Contains(err.Error(), "restricted to agents") {
						t.Fatalf("prepare = %v", err)
					}
				} else {
					if err != nil {
						t.Fatal(err)
					}
					defer env.Close()
					if len(env.runtime.mcpManager.AllTools()) == 0 {
						t.Fatal("allowed agent default did not start")
					}
				}
			})
		}
	}
}
