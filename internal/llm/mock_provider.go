package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
	"unicode/utf8"
)

// MockTurn represents a single response turn from the mock provider.
type MockTurn struct {
	Text      string        // Text to emit (will be chunked for realistic streaming)
	ToolCalls []ToolCall    // Tool calls to emit
	Usage     Usage         // Token usage to report
	Delay     time.Duration // Optional delay before responding (for timeout tests)
	Error     error         // Return this error instead of responding
	// InlineTools emits ToolCalls the way inline-loop bridges (claude-bin) do:
	// each call carries a ToolResponse channel and the turn waits for its
	// result before continuing. InlineText is emitted after every result.
	InlineTools bool
	InlineText  string
	// InlineParallel sends every inline call before waiting for any result,
	// as a bridge does when the model issues parallel tool calls.
	InlineParallel bool
}

// MockProvider is a configurable provider for testing.
// It returns scripted responses and records all requests for verification.
type MockProvider struct {
	name         string
	capabilities Capabilities
	turns        []MockTurn
	turnIndex    int
	Requests     []Request // Recorded requests for verification
	mu           sync.Mutex
	// inlineResults records results inline tool calls received, by call ID.
	inlineResults map[string]ToolExecutionResponse
}

// NewMockProvider creates a new mock provider with the given name.
func NewMockProvider(name string) *MockProvider {
	return &MockProvider{
		name:         name,
		capabilities: Capabilities{ToolCalls: true},
	}
}

// Name returns the provider name.
func (m *MockProvider) Name() string {
	return m.name
}

// Credential returns "mock" for the mock provider.
func (m *MockProvider) Credential() string {
	return "mock"
}

// Capabilities returns the provider capabilities.
func (m *MockProvider) Capabilities() Capabilities {
	return m.capabilities
}

// WithCapabilities sets the provider capabilities and returns the provider for chaining.
func (m *MockProvider) WithCapabilities(c Capabilities) *MockProvider {
	m.capabilities = c
	return m
}

// AddTurn adds a response turn and returns the provider for chaining.
func (m *MockProvider) AddTurn(t MockTurn) *MockProvider {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turns = append(m.turns, t)
	return m
}

// AddTextResponse is a convenience method to add a simple text response.
func (m *MockProvider) AddTextResponse(text string) *MockProvider {
	return m.AddTurn(MockTurn{Text: text})
}

// AddToolCall is a convenience method to add a turn with a single tool call.
func (m *MockProvider) AddToolCall(id, name string, args any) *MockProvider {
	argsJSON, err := json.Marshal(args)
	if err != nil {
		panic(fmt.Sprintf("failed to marshal tool call args: %v", err))
	}
	return m.AddTurn(MockTurn{
		ToolCalls: []ToolCall{{
			ID:        id,
			Name:      name,
			Arguments: argsJSON,
		}},
	})
}

// AddError adds a turn that returns an error.
func (m *MockProvider) AddError(err error) *MockProvider {
	return m.AddTurn(MockTurn{Error: err})
}

// RecordedRequests returns a snapshot of requests observed by the provider.
func (m *MockProvider) RecordedRequests() []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Request(nil), m.Requests...)
}

// InlineToolResult returns the result an inline tool call received.
func (m *MockProvider) InlineToolResult(callID string) (ToolExecutionResponse, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	result, ok := m.inlineResults[callID]
	return result, ok
}

// Reset clears recorded requests and resets the turn index.
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnIndex = 0
	m.Requests = nil
	m.inlineResults = nil
}

// ResetTurns clears the scripted turns and resets the turn index.
func (m *MockProvider) ResetTurns() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnIndex = 0
	m.turns = nil
	m.inlineResults = nil
}

// TurnCount returns the number of scripted turns.
func (m *MockProvider) TurnCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.turns)
}

// CurrentTurn returns the current turn index (0-based).
func (m *MockProvider) CurrentTurn() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.turnIndex
}

