package mcp

import (
	"context"
	"io"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/samsaffron/term-llm/internal/llm"
)

// mockProvider is a simple mock LLM provider for testing
type mockProvider struct {
	name      string
	responses []string
	callCount int
}

func (m *mockProvider) Name() string {
	return m.name
}

func (m *mockProvider) Credential() string {
	return "mock"
}

func (m *mockProvider) Capabilities() llm.Capabilities {
	return llm.Capabilities{ToolCalls: true}
}

func (m *mockProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	response := "Hello from mock"
	if m.callCount < len(m.responses) {
		response = m.responses[m.callCount]
	}
	m.callCount++
	return &mockStream{response: response}, nil
}

type mockStream struct {
	response string
	sent     bool
	done     bool
}

func (s *mockStream) Recv() (llm.Event, error) {
	if !s.sent {
		s.sent = true
		return llm.Event{Type: llm.EventTextDelta, Text: s.response}, nil
	}
	if !s.done {
		s.done = true
		return llm.Event{Type: llm.EventDone}, nil
	}
	return llm.Event{}, io.EOF
}

func (s *mockStream) Close() error {
	return nil
}

func TestConvertSamplingMessages(t *testing.T) {
	tests := []struct {
		name     string
		msgs     []*mcp.SamplingMessage
		expected []llm.Message
	}{
		{
			name: "single user message",
			msgs: []*mcp.SamplingMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Hello"}},
			},
			expected: []llm.Message{
				{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "Hello"}}},
			},
		},
		{
			name: "user and assistant messages",
			msgs: []*mcp.SamplingMessage{
				{Role: "user", Content: &mcp.TextContent{Text: "Hello"}},
				{Role: "assistant", Content: &mcp.TextContent{Text: "Hi there!"}},
			},
			expected: []llm.Message{
				{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "Hello"}}},
				{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "Hi there!"}}},
			},
		},
		{
			name:     "empty messages",
			msgs:     []*mcp.SamplingMessage{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertSamplingMessages(tt.msgs)
			if len(result) != len(tt.expected) {
				t.Fatalf("expected %d messages, got %d", len(tt.expected), len(result))
			}
			for i, msg := range result {
				if msg.Role != tt.expected[i].Role {
					t.Errorf("message %d: expected role %v, got %v", i, tt.expected[i].Role, msg.Role)
				}
				if len(msg.Parts) != len(tt.expected[i].Parts) {
					t.Errorf("message %d: expected %d parts, got %d", i, len(tt.expected[i].Parts), len(msg.Parts))
					continue
				}
				for j, part := range msg.Parts {
					if part.Text != tt.expected[i].Parts[j].Text {
						t.Errorf("message %d part %d: expected text %q, got %q", i, j, tt.expected[i].Parts[j].Text, part.Text)
					}
				}
			}
		})
	}
}

func TestSamplingConfigDefaults(t *testing.T) {
	// Test nil config
	var nilConfig *SamplingConfig
	if !nilConfig.IsSamplingEnabled() {
		t.Error("nil config should have sampling enabled by default")
	}

	// Test empty config (Enabled nil)
	emptyConfig := &SamplingConfig{}
	if !emptyConfig.IsSamplingEnabled() {
		t.Error("empty config should have sampling enabled by default")
	}

	// Test explicitly enabled
	enabled := true
	enabledConfig := &SamplingConfig{Enabled: &enabled}
	if !enabledConfig.IsSamplingEnabled() {
		t.Error("explicitly enabled config should return true")
	}

	// Test explicitly disabled
	disabled := false
	disabledConfig := &SamplingConfig{Enabled: &disabled}
	if disabledConfig.IsSamplingEnabled() {
		t.Error("explicitly disabled config should return false")
	}
}

func TestSamplingHandlerYoloMode(t *testing.T) {
	provider := &mockProvider{name: "test", responses: []string{"Test response"}}
	handler := NewSamplingHandler(provider, "test-model")

	// Test with yolo mode enabled
	handler.SetYoloMode(true)
	handler.SetServerConfig("test-server", ServerConfig{})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens: 100,
		},
	}

	result, err := handler.Handle(context.Background(), "test-server", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Role != "assistant" {
		t.Errorf("expected role 'assistant', got %q", result.Role)
	}

	textContent, ok := result.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content)
	}
	if textContent.Text != "Test response" {
		t.Errorf("expected 'Test response', got %q", textContent.Text)
	}
}

func TestSamplingHandlerAutoApprove(t *testing.T) {
	provider := &mockProvider{name: "test", responses: []string{"Auto-approved response"}}
	handler := NewSamplingHandler(provider, "test-model")

	// Configure server with auto_approve
	handler.SetServerConfig("auto-server", ServerConfig{
		Sampling: &SamplingConfig{AutoApprove: true},
	})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens: 100,
		},
	}

	result, err := handler.Handle(context.Background(), "auto-server", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	textContent, ok := result.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content)
	}
	if textContent.Text != "Auto-approved response" {
		t.Errorf("expected 'Auto-approved response', got %q", textContent.Text)
	}
}

func TestSamplingHandlerDisabled(t *testing.T) {
	provider := &mockProvider{name: "test"}
	handler := NewSamplingHandler(provider, "test-model")

	// Configure server with sampling disabled
	disabled := false
	handler.SetServerConfig("disabled-server", ServerConfig{
		Sampling: &SamplingConfig{Enabled: &disabled},
	})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens: 100,
		},
	}

	_, err := handler.Handle(context.Background(), "disabled-server", req)
	if err == nil {
		t.Fatal("expected error for disabled sampling, got nil")
	}
	if err.Error() != "sampling is disabled for server disabled-server" {
		t.Errorf("unexpected error message: %v", err)
	}
}

