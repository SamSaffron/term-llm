package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/procutil"
)

var mcpCommandWaitDelay = time.Second

const mcpStderrLimit = 64 * 1024

type synchronizedLimitedBuffer struct {
	mu        sync.Mutex
	data      []byte
	limit     int
	truncated bool
}

func newSynchronizedLimitedBuffer(limit int) *synchronizedLimitedBuffer {
	return &synchronizedLimitedBuffer{limit: limit}
}

func (b *synchronizedLimitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	written := len(p)
	remaining := b.limit - len(b.data)
	if remaining <= 0 {
		b.truncated = b.truncated || written > 0
		return written, nil
	}
	if len(p) > remaining {
		p = p[:remaining]
		b.truncated = true
	}
	b.data = append(b.data, p...)
	return written, nil
}

func (b *synchronizedLimitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	output := strings.TrimSpace(string(b.data))
	if output != "" && b.truncated {
		output += "\n[stderr truncated]"
	}
	return output
}

// ToolSpec describes a tool available from an MCP server.
type ToolSpec struct {
	Name        string
	Description string
	Schema      map[string]any
}

// Client wraps an MCP server connection.
type Client struct {
	name              string
	config            ServerConfig
	client            *mcp.Client
	session           *mcp.ClientSession
	samplingHandler   *SamplingHandler
	processCancel     context.CancelFunc
	refreshCancel     context.CancelFunc
	refreshDone       chan struct{}
	toolRefreshSignal chan struct{}
	stdioStderr       *synchronizedLimitedBuffer
	maxToolsPerServer int
	onCatalogueChange func(oldSnapshot, newSnapshot *ToolSnapshot, err error)
	snapshot          atomic.Pointer[ToolSnapshot]
	lifecycleMu       sync.Mutex
	mu                sync.RWMutex
	running           bool
}

// NewClient creates a new MCP client for the given server configuration.
func NewClient(name string, config ServerConfig) *Client {
	return &Client{
		name:              name,
		config:            config,
		toolRefreshSignal: make(chan struct{}, 1),
	}
}

// SetSamplingHandler sets the sampling handler for this client.
func (c *Client) SetSamplingHandler(handler *SamplingHandler) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.samplingHandler = handler
}

// SetCatalogueChangeHandler installs the callback used after complete snapshot
// publication. The callback is always invoked without a client lock held.
func (c *Client) SetCatalogueChangeHandler(handler func(oldSnapshot, newSnapshot *ToolSnapshot, err error)) {
	c.mu.Lock()
	c.onCatalogueChange = handler
	c.mu.Unlock()
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
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.RLock()
	if c.running {
		c.mu.RUnlock()
		return nil
	}
	samplingHandler := c.samplingHandler
	c.mu.RUnlock()
	select {
	case <-c.toolRefreshSignal:
	default:
	}

	// Notification handlers are serialized by the SDK, so this callback only
	// signals a worker and never performs remote I/O.
	clientOpts := &mcp.ClientOptions{
		ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
			select {
			case c.toolRefreshSignal <- struct{}{}:
			default:
			}
		},
	}
	if samplingHandler != nil {
		clientName := c.name
		clientOpts.CreateMessageHandler = func(ctx context.Context, req *mcp.CreateMessageRequest) (*mcp.CreateMessageResult, error) {
			return samplingHandler.Handle(ctx, clientName, req)
		}
	}

	client := mcp.NewClient(&mcp.Implementation{
		Name:    "term-llm",
		Version: "1.0.0",
	}, clientOpts)

	var transport mcp.Transport
	if c.config.TransportType() == "http" {
		transport = c.createHTTPTransport()
	} else {
		transport = c.createStdioTransport(processCtx)
	}

	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		c.mu.Lock()
		c.cancelStdioProcessLocked()
		c.mu.Unlock()
		return fmt.Errorf("connect to MCP server %s: %w", c.name, c.withStdioStderr(err))
	}

	candidate, err := c.acquireToolSnapshot(ctx, session)
	if err != nil {
		c.mu.Lock()
		c.cancelStdioProcessLocked()
		c.mu.Unlock()
		_ = session.Close()
		return fmt.Errorf("list tools from %s: %w", c.name, c.withStdioStderr(err))
	}

	refreshCtx, refreshCancel := context.WithCancel(processCtx)
	refreshDone := make(chan struct{})
	c.mu.Lock()
	c.client = client
	c.session = session
	c.refreshCancel = refreshCancel
	c.refreshDone = refreshDone
	c.running = true
	c.mu.Unlock()
	c.snapshot.Store(candidate)
	slog.Debug("MCP tool catalogue initial snapshot published", "server", c.name, "generation", candidate.Generation, "tools", len(candidate.Tools), "hash", candidate.Hash)
	go c.refreshWorker(refreshCtx, session, refreshDone)
	return nil
}

// createStdioTransport creates a stdio transport for command-based servers.
func (c *Client) createStdioTransport(ctx context.Context) mcp.Transport {
	processCtx, processCancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.processCancel = processCancel
	c.mu.Unlock()

	cmd := exec.CommandContext(processCtx, c.config.Command, c.config.Args...)
	cmd.WaitDelay = mcpCommandWaitDelay
	procutil.ConfigureDetachedCommand(cmd)
	stderr := newSynchronizedLimitedBuffer(mcpStderrLimit)
	cmd.Stderr = stderr
	c.mu.Lock()
	c.stdioStderr = stderr
	c.mu.Unlock()
	if len(c.config.Env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range c.config.Env {
			cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
		}
	}
	return &mcp.CommandTransport{Command: cmd}
}

