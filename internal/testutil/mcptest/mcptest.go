// Package mcptest provides the shared real MCP subprocess used by tests.
package mcptest

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	internalmcp "github.com/samsaffron/term-llm/internal/mcp"
)

const ChildEnv = "TERM_LLM_SERVE_MCP_HANDLER_TEST_SERVER"

type greetingParams struct {
	Name string `json:"name"`
}

// RunIfChild dispatches the current test binary as the real stdio MCP server.
func RunIfChild() {
	if os.Getenv(ChildEnv) == "" {
		return
	}
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "serve-mcp-handler-test", Version: "v0.0.1"}, nil)
	sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet", Description: "say hi"}, func(ctx context.Context, req *sdkmcp.CallToolRequest, args greetingParams) (*sdkmcp.CallToolResult, any, error) {
		return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hi " + args.Name}}}, nil, nil
	})
	if err := server.Run(context.Background(), &sdkmcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
	os.Exit(0)
}

// WriteConfig writes an MCP configuration beneath a test-owned config home.
func WriteConfig(t testing.TB, servers map[string]internalmcp.ServerConfig) {
	t.Helper()
	configHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configHome)
	path := filepath.Join(configHome, "term-llm", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	data, err := json.Marshal(internalmcp.Config{Servers: servers})
	if err != nil {
		t.Fatalf("Marshal config: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
}
