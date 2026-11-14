package mcp

import (
	"strings"
	"testing"

	"github.com/term-llm/term-llm/internal/llm/core"
)

func TestValidateServerNameReservesWebMCPNamespace(t *testing.T) {
	for _, name := range []string{"webmcp", "WebMCP", "WEBMCP", "webmcp_", "webmcp."} {
		if err := ValidateServerName(name); err == nil {
			t.Fatalf("reserved server name %q accepted", name)
		}
		if !IsReservedServerName(name) {
			t.Fatalf("IsReservedServerName(%q) = false", name)
		}
	}
	for _, name := range []string{"webmcp2", "web", "web-mcp", "my-webmcp", "webmcp.-", "webmcp.other"} {
		if err := ValidateServerName(name); err != nil {
			t.Fatalf("ordinary server name %q rejected: %v", name, err)
		}
		if IsReservedServerName(name) {
			t.Fatalf("IsReservedServerName(%q) = true", name)
		}
	}
}

func catalogProvenanceTool(manager *Manager, server, original string, schema map[string]any) *MCPTool {
	return NewCatalogMCPTool(manager, CatalogTool{
		ID:           original,
		Server:       server,
		OriginalName: original,
		Name:         original,
		InputSchema:  schema,
	})
}

func TestMCPToolOriginUsesServerIdentityNotVisibleName(t *testing.T) {
	manager := &Manager{}
	tool := catalogProvenanceTool(manager, "remote", "spawn_agent", map[string]any{"type": "object"})
	origin := tool.ToolOrigin()
	if origin.Class != core.ToolOriginMCP {
		t.Fatalf("origin class = %q", origin.Class)
	}
	if origin.ID != "remote\x00spawn_agent" {
		t.Fatalf("origin id = %q", origin.ID)
	}

	legacy := NewMCPTool(manager, ToolSpec{Name: "spawn_agent"})
	legacyOrigin := legacy.ToolOrigin()
	if legacyOrigin.Class != core.ToolOriginMCP || legacyOrigin.ID != "legacy\x00spawn_agent" {
		t.Fatalf("legacy origin = %#v", legacyOrigin)
	}
	if legacyOrigin == origin {
		t.Fatal("legacy and catalogue origins collapsed")
	}
}

func TestMCPToolBindingIDTracksTargetAndCredentialsWithoutExposingSecrets(t *testing.T) {
	const secret = "super-secret-bearer-token"
	cfg := &Config{Servers: map[string]ServerConfig{
		"docs": {
			Type:    "http",
			URL:     "https://docs.example/mcp",
			Headers: map[string]string{"Authorization": "Bearer " + secret},
			Env:     map[string]string{"API_KEY": secret},
		},
	}}
	manager := &Manager{config: cfg}
	schema := map[string]any{"type": "object"}
	first := catalogProvenanceTool(manager, "docs", "search", schema).ToolBindingID()
	if strings.Contains(first, secret) || strings.Contains(first, "Authorization") || strings.Contains(first, "API_KEY") {
		t.Fatalf("binding id exposed configuration material: %q", first)
	}
	if got := catalogProvenanceTool(manager, "docs", "search", schema).ToolBindingID(); got != first {
		t.Fatalf("identical same-target refresh changed binding: %q vs %q", got, first)
	}

	cfg.Servers["docs"] = ServerConfig{Type: "http", URL: "https://other.example/mcp", Headers: map[string]string{"Authorization": "Bearer " + secret}}
	if got := catalogProvenanceTool(manager, "docs", "search", schema).ToolBindingID(); got == first {
		t.Fatal("target change reused binding id")
	}

	cfg.Servers["docs"] = ServerConfig{Type: "http", URL: "https://other.example/mcp", Headers: map[string]string{"Authorization": "Bearer changed"}}
	if got := catalogProvenanceTool(manager, "docs", "search", schema).ToolBindingID(); strings.Contains(got, "changed") {
		t.Fatalf("changed credential leaked into binding id: %q", got)
	}
}

func TestMCPToolBindingIDTracksSchemaAndManager(t *testing.T) {
	manager := &Manager{}
	schema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}
	first := catalogProvenanceTool(manager, "docs", "search", schema)
	firstID := first.ToolBindingID()
	if firstID == "" {
		t.Fatal("empty binding id")
	}

	// A fresh wrapper built from an identical catalogue refresh is idempotent.
	if got := catalogProvenanceTool(manager, "docs", "search", schema).ToolBindingID(); got != firstID {
		t.Fatalf("identical refresh changed binding id: %q vs %q", got, firstID)
	}

	changedSchema := map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "number"}}}
	if got := catalogProvenanceTool(manager, "docs", "search", changedSchema).ToolBindingID(); got == firstID {
		t.Fatal("schema change reused binding id")
	}
	if got := catalogProvenanceTool(&Manager{}, "docs", "search", schema).ToolBindingID(); got == firstID {
		t.Fatal("manager replacement reused binding id")
	}
	if got := catalogProvenanceTool(manager, "other", "search", schema).ToolBindingID(); got == firstID {
		t.Fatal("server change reused binding id")
	}
}