func TestSamplingHandlerSessionApproval(t *testing.T) {
	provider := &mockProvider{name: "test", responses: []string{"First response", "Second response"}}
	handler := NewSamplingHandler(provider, "test-model")
	handler.SetYoloMode(true) // Use yolo mode to simulate session approval
	handler.SetServerConfig("session-server", ServerConfig{})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens: 100,
		},
	}

	// First call - should work and store approval
	result1, err := handler.Handle(context.Background(), "session-server", req)
	if err != nil {
		t.Fatalf("first call unexpected error: %v", err)
	}

	textContent1, ok := result1.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result1.Content)
	}
	if textContent1.Text != "First response" {
		t.Errorf("first call: expected 'First response', got %q", textContent1.Text)
	}

	// Second call - should use cached approval
	result2, err := handler.Handle(context.Background(), "session-server", req)
	if err != nil {
		t.Fatalf("second call unexpected error: %v", err)
	}

	textContent2, ok := result2.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result2.Content)
	}
	if textContent2.Text != "Second response" {
		t.Errorf("second call: expected 'Second response', got %q", textContent2.Text)
	}
}

func TestSamplingHandlerMaxTokensOverride(t *testing.T) {
	var capturedRequest llm.Request
	provider := &mockProvider{name: "test", responses: []string{"Response"}}

	// Create a wrapper to capture the request
	wrappedProvider := &requestCapturingProvider{
		Provider:      provider,
		lastRequest:   &capturedRequest,
		shouldCapture: true,
	}

	handler := NewSamplingHandler(wrappedProvider, "test-model")
	handler.SetYoloMode(true)

	// Configure server with max_tokens limit
	handler.SetServerConfig("limited-server", ServerConfig{
		Sampling: &SamplingConfig{MaxTokens: 50},
	})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens: 100, // Request 100, but server config limits to 50
		},
	}

	_, err := handler.Handle(context.Background(), "limited-server", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if capturedRequest.MaxOutputTokens != 50 {
		t.Errorf("expected MaxOutputTokens 50, got %d", capturedRequest.MaxOutputTokens)
	}
}

// requestCapturingProvider wraps a provider to capture requests
type requestCapturingProvider struct {
	llm.Provider
	lastRequest   *llm.Request
	shouldCapture bool
}

func (p *requestCapturingProvider) Stream(ctx context.Context, req llm.Request) (llm.Stream, error) {
	if p.shouldCapture && p.lastRequest != nil {
		*p.lastRequest = req
	}
	return p.Provider.Stream(ctx, req)
}

func TestSamplingHandlerNilSamplingConfig(t *testing.T) {
	// Test that handler works when ServerConfig.Sampling is nil
	provider := &mockProvider{name: "test", responses: []string{"Response"}}
	handler := NewSamplingHandler(provider, "test-model")
	handler.SetYoloMode(true)

	// Configure server with nil Sampling config
	handler.SetServerConfig("nil-sampling-server", ServerConfig{
		Command: "test-command",
		// Sampling is nil
	})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:  []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens: 100,
		},
	}

	result, err := handler.Handle(context.Background(), "nil-sampling-server", req)
	if err != nil {
		t.Fatalf("unexpected error with nil Sampling config: %v", err)
	}

	textContent, ok := result.Content.(*mcp.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content)
	}
	if textContent.Text != "Response" {
		t.Errorf("expected 'Response', got %q", textContent.Text)
	}
}

func TestSamplingHandlerSystemPrompt(t *testing.T) {
	var capturedRequest llm.Request
	provider := &mockProvider{name: "test", responses: []string{"Response"}}

	wrappedProvider := &requestCapturingProvider{
		Provider:      provider,
		lastRequest:   &capturedRequest,
		shouldCapture: true,
	}

	handler := NewSamplingHandler(wrappedProvider, "test-model")
	handler.SetYoloMode(true)
	handler.SetServerConfig("test-server", ServerConfig{})

	req := &mcp.CreateMessageRequest{
		Params: &mcp.CreateMessageParams{
			Messages:     []*mcp.SamplingMessage{{Role: "user", Content: &mcp.TextContent{Text: "Hello"}}},
			MaxTokens:    100,
			SystemPrompt: "You are a helpful assistant.",
		},
	}

	_, err := handler.Handle(context.Background(), "test-server", req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check that system prompt was included
	if len(capturedRequest.Messages) < 2 {
		t.Fatalf("expected at least 2 messages (system + user), got %d", len(capturedRequest.Messages))
	}
	if capturedRequest.Messages[0].Role != llm.RoleSystem {
		t.Errorf("expected first message to be system, got %v", capturedRequest.Messages[0].Role)
	}
	if len(capturedRequest.Messages[0].Parts) == 0 || capturedRequest.Messages[0].Parts[0].Text != "You are a helpful assistant." {
		t.Error("system prompt not correctly included in request")
	}
}

func TestSamplingApprovalModel_SpaceConfirmsSelection(t *testing.T) {
	m := newSamplingApprovalModel("test-server", &mcp.CreateMessageParams{}, 80)
	m.cursor = 1

	model, _ := m.Update(tea.KeyPressMsg{Code: tea.KeySpace})
	updated := model.(samplingApprovalModel)

	if !updated.done {
		t.Fatal("expected space to complete sampling approval")
	}
	if got := updated.choice; got != SamplingChoiceDeny {
		t.Fatalf("expected deny choice from cursor 1, got %v", got)
	}
}
