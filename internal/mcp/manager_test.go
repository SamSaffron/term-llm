package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	mcpSDK "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
)

const runMCPManagerTestServerEnv = "TERM_LLM_MCP_MANAGER_TEST_SERVER"

type managerTestGreetingParams struct {
	Name string `json:"name"`
}

func TestMain(m *testing.M) {
	if os.Getenv(runMCPManagerTestServerEnv) != "" {
		runMCPManagerTestServer()
		return
	}
	os.Exit(m.Run())
}

func runMCPManagerTestServer() {
	server := mcpSDK.NewServer(&mcpSDK.Implementation{Name: "manager-test", Version: "v0.0.1"}, nil)
	mcpSDK.AddTool(server, &mcpSDK.Tool{Name: "greet", Description: "say hi"}, func(ctx context.Context, req *mcpSDK.CallToolRequest, args managerTestGreetingParams) (*mcpSDK.CallToolResult, any, error) {
		return &mcpSDK.CallToolResult{
			Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "hi " + args.Name}},
		}, nil, nil
	})
	mcpSDK.AddTool(server, &mcpSDK.Tool{Name: "mixed", Description: "return text and an image"}, func(ctx context.Context, req *mcpSDK.CallToolRequest, args struct{}) (*mcpSDK.CallToolResult, any, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{
			&mcpSDK.TextContent{Text: "before"},
			&mcpSDK.ImageContent{MIMEType: "image/png", Data: []byte("image bytes")},
			&mcpSDK.TextContent{Text: "after"},
		}}, nil, nil
	})
	mcpSDK.AddTool(server, &mcpSDK.Tool{Name: "image", Description: "return an image"}, func(ctx context.Context, req *mcpSDK.CallToolRequest, args struct{}) (*mcpSDK.CallToolResult, any, error) {
		return &mcpSDK.CallToolResult{Content: []mcpSDK.Content{
			&mcpSDK.ImageContent{MIMEType: "image/png", Data: []byte("image only")},
		}}, nil, nil
	})
	mcpSDK.AddTool(server, &mcpSDK.Tool{Name: "failure", Description: "return a tool error"}, func(ctx context.Context, req *mcpSDK.CallToolRequest, args struct{}) (*mcpSDK.CallToolResult, any, error) {
		return &mcpSDK.CallToolResult{
			Content: []mcpSDK.Content{&mcpSDK.TextContent{Text: "tool failed"}},
			IsError: true,
		}, nil, nil
	})
	if err := server.Run(context.Background(), &mcpSDK.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}

func TestManagerEnable_TimesOutStartupWithBackgroundContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}

	oldTimeout := mcpStartupTimeout
	mcpStartupTimeout = 100 * time.Millisecond
	defer func() { mcpStartupTimeout = oldTimeout }()

	manager := NewManager()
	manager.config = &Config{Servers: map[string]ServerConfig{
		"sleepy": {
			Command: "sh",
			Args:    []string{"-c", "sleep 10"},
		},
	}}
	defer manager.StopAll()

	if err := manager.Enable(context.Background(), "sleepy"); err != nil {
		t.Fatalf("Enable returned error: %v", err)
	}

	status, err := waitForServerStatus(t, manager, "sleepy", StatusFailed, 3*time.Second)
	if status != StatusFailed {
		t.Fatalf("status = %s, want %s", status, StatusFailed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status error = %v, want wrapped context deadline exceeded", err)
	}
}

