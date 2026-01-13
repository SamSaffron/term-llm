package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// MockTurn represents a single response turn from the mock provider.
type MockTurn struct {
	Text      string        // Text to emit (will be chunked for realistic streaming)
	ToolCalls []ToolCall    // Tool calls to emit
	Usage     Usage         // Token usage to report
	Delay     time.Duration // Optional delay before responding (for timeout tests)
	Error     error         // Return this error instead of responding
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

// Reset clears recorded requests and resets the turn index.
func (m *MockProvider) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnIndex = 0
	m.Requests = nil
}

// ResetTurns clears the scripted turns and resets the turn index.
func (m *MockProvider) ResetTurns() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turnIndex = 0
	m.turns = nil
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

	return newEventStream(ctx, func(ctx context.Context, ch chan<- Event) error {
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
				select {
				case <-ctx.Done():
					return ctx.Err()
				case ch <- Event{Type: EventTextDelta, Text: chunk}:
				}
			}
		}

		// Emit tool calls
		for i := range turn.ToolCalls {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case ch <- Event{Type: EventToolCall, Tool: &turn.ToolCalls[i]}:
			}
		}

		// Emit usage
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ch <- Event{Type: EventUsage, Use: &turn.Usage}:
		}

		return nil
	}), nil
}

// chunkText splits text into chunks of approximately the given size.
// It tries to break at word boundaries when possible.
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

		// Find a good break point (space) near the chunk size
		breakPoint := chunkSize
		for i := chunkSize; i > chunkSize/2; i-- {
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
