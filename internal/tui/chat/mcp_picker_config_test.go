package chat

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyEmptySelectionCommandsUseStartupConfig(t *testing.T) {
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "available", Version: "1"}, nil)
	endpoint := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true}))
	defer endpoint.Close()
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"available": {URL: endpoint.URL}})
	manager := mcp.NewUnloadedManager()
	if err := manager.LoadConfig(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manager.StopAll)
	model := newTestChatModel(false)
	model.mcpManager = manager
	if model.mcpStr != "" || len(manager.EnabledServers()) != 0 {
		t.Fatal("expected an empty-selection launch")
	}
	// Commands must use startup's snapshot, never reload config on picker open.
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "mcp.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	model.cmdMcp(nil)
	if model.dialog.Type() != DialogMCPPicker {
		t.Fatal("/mcp did not open the configured-server picker")
	}
	model.dialog.Close()
	model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	if model.dialog.Type() != DialogMCPPicker {
		t.Fatal("Ctrl+T did not open the configured-server picker")
	}
	model.dialog.Close()
	if match := model.mcpFindServerMatch("avai"); match != "available" {
		t.Fatalf("server completion=%q", match)
	}
	_, indicator := model.statusLineMCPParts(lipgloss.NewStyle(), lipgloss.NewStyle())
	if !strings.Contains(indicator, "mcp:off") {
		t.Fatalf("empty-selection indicator=%q", indicator)
	}
	model.cmdMcp([]string{"status"})
	if model.dialog.Type() != DialogContent {
		t.Fatal("/mcp status did not open status content")
	}
	model.dialog.Close()
	updates := make(chan mcp.StatusUpdate, 10)
	manager.SetStatusChannel(updates)
	model.cmdMcp([]string{"start", "available"})
	if !slices.Contains(manager.EnabledServers(), "available") || model.mcpStr != "available" {
		t.Fatalf("/mcp start did not select and enable server: enabled=%v selection=%q", manager.EnabledServers(), model.mcpStr)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for {
		select {
		case update := <-updates:
			if update.Error != nil {
				t.Fatal(update.Error)
			}
			if update.Status == mcp.StatusReady {
				return
			}
		case <-ctx.Done():
			t.Fatal("/mcp start never reached ready:", ctx.Err())
		}
	}
}
