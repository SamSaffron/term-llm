package testutil

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

// ApprovalResponse represents how to respond to an approval prompt.
type ApprovalResponse int

const (
	ApprovalApprove ApprovalResponse = iota
	ApprovalDeny
	ApprovalAbort
)

// EngineHarness provides a test harness for engine-level testing without TUI.
type EngineHarness struct {
	Provider *llm.MockProvider
	Engine   *llm.Engine
	Registry *llm.ToolRegistry
	Screen   *ScreenCapture

	// Recorded data
	TextOutput     strings.Builder
	ToolCalls      []llm.ToolCall
	ToolExecutions []ToolExecution
	Events         []llm.Event

	// Approval handling
	approvalResponse ApprovalResponse
	approvalMu       sync.Mutex
}

// ToolExecution records a tool execution.
type ToolExecution struct {
	Name   string
	Args   json.RawMessage
	Result string
	Error  error
}

// NewEngineHarness creates a new engine test harness.
func NewEngineHarness() *EngineHarness {
	provider := llm.NewMockProvider("test-mock")
	registry := llm.NewToolRegistry()

	h := &EngineHarness{
		Provider:         provider,
		Registry:         registry,
		Screen:           NewScreenCapture(),
		approvalResponse: ApprovalApprove, // default to auto-approve
	}

	h.Engine = llm.NewEngine(provider, registry)
	return h
}

// AddTool registers a tool with the engine.
func (h *EngineHarness) AddTool(tool llm.Tool) {
	h.Registry.Register(tool)
}

// AddMockTool creates and registers a simple mock tool.
func (h *EngineHarness) AddMockTool(name, result string) *MockTool {
	tool := NewMockTool(name, result)
	h.Registry.Register(tool)
	return tool
}

// AutoApproveAll sets the harness to automatically approve all tool calls.
func (h *EngineHarness) AutoApproveAll() {
	h.approvalMu.Lock()
	defer h.approvalMu.Unlock()
	h.approvalResponse = ApprovalApprove
}

// AutoDenyAll sets the harness to automatically deny all tool calls.
func (h *EngineHarness) AutoDenyAll() {
	h.approvalMu.Lock()
	defer h.approvalMu.Unlock()
	h.approvalResponse = ApprovalDeny
}

// Reset clears all recorded data and resets the provider.
func (h *EngineHarness) Reset() {
	h.TextOutput.Reset()
	h.ToolCalls = nil
	h.ToolExecutions = nil
	h.Events = nil
	h.Provider.Reset()
	h.Screen.Clear()
}

// Stream runs a streaming request and collects all output.
func (h *EngineHarness) Stream(ctx context.Context, req llm.Request) (string, error) {
	h.Screen.Enable()
	defer h.Screen.Disable()

	stream, err := h.Engine.Stream(ctx, req)
	if err != nil {
		return "", err
	}
	defer stream.Close()

	var textBuilder strings.Builder
	var phase string

	for {
		event, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return textBuilder.String(), err
		}

		h.Events = append(h.Events, event)

		switch event.Type {
		case llm.EventTextDelta:
			textBuilder.WriteString(event.Text)
			h.TextOutput.WriteString(event.Text)
			// Capture screen state after each text update
			h.Screen.Capture(textBuilder.String(), phase)

		case llm.EventToolCall:
			if event.Tool != nil {
				h.ToolCalls = append(h.ToolCalls, *event.Tool)
			}

		case llm.EventToolExecStart:
			if event.ToolName != "" {
				phase = fmt.Sprintf("%s: %s", event.ToolName, event.ToolInfo)
			} else {
				phase = "Thinking"
			}

		case llm.EventError:
			if event.Err != nil {
				return textBuilder.String(), event.Err
			}
		}
	}

	// Final capture
	h.Screen.Capture(textBuilder.String(), "Done")

	return textBuilder.String(), nil
}

// Run is an alias for Stream that returns the text output.
func (h *EngineHarness) Run(ctx context.Context, req llm.Request) (string, error) {
	return h.Stream(ctx, req)
}

// GetOutput returns the accumulated text output.
func (h *EngineHarness) GetOutput() string {
	return h.TextOutput.String()
}

// DumpScreen prints the final screen for debugging.
func (h *EngineHarness) DumpScreen() {
	h.Screen.RenderPlain()
}

// SaveScreen saves the screen to a file.
func (h *EngineHarness) SaveScreen(path string) error {
	return h.Screen.SaveScreenPlain(path)
}

// SaveFrames saves all frames to a directory.
func (h *EngineHarness) SaveFrames(dir string) error {
	return h.Screen.SaveFrames(dir)
}

// EnableScreenCapture enables screen capture.
func (h *EngineHarness) EnableScreenCapture() {
	h.Screen.Enable()
}

// TUIHarness provides a test harness for TUI-level testing with bubbletea.
type TUIHarness struct {
	*EngineHarness

	// TUI control
	input  *bytes.Buffer
	output *bytes.Buffer

	// Approval automation
	approvalQueue []ApprovalAction
	approvalIndex int
	approvalChan  chan ApprovalRequest
	responseChan  chan ApprovalResponseMsg

	// Timing
	timeout time.Duration
}

// ApprovalAction defines what to do when an approval prompt appears.
type ApprovalAction struct {
	WaitFor  string           // Wait for this text to appear before responding
	Response ApprovalResponse // How to respond
}

// ApprovalRequest is sent when approval is needed.
type ApprovalRequest struct {
	Description string
	ToolName    string
	ToolInfo    string
}

// ApprovalResponseMsg is the response to an approval request.
type ApprovalResponseMsg struct {
	Approved bool
	Path     string
}

// NewTUIHarness creates a new TUI test harness.
func NewTUIHarness() *TUIHarness {
	h := &TUIHarness{
		EngineHarness: NewEngineHarness(),
		input:         bytes.NewBuffer(nil),
		output:        bytes.NewBuffer(nil),
		approvalChan:  make(chan ApprovalRequest, 10),
		responseChan:  make(chan ApprovalResponseMsg, 10),
		timeout:       30 * time.Second,
	}
	return h
}

// SetTimeout sets the maximum time to wait for operations.
func (h *TUIHarness) SetTimeout(d time.Duration) {
	h.timeout = d
}

// OnApprovalPrompt adds an approval action to the queue.
func (h *TUIHarness) OnApprovalPrompt(action ApprovalAction) {
	h.approvalQueue = append(h.approvalQueue, action)
}

// GetApprovalResponse returns the response for the current approval.
func (h *TUIHarness) GetApprovalResponse() ApprovalResponse {
	h.approvalMu.Lock()
	defer h.approvalMu.Unlock()

	if h.approvalIndex < len(h.approvalQueue) {
		action := h.approvalQueue[h.approvalIndex]
		h.approvalIndex++
		return action.Response
	}
	return h.approvalResponse // default response
}

// CapturedOutput returns the captured TUI output.
func (h *TUIHarness) CapturedOutput() string {
	return h.output.String()
}

// RunWithOutput runs a request and returns both the text content and TUI output.
func (h *TUIHarness) RunWithOutput(ctx context.Context, req llm.Request) (textContent string, tuiOutput string, err error) {
	textContent, err = h.Run(ctx, req)
	tuiOutput = h.CapturedOutput()
	return
}
