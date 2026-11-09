package mcpstart

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyStartupConvenienceLoadsOnlyOnce(t *testing.T) {
	for _, implicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "implicit"}[implicit], func(t *testing.T) {
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"selected": {Command: os.Args[0], Env: map[string]string{mcptest.ChildEnv: "1"}}})
			path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "mcp.json")
			loads := 0
			var loadedManager *mcp.Manager
			opts := &MCPOptions{ImplicitSelection: implicit, LoadConfig: func(manager *mcp.Manager) error {
				loads++
				loadedManager = manager
				if err := manager.LoadConfig(); err != nil {
					return err
				}
				return os.Remove(path)
			}}
			provider := mockprovider.NewMockProvider("mock")
			manager, err := EnableMCPServersWithFeedback(context.Background(), "selected", engine.NewEngine(provider, nil), io.Discard, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer manager.StopAll()
			if loads != 1 || manager != loadedManager {
				t.Fatalf("loads=%d, reused manager=%v", loads, manager == loadedManager)
			}
		})
	}
}

func TestMCPPolicyEmptyPreparationHasNoConfigIO(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		loads := 0
		prepared, err := PrepareSelection("identity", " , , ", explicit, io.Discard, func(*mcp.Manager) error { loads++; return nil })
		if err != nil || loads != 0 || prepared.Manager != nil || prepared.Loaded {
			t.Fatalf("empty prepare=%+v loads=%d err=%v", prepared, loads, err)
		}
		manager, err := prepared.Enable(context.Background(), nil, io.Discard, nil)
		if err != nil || manager != nil || loads != 0 {
			t.Fatal("empty enable loaded or created a manager")
		}
	}
}

func TestMCPPolicyPreparedSelectionRechecksRevocationWithoutReloadingConnections(t *testing.T) {
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"selected": {Command: "must-not-start"}})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	loads := 0
	prepared, err := PrepareSelection("", "selected", false, io.Discard, func(manager *mcp.Manager) error { loads++; return manager.LoadConfig() })
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "mcp.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: {servers: {selected: {agents: []}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := prepared.Enable(context.Background(), nil, io.Discard, nil)
	if err != nil || manager != nil || loads != 1 || !prepared.Loaded || prepared.Active != "" || prepared.Durable != "" {
		t.Fatalf("revocation=%+v manager=%v loads=%d error=%v", prepared, manager, loads, err)
	}
}