// Stream implements the Provider interface.
func (m *MockProvider) Stream(ctx context.Context, req Request) (Stream, error) {
	m.mu.Lock()
	m.Requests = append(m.Requests, req)

	if m.turnIndex >= len(m.turns) {
		m.mu.Unlock()
		return nil, fmt.Errorf("mock provider: no more turns configured (expected turn %d, have %d)", m.turnIndex, len(m.turns))
	}

	turn := m.turns[m.turnIndex]
	m.turnIndex++
	m.mu.Unlock()

	return newEventStream(ctx, func(ctx context.Context, send eventSender) error {
		// Apply delay if configured
		if turn.Delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(turn.Delay):
			}
		}

		// Return error if configured
		if turn.Error != nil {
			return turn.Error
		}

		// Emit text in chunks (simulates realistic streaming)
		if turn.Text != "" {
			for _, chunk := range chunkText(turn.Text, 10) {
				if err := send.Send(Event{Type: EventTextDelta, Text: chunk}); err != nil {
					return err
				}
			}
		}

		if turn.InlineTools {
			if err := m.emitInlineToolCalls(ctx, send, turn); err != nil {
				return err
			}
			return send.Send(Event{Type: EventUsage, Use: &turn.Usage})
		}

		// Emit tool calls
		for i := range turn.ToolCalls {
			if err := send.Send(Event{Type: EventToolCall, Tool: &turn.ToolCalls[i]}); err != nil {
				return err
			}
		}

		// Emit usage
		return send.Send(Event{Type: EventUsage, Use: &turn.Usage})
	}), nil
}

// emitInlineToolCalls sends each call with a response channel, waits for its
// result, records it, and then emits the turn's InlineText. With
// InlineParallel every call is sent before any result is awaited.
func (m *MockProvider) emitInlineToolCalls(ctx context.Context, send eventSender, turn MockTurn) error {
	responses := make([]chan ToolExecutionResponse, len(turn.ToolCalls))
	for i := range turn.ToolCalls {
		call := turn.ToolCalls[i]
		responses[i] = make(chan ToolExecutionResponse, 1)
		if err := send.Send(Event{Type: EventToolCall, ToolCallID: call.ID, ToolName: call.Name, Tool: &call, ToolResponse: responses[i]}); err != nil {
			return err
		}
		if !turn.InlineParallel {
			if err := m.awaitInlineResult(ctx, call.ID, responses[i]); err != nil {
				return err
			}
		}
	}
	if turn.InlineParallel {
		for i, call := range turn.ToolCalls {
			if err := m.awaitInlineResult(ctx, call.ID, responses[i]); err != nil {
				return err
			}
		}
	}
	if turn.InlineText == "" {
		return nil
	}
	return send.Send(Event{Type: EventTextDelta, Text: turn.InlineText})
}

func (m *MockProvider) awaitInlineResult(ctx context.Context, callID string, response <-chan ToolExecutionResponse) error {
	select {
	case result := <-response:
		m.mu.Lock()
		if m.inlineResults == nil {
			m.inlineResults = make(map[string]ToolExecutionResponse)
		}
		m.inlineResults[callID] = result
		m.mu.Unlock()
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// chunkText splits text into chunks of approximately the given size.
// It tries to break at word boundaries when possible and always splits
// on valid rune boundaries so that every chunk is valid UTF-8.
func chunkText(text string, chunkSize int) []string {
	if len(text) == 0 {
		return nil
	}
	if len(text) <= chunkSize {
		return []string{text}
	}

	var chunks []string
	for len(text) > 0 {
		if len(text) <= chunkSize {
			chunks = append(chunks, text)
			break
		}

		// Start at the byte-level chunk boundary, then back up to a
		// valid rune start so we never split a multi-byte codepoint.
		breakPoint := chunkSize
		for breakPoint > 0 && !utf8.RuneStart(text[breakPoint]) {
			breakPoint--
		}

		// Try to find a space to break at within the second half.
		for i := breakPoint; i > chunkSize/2; i-- {
			if text[i] == ' ' {
				breakPoint = i + 1 // include the space in current chunk
				break
			}
		}

		chunks = append(chunks, text[:breakPoint])
		text = text[breakPoint:]
	}
	return chunks
}
