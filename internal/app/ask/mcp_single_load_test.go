package ask

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/term-llm/term-llm/internal/config"
	"github.com/term-llm/term-llm/internal/llm/core"
	"github.com/term-llm/term-llm/internal/llm/mockprovider"
	"github.com/term-llm/term-llm/internal/mcp"
	"github.com/term-llm/term-llm/internal/session"
	"github.com/term-llm/term-llm/internal/testutil/mcptest"
)

type mcpLoadCheckingProvider struct {
	*mockprovider.MockProvider
	check func() error
}

func (p *mcpLoadCheckingProvider) Stream(ctx context.Context, req core.Request) (core.Stream, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	return p.MockProvider.Stream(ctx, req)
}

func TestMCPPolicyAskLoadsSelectionOnce(t *testing.T) {
	for _, mode := range []string{"explicit", "implicit", "resume", "explicit-resume", "empty", "failed-resume"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_DATA_HOME", t.TempDir())
			t.Chdir(t.TempDir())
			server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "single-load", Version: "1"}, nil)
			sdkmcp.AddTool(server, &sdkmcp.Tool{Name: "greet", Description: "fixture"}, func(context.Context, *sdkmcp.CallToolRequest, struct{}) (*sdkmcp.CallToolResult, any, error) {
				return &sdkmcp.CallToolResult{Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: "hello"}}}, nil, nil
			})
			endpoint := httptest.NewServer(sdkmcp.NewStreamableHTTPHandler(func(*http.Request) *sdkmcp.Server { return server }, &sdkmcp.StreamableHTTPOptions{Stateless: true}))
			defer endpoint.Close()
			mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"selected": {URL: endpoint.URL}})
			root := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm")
			db := filepath.Join(t.TempDir(), "sessions.db")
			body := fmt.Sprintf("default_provider: mock\nproviders: {mock: {model: mock-model}}\nsessions: {enabled: true, path: %q}\nskills: {enabled: false}\nmcp: {servers: {selected: {agents: [single-load]}}}\n", db)
			if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, "agents", "single-load")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			agent := "name: single-load\n"
			if mode == "implicit" {
				agent += "mcp: [{name: selected}]\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "agent.yaml"), []byte(agent), 0600); err != nil {
				t.Fatal(err)
			}
			opts := Options{Agent: "single-load", Text: true, Porcelain: true, MaxTurns: 5}
			if mode == "explicit" || mode == "explicit-resume" {
				opts.MCP, opts.Changed.MCP = "selected", true
			}
			var store session.Store
			if mode == "resume" || mode == "explicit-resume" || mode == "failed-resume" {
				var err error
				store, err = session.NewSQLiteStore(session.Config{Enabled: true, Path: db})
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				sess := &session.Session{ID: session.NewID(), Agent: "single-load", MCP: "selected", Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: session.ModeAsk}
				if err := store.Create(context.Background(), sess); err != nil {
					t.Fatal(err)
				}
				opts.Resume, opts.Changed.Resume, opts.Agent = sess.ID, true, ""
			}
			connections := filepath.Join(root, "mcp.json")
			if mode == "failed-resume" {
				if err := os.WriteFile(connections, []byte("{temporary"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var output bytes.Buffer
			deps := testDependencies(&output, &output)
			deps.SessionStore.DBPath = db
			var loads atomic.Int32
			var loadedManager atomic.Pointer[mcp.Manager]
			deps.LoadMCPConfig = func(manager *mcp.Manager) error {
				loads.Add(1)
				loadedManager.Store(manager)
				if err := manager.LoadConfig(); err != nil {
					return err
				}
				return os.Remove(connections) // Startup cannot secretly load again.
			}
			provider := &mcpLoadCheckingProvider{MockProvider: mockprovider.NewMockProvider("mock").AddTextResponse("done"), check: func() error {
				want := int32(1)
				if mode == "empty" {
					want = 0
				}
				if loads.Load() != want {
					return fmt.Errorf("MCP loads=%d want=%d", loads.Load(), want)
				}
				manager := loadedManager.Load()
				if mode != "empty" && mode != "failed-resume" && (manager == nil || len(manager.EnabledServers()) != 1) {
					return fmt.Errorf("startup did not reuse loaded manager")
				}
				return nil
			}}
			deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
			if err := Run(context.Background(), []string{"hello"}, opts, deps); err != nil {
				t.Fatalf("ask: %v\n%s", err, output.String())
			}
			want := int32(1)
			if mode == "empty" {
				want = 0
			}
			if loads.Load() != want || len(provider.RecordedRequests()) != 1 {
				t.Fatalf("loads=%d provider requests=%d", loads.Load(), len(provider.RecordedRequests()))
			}
			if mode == "failed-resume" {
				saved, err := store.Get(context.Background(), opts.Resume)
				if err != nil || saved.MCP != "selected" {
					t.Fatalf("failed load erased selection: %+v %v", saved, err)
				}
			}
		})
	}
}
