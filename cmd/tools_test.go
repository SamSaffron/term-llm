package cmd

import (
	"reflect"
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
