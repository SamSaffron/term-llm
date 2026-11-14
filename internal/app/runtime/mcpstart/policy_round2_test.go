package mcpstart

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyImplicitStartupFiltersButExplicitRequestFails(t *testing.T) {
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"denied": {Command: "must-not-start"}})
	root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: {servers: {denied: {agents: []}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	var warnings bytes.Buffer
	manager, err := EnableMCPServersWithFeedback(context.Background(), "denied,removed", nil, &warnings, &MCPOptions{ImplicitSelection: true})
	if err != nil || manager != nil {
		t.Fatalf("implicit startup failed: %v %v", manager, err)
	}
	if warnings.Len() == 0 {
		t.Fatal("implicit cleanup had no warning")
	}
	_, err = EnableMCPServersWithFeedback(context.Background(), "denied", nil, &warnings, &MCPOptions{})
	var denied *mcp.PermissionError
	if !errors.As(err, &denied) {
		t.Fatalf("explicit request not rejected: %v", err)
	}
}

func TestMCPPolicyRestoreReportsLoadedAndSkipsEmptySelection(t *testing.T) {
	for _, failure := range []string{"none", "connections", "policy"} {
		t.Run(failure, func(t *testing.T) {
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"open": {Command: "must-not-start"}})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			if failure == "connections" {
				if err := os.WriteFile(filepath.Join(root, "mcp.json"), []byte("{invalid"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "policy" {
				if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("mcp: {servers: null}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var warnings bytes.Buffer
			prepared, err := PrepareSelection("", "open,removed", false, &warnings, nil)
			if err != nil {
				t.Fatal(err)
			}
			cleaned, loaded := prepared.Active, prepared.Loaded
			if failure == "none" {
				if !loaded || cleaned != "open" {
					t.Fatalf("successful restore=%q loaded=%v", cleaned, loaded)
				}
			} else if loaded || cleaned != "" {
				t.Fatalf("failed restore=%q loaded=%v", cleaned, loaded)
			}
			warnings.Reset()
			if prepared, err := PrepareSelection("", " , , ", false, &warnings, nil); err != nil || prepared.Active != "" || prepared.Loaded || prepared.Manager != nil || warnings.Len() != 0 {
				t.Fatalf("empty restore loaded or warned: %+v %v %s", prepared, err, warnings.String())
			}
		})
	}
}
