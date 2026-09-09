package llm

import (
	"context"
	"encoding/json"
	"os"
	"testing"
)

func TestClaudeGeneratedMCPConfigAllowsLongRunningTools(t *testing.T) {
	provider := NewClaudeBinProvider("sonnet", nil)
	path := provider.createHTTPMCPConfig(context.Background(), []ToolSpec{{
		Name: "spawn_agent", Schema: map[string]any{"type": "object"},
	}}, false)
	if path == "" {
		t.Fatal("createHTTPMCPConfig returned empty path")
	}
	t.Cleanup(provider.CleanupMCP)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]struct {
			Timeout int `json:"timeout"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	if got := config.Servers["term-llm"].Timeout; got != claudeMCPToolTimeoutMillis {
		t.Fatalf("timeout = %d, want %d", got, claudeMCPToolTimeoutMillis)
	}
}
