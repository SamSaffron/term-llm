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

// TestServerStopRespectsContextDeadline verifies that Stop returns within the
// caller-supplied context deadline even when an in-flight tool call's executor
// is blocked indefinitely.
//
// Regression test: ClaudeBinProvider.CleanupMCP previously passed
// context.Background() to Stop. When a tool call was mid-flight (e.g. a long
// shell command) and the parent stream had already been cancelled so no writer
// remained for the result channel, http.Server.Shutdown blocked forever waiting
// for the active handler — deadlocking process exit on SIGTERM during runit
// restarts.
func TestServerStopRespectsContextDeadline(t *testing.T) {
	executorEntered := make(chan struct{})
	executor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		close(executorEntered)
		<-ctx.Done()
		return "", ctx.Err()
	}

	server := NewServer(executor)
	tools := []ToolSpec{
		{
			Name:        "blocking_tool",
			Description: "A tool whose executor never returns until ctx fires",
			Schema:      map[string]interface{}{"type": "object"},
		},
	}

	url, token, err := server.Start(context.Background(), tools)
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Issue a tool/call that will land in the blocking executor and stay
	// active. The request body is the minimal MCP JSON-RPC payload that the
	// stateless StreamableHTTPHandler accepts without prior session setup.
	go func() {
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"blocking_tool","arguments":{}}}`
		req, _ := http.NewRequest("POST", url, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	// Wait until the executor is actually running so server.Shutdown will
	// see an active handler. Without this we'd race the request setup.
	select {
	case <-executorEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("executor never entered — request setup failed; cannot test Stop deadline")
	}

	stopCtx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- server.Stop(stopCtx)
	}()

	select {
	case <-stopDone:
		// Stop returned — graceful shutdown timed out and forced close, exactly
		// what the production fix relies on.
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return within 2s of a 200ms context deadline — server is wedged on active handler")
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

func TestStartOnAddress(t *testing.T) {
	executor := func(ctx context.Context, name string, args json.RawMessage) (string, error) {
		return "ok", nil
	}
	tools := []ToolSpec{
		{Name: "t", Description: "d", Schema: map[string]interface{}{"type": "object"}},
	}
	ctx := context.Background()

	t.Run("provided token is used", func(t *testing.T) {
		server := NewServer(executor)
		url, token, err := server.StartOnAddress("127.0.0.1", 0, "my-secret", tools)
		if err != nil {
			t.Fatalf("StartOnAddress failed: %v", err)
		}
		defer server.Stop(ctx)

		if token != "my-secret" {
			t.Errorf("expected provided token, got %q", token)
		}
		if !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.HasSuffix(url, "/mcp") {
			t.Errorf("unexpected URL: %s", url)
		}

		// Verify auth works with the provided token
		time.Sleep(10 * time.Millisecond)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Authorization", "Bearer my-secret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request failed: %v", err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized {
			t.Error("provided token should be accepted")
		}
	})

	t.Run("empty token generates one", func(t *testing.T) {
		server := NewServer(executor)
		_, token, err := server.StartOnAddress("127.0.0.1", 0, "", tools)
		if err != nil {
			t.Fatalf("StartOnAddress failed: %v", err)
		}
		defer server.Stop(ctx)

		if token == "" {
			t.Error("auto-generated token should not be empty")
		}
	})

	t.Run("wildcard host URL uses localhost", func(t *testing.T) {
		server := NewServer(executor)
		url, _, err := server.StartOnAddress("0.0.0.0", 0, "tok", tools)
		if err != nil {
			t.Fatalf("StartOnAddress failed: %v", err)
		}
		defer server.Stop(ctx)

		if strings.Contains(url, "0.0.0.0") {
			t.Errorf("URL should not contain wildcard bind address, got %s", url)
		}
		if !strings.HasPrefix(url, "http://127.0.0.1:") {
			t.Errorf("wildcard URL should use 127.0.0.1, got %s", url)
		}
	})

	t.Run("IPv6 localhost", func(t *testing.T) {
		server := NewServer(executor)
		url, _, err := server.StartOnAddress("::1", 0, "tok", tools)
		if err != nil {
			t.Fatalf("StartOnAddress failed: %v", err)
		}
		defer server.Stop(ctx)

		// IPv6 addresses in URLs must be bracketed
		if !strings.HasPrefix(url, "http://[::1]:") {
			t.Errorf("IPv6 URL should bracket the host, got %s", url)
		}
	})

	t.Run("IPv6 wildcard uses localhost", func(t *testing.T) {
		server := NewServer(executor)
		url, _, err := server.StartOnAddress("::", 0, "tok", tools)
		if err != nil {
			t.Fatalf("StartOnAddress failed: %v", err)
		}
		defer server.Stop(ctx)

		if strings.Contains(url, "::") {
			t.Errorf("URL should not contain :: wildcard, got %s", url)
		}
		if !strings.HasPrefix(url, "http://127.0.0.1:") {
			t.Errorf("IPv6 wildcard URL should use 127.0.0.1, got %s", url)
		}
	})

	t.Run("cannot start twice", func(t *testing.T) {
		server := NewServer(executor)
		_, _, err := server.StartOnAddress("127.0.0.1", 0, "tok", tools)
		if err != nil {
			t.Fatalf("first start failed: %v", err)
		}
		defer server.Stop(ctx)

		_, _, err = server.StartOnAddress("127.0.0.1", 0, "tok", tools)
		if err == nil {
			t.Error("second start should fail")
		}
	})
}

func TestDisplayHost(t *testing.T) {
	tests := []struct {
		input, expected string
	}{
		{"127.0.0.1", "127.0.0.1"},
		{"::1", "::1"},
		{"10.0.0.5", "10.0.0.5"},
		{"0.0.0.0", "127.0.0.1"},
		{"::", "127.0.0.1"},
		{"", "127.0.0.1"},
	}
	for _, tc := range tests {
		got := displayHost(tc.input)
		if got != tc.expected {
			t.Errorf("displayHost(%q) = %q, want %q", tc.input, got, tc.expected)
		}
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