func (c *Client) withStdioStderr(err error) error {
	if err == nil || c.config.TransportType() == "http" {
		return err
	}
	c.mu.RLock()
	stderr := c.stdioStderr
	c.mu.RUnlock()
	if stderr == nil {
		return err
	}
	output := stderr.String()
	if output == "" {
		return err
	}
	return fmt.Errorf("%w\n\nMCP server stderr:\n%s", err, output)
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

func (c *Client) currentSession() *mcp.ClientSession {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.session
}

// clearTerminatedSession clears only the exact active session observed by its
// watcher. An old watcher therefore cannot disrupt a replacement session.
func (c *Client) clearTerminatedSession(session *mcp.ClientSession) bool {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	if c.session != session || !c.running {
		c.mu.Unlock()
		return false
	}
	processCancel := c.processCancel
	refreshCancel := c.refreshCancel
	refreshDone := c.refreshDone
	c.session = nil
	c.processCancel = nil
	c.refreshCancel = nil
	c.refreshDone = nil
	c.running = false
	c.mu.Unlock()
	c.snapshot.Store(nil)

	if refreshCancel != nil {
		refreshCancel()
	}
	if processCancel != nil {
		processCancel()
	}
	if refreshDone != nil {
		<-refreshDone
	}
	return true
}

// Stop closes the MCP server connection.
func (c *Client) Stop() error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	c.mu.Lock()
	if !c.running && c.session == nil {
		c.mu.Unlock()
		return nil
	}

	session := c.session
	processCancel := c.processCancel
	refreshCancel := c.refreshCancel
	refreshDone := c.refreshDone
	c.session = nil
	c.processCancel = nil
	c.refreshCancel = nil
	c.refreshDone = nil
	c.running = false
	c.mu.Unlock()
	c.snapshot.Store(nil)

	if refreshCancel != nil {
		refreshCancel()
	}
	if processCancel != nil {
		processCancel()
	}

	var err error
	if session != nil {
		err = session.Close()
	}
	if refreshDone != nil {
		<-refreshDone
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

// Tools returns a copy-safe legacy projection of the current complete snapshot.
func (c *Client) Tools() []ToolSpec {
	snapshot := c.snapshot.Load()
	if snapshot == nil {
		return nil
	}
	tools := make([]ToolSpec, 0, len(snapshot.Tools))
	for _, tool := range snapshot.Tools {
		tools = append(tools, ToolSpec{
			Name:        tool.OriginalName,
			Description: tool.Description,
			Schema:      cloneMap(tool.InputSchema),
		})
	}
	return tools
}

// ToolSnapshot returns a copy-safe view of the immutable current snapshot.
func (c *Client) ToolSnapshot() *ToolSnapshot {
	return copyToolSnapshot(c.snapshot.Load())
}

func (c *Client) acquireToolSnapshot(ctx context.Context, session *mcp.ClientSession) (*ToolSnapshot, error) {
	if session == nil {
		return nil, fmt.Errorf("MCP server %s has no active session", c.name)
	}
	maxTools := c.maxToolsPerServer
	if maxTools <= 0 {
		maxTools = MaxToolsPerServer
	}
	var tools []*mcp.Tool
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			return nil, err
		}
		if len(tools) >= maxTools {
			return nil, fmt.Errorf("MCP server %s exceeds maximum catalogue size of %d tools", c.name, maxTools)
		}
		tools = append(tools, tool)
	}
	previous := c.snapshot.Load()
	generation := uint64(1)
	if previous != nil {
		generation = previous.Generation + 1
	}
	return buildToolSnapshotWithDescription(c.name, generation, mcpNamespaceDescription(session.InitializeResult()), tools)
}

func mcpNamespaceDescription(result *mcp.InitializeResult) string {
	if result == nil {
		return ""
	}
	if instructions := strings.TrimSpace(result.Instructions); instructions != "" {
		return instructions
	}
	if result.ServerInfo == nil {
		return ""
	}
	name := strings.TrimSpace(result.ServerInfo.Title)
	if name == "" {
		name = strings.TrimSpace(result.ServerInfo.Name)
	}
	if name == "" {
		return ""
	}
	return fmt.Sprintf("Tools provided by %s.", name)
}

func (c *Client) publishRefresh(session *mcp.ClientSession, candidate *ToolSnapshot) bool {
	c.mu.Lock()
	if !c.running || c.session != session {
		c.mu.Unlock()
		return false
	}
	old := c.snapshot.Swap(candidate)
	handler := c.onCatalogueChange
	c.mu.Unlock()

	slog.Debug("MCP tool catalogue refresh published", "server", c.name, "generation", candidate.Generation, "tools", len(candidate.Tools), "hash", candidate.Hash)
	if handler != nil {
		handler(copyToolSnapshot(old), copyToolSnapshot(candidate), nil)
	}
	return true
}

func (c *Client) reportRefreshError(err error) {
	c.mu.RLock()
	handler := c.onCatalogueChange
	c.mu.RUnlock()
	if handler != nil {
		snapshot := copyToolSnapshot(c.snapshot.Load())
		handler(snapshot, snapshot, err)
		return
	}
	slog.Warn("MCP tool catalogue refresh failed; retaining previous snapshot", "server", c.name, "error", err)
}

func (c *Client) refreshWorker(ctx context.Context, session *mcp.ClientSession, done chan struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.toolRefreshSignal:
		}

		refreshCtx, cancel := context.WithTimeout(ctx, mcpStartupTimeout)
		candidate, err := c.acquireToolSnapshot(refreshCtx, session)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			c.reportRefreshError(err)
			continue
		}
		if !c.publishRefresh(session, candidate) {
			return
		}
	}
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
