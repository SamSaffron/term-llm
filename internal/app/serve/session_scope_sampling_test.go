package serve

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	termmcp "github.com/samsaffron/term-llm/internal/mcp"
	runpkg "github.com/samsaffron/term-llm/internal/run"
	"github.com/samsaffron/term-llm/internal/session"
)

// MCP sampling sends server-chosen prompts to the runtime provider, optionally
// with a configured Sampling.Model override, outside per-turn admission. A
// locked runtime installs no sampling handler at all, so neither the default
// nor the override model can be reached; an ordinary runtime is unchanged.
func TestLockedRuntimeInstallsNoMCPSamplingHandler(t *testing.T) {
	f := newScopeFixture(t)
	ctx := context.Background()
	id := session.NewID()
	if err := f.run(t, id, f.private, f.privateProvider, "invented private marker"); err != nil {
		t.Fatalf("locked first turn: %v", err)
	}
	// Resume under a definition that no longer opts into lock_session: the
	// persisted lock still marks the runtime locked.
	widened := "name: private\nprovider: debug\nmodel: allowed\nallowed_models: [debug:*, other:*]\n"
	if err := os.WriteFile(filepath.Join(f.private, "agent.yaml"), []byte(widened), 0o600); err != nil {
		t.Fatal(err)
	}
	includeTools := false
	resumed, err := newCmdRunner(f.cfg, cmdRunnerOptions{Store: f.store}).(*cmdRunner).prepare(ctx, runpkg.Request{
		Platform: runpkg.PlatformConsole, SessionID: id, Persist: true, Stateful: true, Resume: true,
		Messages: []core.Message{core.UserText("again")}, IncludeConfiguredTools: &includeTools, ProviderInstance: mockprovider.NewMockProvider("debug"),
	}, eventSinkFunc(func(core.Event) {}))
	if err != nil {
		t.Fatalf("prepare resumed locked session: %v", err)
	}
	defer resumed.Close()
	if !resumed.runtime.scopeLocked {
		t.Fatal("resumed persisted-lock runtime is not marked locked")
	}

	locked := mockprovider.NewMockProvider("debug")
	plain := mockprovider.NewMockProvider("debug").AddTextResponse("sampled")
	for name, tc := range map[string]struct {
		rt       *serveRuntime
		provider *mockprovider.MockProvider
	}{
		"initially locked":  {&serveRuntime{provider: locked, defaultModel: "allowed", scopeLocked: true}, locked},
		"resumed persisted": {resumed.runtime, nil},
	} {
		if tc.rt.mcpManager != nil {
			tc.rt.setMCPManager(nil)
		}
		if err := tc.rt.ensureMCPManagerLocked(); err != nil {
			t.Fatalf("%s: ensure MCP manager: %v", name, err)
		}
		if h := tc.rt.mcpManager.GetSamplingHandler(); h != nil {
			t.Fatalf("%s: locked runtime installed an MCP sampling handler", name)
		}
		if tc.provider != nil && len(tc.provider.RecordedRequests()) != 0 {
			t.Fatalf("%s: locked runtime dispatched sampling", name)
		}
	}

	// Ordinary runtimes keep sampling, including the configured model override.
	ordinary := &serveRuntime{provider: plain, defaultModel: "allowed"}
	if err := ordinary.ensureMCPManagerLocked(); err != nil {
		t.Fatal(err)
	}
	handler := ordinary.mcpManager.GetSamplingHandler()
	if handler == nil {
		t.Fatal("ordinary runtime lost its MCP sampling handler")
	}
	enabled := true
	handler.SetServerConfig("srv", termmcp.ServerConfig{Sampling: &termmcp.SamplingConfig{Enabled: &enabled, AutoApprove: true, Model: "override"}})
	if _, err := handler.Handle(ctx, "srv", &mcp.CreateMessageRequest{Params: &mcp.CreateMessageParams{
		Messages: []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "hi"}}}, MaxTokens: 10,
	}}); err != nil {
		t.Fatalf("ordinary sampling: %v", err)
	}
	if got := plain.RecordedRequests(); len(got) != 1 || got[0].Model != "override" {
		t.Fatalf("ordinary sampling requests = %+v, want one override request", got)
	}
}
