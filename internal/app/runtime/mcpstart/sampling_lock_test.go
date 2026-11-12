package mcpstart

import (
	"context"
	"io"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm/engine"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
)

// Locked serve runs pass NoSampling; ordinary runs keep the sampling handler.
func TestEnableLoadedServersNoSamplingInstallsNoHandler(t *testing.T) {
	for _, noSampling := range []bool{true, false} {
		manager := mcp.NewManagerWithConfig(&mcp.Config{Servers: map[string]mcp.ServerConfig{}})
		// The handler is decided before any server starts; the empty
		// selection's startup error is irrelevant here.
		_, _ = enableLoadedServers(context.Background(), manager, "", engine.NewEngine(mockprovider.NewMockProvider("debug"), nil), io.Discard, &MCPOptions{
			Provider: mockprovider.NewMockProvider("debug"), Model: "allowed", NoSampling: noSampling,
		})
		if got := manager.GetSamplingHandler() != nil; got == noSampling {
			t.Fatalf("NoSampling=%v installed handler=%v", noSampling, got)
		}
	}
}
