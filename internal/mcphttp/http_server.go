// Package mcphttp provides an HTTP-based MCP server for tool execution.
// This package is kept separate to avoid import cycles with internal/llm.
package mcphttp

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolExecutor is a function that executes a tool and returns the result.
type ToolExecutor func(ctx context.Context, name string, args json.RawMessage) (string, error)

// ToolSpec describes a tool to expose via MCP.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]interface{}
}

// Server runs an MCP server over HTTP with token-based authentication.
// It exposes tools to Claude CLI and executes them using the provided executor.
type Server struct {
	server    *http.Server
	listener  net.Listener
	authToken string
	executor  ToolExecutor

	mu      sync.Mutex
	running bool
}

// NewServer creates a new HTTP MCP server.
// The executor function is called to execute tool calls.
func NewServer(executor ToolExecutor) *Server {
	return &Server{
		executor: executor,
	}
}

// Start starts the HTTP server on a random available port.
// Returns the server URL and auth token for connecting.
func (s *Server) Start(ctx context.Context, tools []ToolSpec) (url, token string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.running {
		return "", "", fmt.Errorf("server already running")
	}

	if s.executor == nil {
		return "", "", fmt.Errorf("tool executor is required")
	}

	// Generate crypto-random auth token
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", "", fmt.Errorf("generate auth token: %w", err)
	}
	s.authToken = base64.URLEncoding.EncodeToString(tokenBytes)

	// Listen on localhost only with random port
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", "", fmt.Errorf("listen: %w", err)
	}
	s.listener = listener

	// Create MCP server
	mcpServer := mcp.NewServer(&mcp.Implementation{
		Name:    "term-llm",
		Version: "1.0.0",
	}, nil)

	// Register tools with actual execution
	for _, tool := range tools {
		toolName := tool.Name // capture for closure

		mcpServer.AddTool(&mcp.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Schema, // Pass map directly - SDK handles marshaling
		}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			// Execute the tool using the provided executor
			argsJSON, err := json.Marshal(req.Params.Arguments)
			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: fmt.Sprintf("Error marshaling arguments: %v", err)},
					},
					IsError: true,
				}, nil
			}

			result, err := s.executor(ctx, toolName, argsJSON)
			if err != nil {
				return &mcp.CallToolResult{
					Content: []mcp.Content{
						&mcp.TextContent{Text: fmt.Sprintf("Error executing tool: %v", err)},
					},
					IsError: true,
				}, nil
			}

			return &mcp.CallToolResult{
				Content: []mcp.Content{
					&mcp.TextContent{Text: result},
				},
			}, nil
		})
	}

	// Create HTTP handler with auth middleware
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{
			Stateless: true, // No session state needed
		},
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", s.authMiddleware(mcpHandler))

	s.server = &http.Server{Handler: mux}
	s.running = true

	// Use a channel to capture immediate startup errors
	startupErr := make(chan error, 1)

	// Start serving in background
	go func() {
		err := s.server.Serve(listener)
		// ErrServerClosed is expected on graceful shutdown
		if err != nil && err != http.ErrServerClosed {
			select {
			case startupErr <- err:
			default:
				// Channel full or already closed, log the error
				// This can happen if error occurs after Start() returns
			}
		}
	}()

	// Brief wait to catch immediate startup failures
	select {
	case err := <-startupErr:
		s.running = false
		s.listener = nil
		return "", "", fmt.Errorf("server failed to start: %w", err)
	default:
		// No immediate error, server is likely running
	}

	addr := listener.Addr().(*net.TCPAddr)
	serverURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", addr.Port)

	return serverURL, s.authToken, nil
}

// authMiddleware validates the Authorization header.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		expectedAuth := "Bearer " + s.authToken

		if authHeader != expectedAuth {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// Stop gracefully stops the HTTP server.
func (s *Server) Stop(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return nil
	}

	s.running = false

	if s.server != nil {
		if err := s.server.Shutdown(ctx); err != nil {
			// Force close if shutdown fails
			s.server.Close()
		}
	}

	// Clear sensitive data
	s.authToken = ""
	s.listener = nil

	return nil
}

// URL returns the server URL if running.
func (s *Server) URL() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running || s.listener == nil {
		return ""
	}

	addr := s.listener.Addr().(*net.TCPAddr)
	return fmt.Sprintf("http://127.0.0.1:%d/mcp", addr.Port)
}

// Token returns the auth token if running.
func (s *Server) Token() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return ""
	}
	return s.authToken
}

// ParseMCPToolName extracts the original tool name from an MCP-namespaced name.
// MCP tools from term-llm are namespaced as "mcp__term-llm__<tool>".
func ParseMCPToolName(mcpName string) string {
	prefix := "mcp__term-llm__"
	if strings.HasPrefix(mcpName, prefix) {
		return strings.TrimPrefix(mcpName, prefix)
	}
	return mcpName
}
