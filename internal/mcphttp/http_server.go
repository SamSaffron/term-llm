// Package mcphttp provides an HTTP-based MCP server for tool execution.
// This package is kept separate to avoid import cycles with internal/llm.
package mcphttp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

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
	debug     bool

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

// SetDebug enables debug logging for HTTP requests.
func (s *Server) SetDebug(debug bool) {
	s.debug = debug
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
			Stateless: true, // Stateless mode - each request is independent
		},
	)

	mux := http.NewServeMux()
	// Chain: logging -> auth -> mcp handler
	mux.Handle("/mcp", s.loggingMiddleware(s.authMiddleware(mcpHandler)))

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
			if s.debug {
				fmt.Fprintf(os.Stderr, "[mcp-http] %s Unauthorized request from %s (got auth: %q)\n",
					time.Now().Format("15:04:05.000"), r.RemoteAddr, authHeader[:min(20, len(authHeader))])
			}
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// loggingMiddleware logs HTTP requests when debug is enabled.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.debug {
			// Read and restore body for logging, limiting to 8KB to avoid memory issues
			const maxDebugBodySize = 8 * 1024
			var bodyPreview string
			if r.Body != nil {
				// Use LimitReader to cap the amount we read for debugging
				limitedReader := io.LimitReader(r.Body, maxDebugBodySize+1)
				bodyBytes, err := io.ReadAll(limitedReader)
				if err == nil {
					// Restore full body for actual request processing
					// Note: if body was larger than limit, we've only read part of it
					r.Body = io.NopCloser(io.MultiReader(
						bytes.NewReader(bodyBytes),
						r.Body, // remaining unread bytes
					))
					if len(bodyBytes) > maxDebugBodySize {
						bodyPreview = string(bodyBytes[:500]) + "... (truncated, body > 8KB)"
					} else if len(bodyBytes) > 500 {
						bodyPreview = string(bodyBytes[:500]) + "..."
					} else {
						bodyPreview = string(bodyBytes)
					}
				}
			}
			fmt.Fprintf(os.Stderr, "[mcp-http] %s %s %s from %s\n",
				time.Now().Format("15:04:05.000"), r.Method, r.URL.Path, r.RemoteAddr)
			if bodyPreview != "" {
				fmt.Fprintf(os.Stderr, "[mcp-http] Request body: %s\n", bodyPreview)
			}
		}

		// Wrap response writer to capture status
		wrapped := &responseLogger{ResponseWriter: w, status: 200}
		next.ServeHTTP(wrapped, r)

		if s.debug {
			fmt.Fprintf(os.Stderr, "[mcp-http] %s Response status: %d\n",
				time.Now().Format("15:04:05.000"), wrapped.status)
		}
	})
}

// responseLogger wraps http.ResponseWriter to capture the status code.
type responseLogger struct {
	http.ResponseWriter
	status int
}

func (r *responseLogger) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
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
