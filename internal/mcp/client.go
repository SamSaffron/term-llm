package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/procutil"
)

var mcpCommandWaitDelay = time.Second

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
	processCancel   context.CancelFunc
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
	return c.start(ctx, ctx)
}

func (c *Client) start(ctx, processCtx context.Context) error {
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
		transport = c.createStdioTransport(processCtx)
	}

	session, err := c.client.Connect(ctx, transport, nil)
	if err != nil {
		c.cancelStdioProcessLocked()
		return fmt.Errorf("connect to MCP server %s: %w", c.name, err)
	}
	c.session = session

	// Fetch available tools
	if err := c.refreshTools(ctx); err != nil {
		c.cancelStdioProcessLocked()
		c.session.Close()
		c.session = nil
		return fmt.Errorf("list tools from %s: %w", c.name, err)
	}

	c.running = true
	return nil
}

// createStdioTransport creates a stdio transport for command-based servers.
func (c *Client) createStdioTransport(ctx context.Context) mcp.Transport {
	processCtx, processCancel := context.WithCancel(ctx)
	c.processCancel = processCancel

	cmd := exec.CommandContext(processCtx, c.config.Command, c.config.Args...)
	cmd.WaitDelay = mcpCommandWaitDelay
	procutil.ConfigureCommandProcessGroup(cmd)
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
	// Use a clone of the default transport so proxy, HTTP/2, and other standard
	// settings are preserved while avoiding a whole-request http.Client timeout.
	// Caller contexts control the full request lifetime, including long-running
	// tool calls and streams.
	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.DialContext = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	baseTransport.TLSHandshakeTimeout = 15 * time.Second
	baseTransport.ResponseHeaderTimeout = 2 * time.Minute
	baseTransport.IdleConnTimeout = 90 * time.Second

	httpClient := &http.Client{Transport: baseTransport}

	// If headers are specified, wrap the transport with a custom round tripper.
	if len(c.config.Headers) > 0 {
		httpClient.Transport = &headerTransport{
			base:    baseTransport,
			headers: c.config.Headers,
		}
	}

	// Use StreamableClientTransport (the modern MCP transport)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   c.config.URL,
		HTTPClient: httpClient,
		MaxRetries: 5,
	}

	return transport
}

// headerTransport is an http.RoundTripper that adds custom headers to requests.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		req.Header.Set(k, v)
	}
	return t.base.RoundTrip(req)
}

// Stop closes the MCP server connection.
func (c *Client) Stop() error {
	c.mu.Lock()
	if !c.running {
		c.mu.Unlock()
		return nil
	}

	session := c.session
	processCancel := c.processCancel
	c.session = nil
	c.processCancel = nil
	c.running = false
	c.tools = nil
	c.mu.Unlock()

	if processCancel != nil {
		processCancel()
	}

	var err error
	if session != nil {
		err = session.Close()
	}
	if processCancel != nil && isExpectedStdioStopError(err) {
		return nil
	}
	return err
}

func (c *Client) cancelStdioProcessLocked() {
	if c.processCancel != nil {
		c.processCancel()
		c.processCancel = nil
	}
}

func isExpectedStdioStopError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return true
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
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
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (llm.ToolOutput, error) {
	c.mu.RLock()
	session := c.session
	running := c.running
	c.mu.RUnlock()

	if !running || session == nil {
		return llm.ToolOutput{}, fmt.Errorf("MCP server %s is not running", c.name)
	}

	// Parse arguments
	var arguments map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &arguments); err != nil {
			return llm.ToolOutput{}, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      name,
		Arguments: arguments,
	})
	if err != nil {
		return llm.ToolOutput{}, fmt.Errorf("call tool %s: %w", name, err)
	}

	output := formatContent(result.Content)
	output.IsError = result.IsError
	return output, nil
}

// formatContent converts MCP content to ordered LLM tool-result content. Content
// remains the concatenation of all textual parts for callers and providers that
// only understand text.
func formatContent(content []mcp.Content) llm.ToolOutput {
	output := llm.ToolOutput{
		ContentParts: make([]llm.ToolContentPart, 0, len(content)),
	}
	var text strings.Builder

	for _, contentPart := range content {
		switch part := contentPart.(type) {
		case *mcp.TextContent:
			if part == nil {
				appendTextContentPart(&output, &text, fallbackContent(part))
				continue
			}
			appendTextContentPart(&output, &text, part.Text)
		case *mcp.ImageContent:
			if mediaType, ok := supportedImageMediaType(part); ok {
				output.ContentParts = append(output.ContentParts, llm.ToolContentPart{
					Type: llm.ToolContentPartImageData,
					ImageData: &llm.ToolImageData{
						MediaType: mediaType,
						Base64:    base64.StdEncoding.EncodeToString(part.Data),
					},
				})
				continue
			}
			appendTextContentPart(&output, &text, fallbackContent(part))
		default:
			// The current LLM result model cannot represent audio or resources.
			// Keep their MCP JSON as text rather than silently dropping them.
			appendTextContentPart(&output, &text, fallbackContent(contentPart))
		}
	}

	output.Content = text.String()
	if len(output.ContentParts) == 0 {
		output.ContentParts = nil
	}
	return output
}

func appendTextContentPart(output *llm.ToolOutput, text *strings.Builder, content string) {
	output.ContentParts = append(output.ContentParts, llm.ToolContentPart{
		Type: llm.ToolContentPartText,
		Text: content,
	})
	text.WriteString(content)
}

func supportedImageMediaType(content *mcp.ImageContent) (string, bool) {
	if content == nil || len(content.Data) == 0 {
		return "", false
	}
	mediaType, _, err := mime.ParseMediaType(content.MIMEType)
	if err != nil {
		return "", false
	}
	mediaType = strings.ToLower(mediaType)
	switch mediaType {
	case "image/jpeg", "image/png", "image/gif", "image/webp":
		return mediaType, true
	default:
		return "", false
	}
}

func fallbackContent(content mcp.Content) string {
	data, err := json.Marshal(content)
	if err == nil {
		return string(data)
	}
	return fmt.Sprintf("[unsupported MCP content %T; JSON encoding failed: %v]", content, err)
}
