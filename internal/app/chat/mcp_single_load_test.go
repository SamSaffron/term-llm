package chat

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/term-llm/term-llm/internal/app/runtime/approval"
	"github.com/term-llm/term-llm/internal/app/runtime/bootstrap"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
	"github.com/term-llm/term-llm/internal/tools"
)

func TestMCPPolicyChatLoadsSelectionOnce(t *testing.T) {
	for _, mode := range []string{"explicit", "implicit", "empty", "failed", "empty-failed"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "single-load", Version: "1"}, nil)
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{}, nil, nil
			})
			endpoint := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true}))
			defer endpoint.Close()
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"selected": {URL: endpoint.URL}})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("default_provider: debug\nproviders: {debug: {model: fast}}\nsessions: {enabled: true}\nskills: {enabled: false}\nchat: {terminal_title: off}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			connections := filepath.Join(root, "mcp.json")
			if mode == "failed" || mode == "empty-failed" {
				if err := os.WriteFile(connections, []byte("{temporary"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := testInvocation()
			var warnings bytes.Buffer
			r.deps.Stderr = &warnings
			r.opts.MCP = "selected"
			r.opts.Changed.MCP = mode == "explicit"
			if mode == "empty" || mode == "empty-failed" {
				r.opts.MCP = ""
			}
			r.deps.SessionStore.DBPath = filepath.Join(t.TempDir(), "sessions.db")
			r.deps.MemoryPaths.DBPath = filepath.Join(t.TempDir(), "memory.db")
			r.deps.LoadConfig, r.deps.CreateDebugLogger = bootstrap.LoadConfigWithSetup, bootstrap.CreateDebugLogger
			r.deps.StdoutIsTerminal = func() bool { return false }
			r.deps.ResolveApproval = func(cfg *config.Config, persisted *session.SessionApprovalMode, carried *tools.ApprovalMode, _ string) (approval.ResolvedMode, error) {
				return approval.Resolve(approval.ResolutionInput{Surface: approval.SurfaceChat, Config: cfg, Session: persisted, CLI: carried})
			}
			loads := 0
			var loadedManager *mcp.Manager
			r.deps.LoadMCPConfig = func(manager *mcp.Manager) error {
				loads++
				loadedManager = manager
				if err := manager.LoadConfig(); err != nil {
					return err
				}
				return os.Remove(connections)
			}
			rt, err := r.buildChatSessionRuntime(context.Background(), chatSessionLaunch{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer rt.cleanupResources()
			want := 1
			if loads != want {
				t.Fatalf("loads=%d want=%d", loads, want)
			}
			if mode == "empty" {
				if len(loadedManager.AvailableServers()) != 1 {
					t.Fatal("empty-selection launch did not load configured servers")
				}
				rt.model.Update(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
				if view := rt.model.View().Content; !strings.Contains(view, "MCP Servers") || !strings.Contains(view, "selected") {
					t.Fatalf("Ctrl+T from empty launch did not show configured picker: %s", view)
				}
				rt.model.Update(tea.KeyPressMsg{Code: tea.KeyEscape})
				for _, char := range "/mcp" {
					rt.model.Update(tea.KeyPressMsg{Code: char, Text: string(char)})
				}
				rt.model.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
				if view := rt.model.View().Content; !strings.Contains(view, "MCP Servers") || !strings.Contains(view, "selected") {
					t.Fatalf("/mcp from empty launch did not show configured picker: %s", view)
				}
			}
			if mode == "empty-failed" && !strings.Contains(warnings.String(), "failed to load MCP config") {
				t.Fatalf("empty-selection load failure did not warn: %s", warnings.String())
			}
			if (mode == "explicit" || mode == "implicit") && len(loadedManager.EnabledServers()) != 1 {
				t.Fatal("chat startup replaced its loaded manager")
			}
			if (mode == "failed" || mode == "empty" || mode == "empty-failed") && len(loadedManager.EnabledServers()) != 0 {
				t.Fatal("failed load or empty selection started a server")
			}
		})
	}
}
