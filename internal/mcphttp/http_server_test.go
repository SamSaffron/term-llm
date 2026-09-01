package mcphttp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestServerStartStop(t *testing.T) {
	executor := func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: "executed: " + name}, nil
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

func TestServerPropagatesToolResultIsError(t *testing.T) {
	server := NewServer(func(context.Context, string, json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: "failed usefully", IsError: true}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url, token, err := server.Start(ctx, []ToolSpec{{Name: "fail", Schema: map[string]interface{}{"type": "object"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := server.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"fail","arguments":{}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(payload), []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			payload = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
			break
		}
	}
	var result struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response %q: %v", payload, err)
	}
	if !result.Result.IsError || len(result.Result.Content) != 1 || result.Result.Content[0].Text != "failed usefully" {
		t.Fatalf("tool result = %#v", result.Result)
	}
}

func TestServerEmitsImageContentParts(t *testing.T) {
	server := NewServer(func(context.Context, string, json.RawMessage) (ToolResult, error) {
		return ToolResult{
			Content: "Image loaded",
			Parts: []ContentPart{
				{Type: ContentPartText, Text: "Image loaded"},
				{Type: ContentPartImage, MIMEType: "image/png", Data: []byte("hello")},
			},
		}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url, token, err := server.Start(ctx, []ToolSpec{{Name: "view_image", Schema: map[string]interface{}{"type": "object"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
		defer stopCancel()
		if err := server.Stop(stopCtx); err != nil {
			t.Errorf("Stop: %v", err)
		}
	}()

	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"view_image","arguments":{}}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	for _, line := range bytes.Split(bytes.TrimSpace(payload), []byte("\n")) {
		if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) {
			payload = bytes.TrimSpace(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("data:")))
			break
		}
	}
	var result struct {
		Result struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				MIMEType string `json:"mimeType"`
				Data     string `json:"data"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatalf("decode response %q: %v", payload, err)
	}
	if len(result.Result.Content) != 2 {
		t.Fatalf("content = %#v, want two blocks", result.Result.Content)
	}
	if got := result.Result.Content[0]; got.Type != "text" || got.Text != "Image loaded" {
		t.Errorf("text block = %#v", got)
	}
	image := result.Result.Content[1]
	if image.Type != "image" || image.MIMEType != "image/png" {
		t.Fatalf("image block = %#v", image)
	}
	decoded, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil {
		t.Fatalf("decode image data: %v", err)
	}
	if !bytes.Equal(decoded, []byte("hello")) {
		t.Errorf("image data = %q, want %q", decoded, "hello")
	}
}

func TestToolResultContentFallbacks(t *testing.T) {
	tests := []struct {
		name   string
		result ToolResult
	}{
		{name: "plain text", result: ToolResult{Content: "plain"}},
		{name: "invalid parts", result: ToolResult{Content: "fallback", Parts: []ContentPart{{Type: ContentPartImage, MIMEType: "image/png"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content := toolResultContent(tt.result)
			if len(content) != 1 {
				t.Fatalf("content = %#v, want one text block", content)
			}
			text, ok := content[0].(*mcp.TextContent)
			if !ok || text.Text != tt.result.Content {
				t.Fatalf("content = %#v, want text %q", content, tt.result.Content)
			}
		})
	}
}

func TestServerAuthMiddleware(t *testing.T) {
	executor := func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: "executed"}, nil
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
	executor := func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
		close(executorEntered)
		<-ctx.Done()
		return ToolResult{}, ctx.Err()
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
	executor := func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: "executed"}, nil
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
	executor := func(ctx context.Context, name string, args json.RawMessage) (ToolResult, error) {
		return ToolResult{Content: "ok"}, nil
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
