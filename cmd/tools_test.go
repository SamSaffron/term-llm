package cmd

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
)

func TestDefaultToolRegistryExaMCPSearchKeepsDefaultJinaReader(t *testing.T) {
	cfg := &config.Config{}
	cfg.Search.Provider = "exa_mcp"
	cfg.Search.FetchProvider = "jina"

	registry := defaultToolRegistry(cfg)
	tool, ok := registry.Get(llm.ReadURLToolName)
	if !ok {
		t.Fatalf("read_url tool not registered")
	}
	readURLTool, ok := tool.(*llm.ReadURLTool)
	if !ok {
		t.Fatalf("read_url tool is %T, want *llm.ReadURLTool", tool)
	}
	if readURLToolHasFetcher(readURLTool) {
		t.Fatalf("read_url unexpectedly has a custom fetcher; want default Jina reader")
	}
}

func TestDefaultToolRegistryCanUseExaMCPForFetch(t *testing.T) {
	cfg := &config.Config{}
	cfg.Search.Provider = "duckduckgo"
	cfg.Search.FetchProvider = "exa_mcp"

	registry := defaultToolRegistry(cfg)
	tool, ok := registry.Get(llm.ReadURLToolName)
	if !ok {
		t.Fatalf("read_url tool not registered")
	}
	readURLTool, ok := tool.(*llm.ReadURLTool)
	if !ok {
		t.Fatalf("read_url tool is %T, want *llm.ReadURLTool", tool)
	}
	if !readURLToolHasFetcher(readURLTool) {
		t.Fatalf("read_url does not have a custom fetcher; want Exa MCP fetcher")
	}
}

func TestDefaultToolRegistryCanDisableFetch(t *testing.T) {
	cfg := &config.Config{}
	cfg.Search.Provider = "duckduckgo"
	cfg.Search.FetchProvider = "none"

	registry := defaultToolRegistry(cfg)
	if _, ok := registry.Get(llm.ReadURLToolName); ok {
		t.Fatalf("read_url tool registered with fetch_provider none")
	}
}

func readURLToolHasFetcher(tool *llm.ReadURLTool) bool {
	v := reflect.ValueOf(tool).Elem().FieldByName("fetcher")
	return !v.IsNil()
}

func TestDefaultToolRegistrySearchFallbackOnlyForAbsentCredentials(t *testing.T) {
	t.Setenv("BRAVE_API_KEY", "")
	cfg := &config.Config{}
	cfg.Search.Provider = "brave"
	registry := defaultToolRegistry(cfg)
	tool, ok := registry.Get("web_search")
	if !ok {
		t.Fatal("search tool absent")
	}
	// Match the existing reader wiring tests without issuing a real search.
	searcher := reflect.ValueOf(tool).Elem().FieldByName("searcher")
	if got := searcher.Elem().Type().String(); got != "*search.DuckDuckGoLite" {
		t.Fatalf("unconfigured searcher = %s", got)
	}
	cfg.Search.Brave.APIKey = "file://" + filepath.Join(t.TempDir(), "missing")
	registry = defaultToolRegistry(cfg)
	tool, _ = registry.Get("web_search")
	if _, err := tool.Execute(context.Background(), json.RawMessage(`{"query":""}`)); err == nil || !strings.Contains(err.Error(), "search.brave.api_key") {
		t.Fatalf("configured credential failure must not fall back: %v", err)
	}
}
