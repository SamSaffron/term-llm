package serve

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	appruntime "github.com/samsaffron/term-llm/internal/app/runtime"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
)

func TestMCPPolicyRunnerLoadsSelectionOnce(t *testing.T) {
	for _, mode := range []string{"implicit", "explicit", "borrowed", "empty", "failed", "denied", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			writeServeMCPConfig(t, map[string]mcp.ServerConfig{"selected": {Command: os.Args[0], Env: map[string]string{runServeMCPHandlerTestServerEnv: "1"}}})
			writeServePolicy(t, "mcp: {servers: {selected: {agents: [single-load]}}}")
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			dir := filepath.Join(root, "agents", "single-load")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte("name: single-load\n"), 0600); err != nil {
				t.Fatal(err)
			}
			connections := filepath.Join(root, "mcp.json")
			if mode == "failed" {
				if err := os.WriteFile(connections, []byte("{temporary"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cfg := &config.Config{DefaultProvider: "mock", Providers: map[string]config.ProviderConfig{"mock": {Model: "mock-model"}}}
			loads := 0
			var loadedManager *mcp.Manager
			var warnings bytes.Buffer
			opts := cmdRunnerOptions{ErrWriter: &warnings, LoadMCPConfig: func(manager *mcp.Manager) error {
				loads++
				loadedManager = manager
				if err := manager.LoadConfig(); err != nil {
					return err
				}
				// Any second connection-config read, even one bypassing this loader,
				// would lose the selection. Startup must use the loaded snapshot.
				if mode == "revoked" {
					if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: {servers: {selected: {agents: []}}}"), 0600); err != nil {
						return err
					}
				}
				return os.Remove(connections)
			}}
			provider := mockprovider.NewMockProvider("mock")
			req := runpkg.Request{AgentName: "single-load", MCP: "selected", MCPExplicit: mode == "explicit", ProviderInstance: provider, Cwd: t.TempDir(), DeferSession: true}
			if mode == "borrowed" {
				req.Engine = appruntime.NewEngine(provider, cfg)
			}
			if mode == "empty" {
				req.MCP = ""
			}
			if mode == "denied" {
				req.AgentName = ""
			}
			env, err := newCmdRunner(cfg, opts).(*cmdRunner).prepare(context.Background(), req, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer env.Close()
			want := 1
			if mode == "empty" || mode == "borrowed" {
				want = 0
			}
			if loads != want {
				t.Fatalf("config loads=%d want=%d", loads, want)
			}
			enabled := mode == "implicit" || mode == "explicit"
			if enabled {
				if env.runtime.mcpManager != loadedManager || len(loadedManager.EnabledServers()) != 1 {
					t.Fatal("startup replaced preparation's loaded manager")
				}
			} else if env.runtime.mcpManager != nil {
				t.Fatal("empty/failed/denied/borrowed run started another manager")
			}
			if mode == "failed" && env.runtime.mcpSetting != "selected" {
				t.Fatal("failed load erased durable selection")
			}
			if (mode == "denied" || mode == "revoked") && env.runtime.mcpSetting != "" {
				t.Fatal("denied implicit selection was not cleaned")
			}
			if mode == "revoked" && (env.runtime.mcpManagerSnapshot() != nil || len(loadedManager.EnabledServers()) != 0) {
				t.Fatal("late revocation did not safely hand off a nil manager")
			}
			if mode == "empty" && loadedManager != nil {
				t.Fatal("empty selection built a manager")
			}
			if mode == "borrowed" && loadedManager != nil {
				t.Fatal("borrowed engine built a discarded manager")
			}
		})
	}
}
