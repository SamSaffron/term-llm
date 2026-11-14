package search

import (
	"testing"

	"github.com/term-llm/term-llm/internal/config"
)

func TestSearchDefaultsMatchConfig(t *testing.T) {
	if defaultExaMCPURL != config.DefaultSearchExaMCPURL {
		t.Fatalf("defaultExaMCPURL = %q, want %q", defaultExaMCPURL, config.DefaultSearchExaMCPURL)
	}
	client := NewExaMCPClient("", "")
	if client.url != config.DefaultSearchExaMCPURL {
		t.Fatalf("Exa MCP client URL = %q, want %q", client.url, config.DefaultSearchExaMCPURL)
	}
}