func TestManagerEnable_ReadyStdioServerSurvivesStartupTimeoutContext(t *testing.T) {
	oldTimeout := mcpStartupTimeout
	mcpStartupTimeout = 250 * time.Millisecond
	defer func() { mcpStartupTimeout = oldTimeout }()

	manager := NewManager()
	manager.config = &Config{Servers: map[string]ServerConfig{
		"greeter": {
			Command: os.Args[0],
			Env: map[string]string{
				runMCPManagerTestServerEnv: "1",
			},
		},
	}}
	defer manager.StopAll()

	if err := manager.Enable(context.Background(), "greeter"); err != nil {
		t.Fatalf("Enable returned error: %v", err)
	}

	status, err := waitForServerStatus(t, manager, "greeter", StatusReady, 3*time.Second)
	if status != StatusReady {
		t.Fatalf("status = %s, want %s", status, StatusReady)
	}
	if err != nil {
		t.Fatalf("status error = %v, want nil", err)
	}

	// Wait long enough that the short startup context has been canceled. The MCP
	// subprocess should continue to live on the manager-owned lifecycle context.
	time.Sleep(mcpStartupTimeout + 100*time.Millisecond)

	args, err := json.Marshal(map[string]string{"name": "Ada"})
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}
	got, err := manager.CallTool(context.Background(), "greeter__greet", args)
	if err != nil {
		t.Fatalf("CallTool after startup timeout elapsed: %v", err)
	}
	if !strings.Contains(got.Content, "hi Ada") {
		t.Fatalf("CallTool result = %q, want greeting", got.Content)
	}
	if len(got.ContentParts) != 1 || got.ContentParts[0].Type != llm.ToolContentPartText || got.ContentParts[0].Text != "hi Ada" {
		t.Fatalf("text-only ContentParts = %#v, want one text part", got.ContentParts)
	}

	mixedTool := NewMCPTool(manager, ToolSpec{Name: "greeter__mixed"})
	mixed, err := mixedTool.Execute(context.Background(), nil)
	if err != nil {
		t.Fatalf("execute mixed MCP tool: %v", err)
	}
	if mixed.Content != "beforeafter" {
		t.Fatalf("mixed Content = %q, want %q", mixed.Content, "beforeafter")
	}
	if len(mixed.ContentParts) != 3 ||
		mixed.ContentParts[0].Type != llm.ToolContentPartText || mixed.ContentParts[0].Text != "before" ||
		mixed.ContentParts[1].Type != llm.ToolContentPartImageData || mixed.ContentParts[1].ImageData == nil ||
		mixed.ContentParts[1].ImageData.MediaType != "image/png" ||
		mixed.ContentParts[1].ImageData.Base64 != base64.StdEncoding.EncodeToString([]byte("image bytes")) ||
		mixed.ContentParts[2].Type != llm.ToolContentPartText || mixed.ContentParts[2].Text != "after" {
		t.Fatalf("mixed ContentParts = %#v, want ordered text/image/text", mixed.ContentParts)
	}

	imageOnly, err := manager.CallTool(context.Background(), "greeter__image", nil)
	if err != nil {
		t.Fatalf("call image-only MCP tool: %v", err)
	}
	if imageOnly.Content != "" || len(imageOnly.ContentParts) != 1 ||
		imageOnly.ContentParts[0].Type != llm.ToolContentPartImageData || imageOnly.ContentParts[0].ImageData == nil {
		t.Fatalf("image-only result = %#v, want one structured image", imageOnly)
	}

	failure, err := manager.CallTool(context.Background(), "greeter__failure", nil)
	if err != nil {
		t.Fatalf("MCP IsError result should remain a tool output, got error: %v", err)
	}
	if !failure.IsError || failure.Content != "tool failed" || len(failure.ContentParts) != 1 {
		t.Fatalf("failure result = %#v, want preserved content with IsError", failure)
	}

	_, err = manager.CallTool(context.Background(), "greeter__greet", json.RawMessage("{"))
	if err == nil || !strings.Contains(err.Error(), "invalid tool arguments") {
		t.Fatalf("malformed arguments error = %v, want invalid tool arguments", err)
	}
}

func TestManagerDisable_CancelsInFlightStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires sh")
	}

	oldTimeout := mcpStartupTimeout
	mcpStartupTimeout = 5 * time.Second
	defer func() { mcpStartupTimeout = oldTimeout }()

	manager := NewManager()
	manager.config = &Config{Servers: map[string]ServerConfig{
		"sleepy": {
			Command: "sh",
			Args:    []string{"-c", "sleep 10"},
		},
	}}
	defer manager.StopAll()

	enableCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := manager.Enable(enableCtx, "sleepy"); err != nil {
		t.Fatalf("Enable returned error: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- manager.Disable("sleepy")
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Disable returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		err := <-done
		t.Fatalf("Disable blocked waiting for startup to finish: %v", err)
	}

	status, err := manager.ServerStatus("sleepy")
	if status != StatusStopped {
		t.Fatalf("status immediately after Disable = %s, want %s", status, StatusStopped)
	}
	if err != nil {
		t.Fatalf("status error immediately after Disable = %v, want nil", err)
	}

	time.Sleep(50 * time.Millisecond)

	status, err = manager.ServerStatus("sleepy")
	if status != StatusStopped {
		t.Fatalf("status after canceled startup settled = %s, want %s", status, StatusStopped)
	}
	if err != nil {
		t.Fatalf("status error after canceled startup settled = %v, want nil", err)
	}
}

func waitForServerStatus(t *testing.T, manager *Manager, name string, want ServerStatus, timeout time.Duration) (ServerStatus, error) {
	t.Helper()

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		status, err := manager.ServerStatus(name)
		if status == want {
			return status, err
		}
		time.Sleep(10 * time.Millisecond)
	}

	status, err := manager.ServerStatus(name)
	t.Fatalf("timed out waiting for status %s; last status=%s err=%v", want, status, err)
	return status, err
}
