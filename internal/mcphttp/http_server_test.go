package mcphttp

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestServerStartStop(t *testing.T) {
	executor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "executed: " + name, nil
	}

	server := NewServer(executor)

	tools := []ToolSpec{
		{
			Name:        "test_tool",
			Description: "A test tool",
			Schema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"input": map[string]interface{}{
						"type": "string",
					},
				},
			},
		},
	}

	ctx := context.Background()
	url, token, err := server.Start(ctx, tools)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}

	// Verify URL format
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("URL should start with http://127.0.0.1:, got %s", url)
	}
	if !strings.HasSuffix(url, "/mcp") {
		t.Errorf("URL should end with /mcp, got %s", url)
	}

	// Verify token is non-empty
	if token == "" {
		t.Error("Token should not be empty")
	}

	// Verify URL() and Token() methods
	if server.URL() != url {
		t.Errorf("URL() mismatch: got %s, want %s", server.URL(), url)
	}
	if server.Token() != token {
		t.Errorf("Token() mismatch: got %s, want %s", server.Token(), token)
	}

	// Stop the server
	if err := server.Stop(ctx); err != nil {
		t.Fatalf("Failed to stop server: %v", err)
	}

	// Verify server is stopped
	if server.URL() != "" {
		t.Error("URL() should be empty after stop")
	}
	if server.Token() != "" {
		t.Error("Token() should be empty after stop")
	}
}

func TestServerAuthMiddleware(t *testing.T) {
	executor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "executed", nil
	}

	server := NewServer(executor)
	tools := []ToolSpec{
		{
			Name:        "test_tool",
			Description: "A test tool",
			Schema:      map[string]interface{}{"type": "object"},
		},
	}

	ctx := context.Background()
	url, token, err := server.Start(ctx, tools)
	if err != nil {
		t.Fatalf("Failed to start server: %v", err)
	}
	defer server.Stop(ctx)

	// Wait a bit for server to be ready
	time.Sleep(10 * time.Millisecond)

	// Test with no auth - should fail
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Expected 401 without auth, got %d", resp.StatusCode)
	}

	// Test with wrong token - should fail
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Expected 401 with wrong token, got %d", resp.StatusCode)
	}

	// Test with correct token - should succeed (at least not 401)
	req, _ = http.NewRequest("GET", url, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Request failed: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("Expected non-401 with correct token")
	}
}

func TestServerCannotStartTwice(t *testing.T) {
	executor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "executed", nil
	}

	server := NewServer(executor)
	tools := []ToolSpec{}

	ctx := context.Background()
	_, _, err := server.Start(ctx, tools)
	if err != nil {
		t.Fatalf("First start failed: %v", err)
	}
	defer server.Stop(ctx)

	// Second start should fail
	_, _, err = server.Start(ctx, tools)
	if err == nil {
		t.Error("Second start should fail")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("Error should mention 'already running', got: %v", err)
	}
}

func TestParseMCPToolName(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"mcp__term-llm__read_file", "read_file"},
		{"mcp__term-llm__shell", "shell"},
		{"mcp__other__tool", "mcp__other__tool"}, // Different server prefix
		{"regular_tool", "regular_tool"},
		{"", ""},
	}

	for _, tc := range tests {
		result := ParseMCPToolName(tc.input)
		if result != tc.expected {
			t.Errorf("ParseMCPToolName(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}
