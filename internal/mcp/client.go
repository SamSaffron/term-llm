package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ToolSpec describes a tool available from an MCP server.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
}

// Client wraps an MCP server connection.
type Client struct {
	name            string
	config          ServerConfig
	client          *mcp.Client
	session         *mcp.ClientSession
	tools           []ToolSpec
	samplingHandler *SamplingHandler
	mu              sync.RWMutex
	running         bool
}

// NewClient creates a new MCP client for the given server configuration.
func NewClient(name string, config ServerConfig) *Client {
	return &Client{
		name:   name,
		config: config,
	}
}

// SetSamplingHandler sets the sampling handler for this client.
func (c *Client) SetSamplingHandler(handler *SamplingHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samplingHandler = handler
}

// Name returns the server name.
func (c *Client) Name() string {
	return c.name
}

// Start connects to the MCP server and initializes the session.
func (c *Client) Start(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.running {
		return nil
	}

	// Build client options with sampling handler if available
	var clientOpts *mcp.ClientOptions
	if c.samplingHandler != nil {
		clientName := c.name
		clientOpts = &mcp.ClientOptions{
			CreateMessageHandler: func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
				return c.samplingHandler.Handle(ctx, clientName, req)
			},
		}
	}

	// Create the MCP client with options
	c.client = mcp.NewClient(&mcp.Implementation{
		Name:    "term-llm",
		Version: "1.0.0",
	}, clientOpts)

	// Create transport based on config type
	var transport mcp.Transport
	if c.config.TransportType() == "http" {
		transport = c.createHTTPTransport()
	} else {
		transport = c.createStdioTransport(ctx)
	}

	session, err := c.client.Connect(ctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect to MCP server %s: %w", c.name, err)
	}
	c.session = session

	// Fetch available tools
	if err := c.refreshTools(ctx); err != nil {
		c.session.Close()
		c.session = nil
		return fmt.Errorf("list tools from %s: %w", c.name, err)
	}

	c.running = true
	return nil
}

// createStdioTransport creates a stdio transport for command-based servers.
func (c *Client) createStdioTransport(ctx context.Context) mcp.Transport {
	cmd := exec.CommandContext(ctx, c.config.Command, c.config.Args...)
	if len(c.config.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range c.config.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return &mcp.CommandTransport{Command: cmd}
}

// createHTTPTransport creates an HTTP transport for URL-based servers.
func (c *Client) createHTTPTransport() mcp.Transport {
	// Create HTTP client with custom headers if specified
	httpClient := &http.Client{Timeout: 30 * time.Second}

	// Use StreamableClientTransport (the modern MCP transport)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   c.config.URL,
		HTTPClient: httpClient,
		MaxRetries: 5,
	}

	// If headers are specified, wrap the transport with a custom round tripper
	if len(c.config.Headers) > 0 {
		httpClient.Transport = &headerTransport{
			base:    http.DefaultTransport,
			headers: c.config.Headers,
		}
	}

	return transport
}

// headerTransport is an http.RoundTripper that adds custom headers to requests.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// Stop closes the MCP server connection.
func (c *Client) Stop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.running {
		return nil
	}

	var err error
	if c.session != nil {
		err = c.session.Close()
		c.session = nil
	}
	c.running = false
	c.tools = nil
	return err
}

// IsRunning returns whether the client is connected.
func (c *Client) IsRunning() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.running
}

// Tools returns the available tools from this server.
func (c *Client) Tools() []ToolSpec {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.tools
}

// refreshTools fetches the tool list from the server.
func (c *Client) refreshTools(ctx context.Context) error {
	result, err := c.session.ListTools(ctx, nil)
	if err != nil {
		return err
	}

	c.tools = make([]ToolSpec, 0, len(result.Tools))
	for _, t := range result.Tools {
		schema := make(map[string]any)
		if t.InputSchema != nil {
			if m, ok := t.InputSchema.(map[string]any); ok {
				schema = m
			}
		}
		c.tools = append(c.tools, ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			Schema:      schema,
		})
	}
	return nil
}

// CallTool invokes a tool on the MCP server.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	c.mu.RLock()
	session := c.session
	running := c.running
	c.mu.RUnlock()

	if !running || session == nil {
		return "", fmt.Errorf("MCP server %s is not running", c.name)
	}

	// Parse arguments
	var arguments map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return "", fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		return "", fmt.Errorf("call tool %s: %w", name, err)
	}

	if result.IsError {
		return "", fmt.Errorf("tool %s returned error: %s", name, formatContent(result.Content))
	}

	return formatContent(result.Content), nil
}

// formatContent converts MCP content to a string.
func formatContent(content []mcp.Content) string {
	var sb strings.Builder
	for _, c := range content {
		switch v := c.(type) {
		case *mcp.TextContent:
			sb.WriteString(v.Text)
		default:
			// For other content types, try JSON encoding
			if data, err := json.Marshal(c); err == nil {
				sb.WriteString(string(data))
			}
		}
	}
	return sb.String()
}
