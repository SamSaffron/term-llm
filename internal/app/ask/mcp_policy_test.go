package ask

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm/core"
	"github.com/samsaffron/term-llm/internal/llm/mockprovider"
	"github.com/samsaffron/term-llm/internal/mcp"
	"github.com/samsaffron/term-llm/internal/testutil/mcptest"
)

func TestMCPPolicyAskSelectionPreflight(t *testing.T) {
	var requests atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer endpoint.Close()
	mcptest.WriteConfig(t, map[string]mcp.ServerConfig{"open": {URL: endpoint.URL}, "restricted": {URL: endpoint.URL}})
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "term-llm", "config.yaml")
	body := "default_provider: mock\nproviders:\n  mock:\n    model: mock-model\nsessions:\n  enabled: false\nmcp:\n  servers:\n    restricted:\n      agents: [migration-data]\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	provider := mockprovider.NewMockProvider("mock")
	var output bytes.Buffer
	deps := testDependencies(&output, &output)
	deps.NewProvider = func(*config.Config, bool) (core.Provider, error) { return provider, nil }
	err := Run(context.Background(), []string{"hello"}, Options{Changed: FlagState{MCP: true}, MCP: "open,restricted", Text: true, Porcelain: true, MaxTurns: 5}, deps)
	var denied *mcp.PermissionError
	if !errors.As(err, &denied) {
		t.Fatalf("ask error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("ask started an MCP server before validating the entire selection")
	}
	if len(provider.RecordedRequests()) != 0 {
		t.Fatal("ask sent a provider request for a denied selection")
	}
}
