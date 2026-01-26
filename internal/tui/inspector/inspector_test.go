package inspector

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestNew(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello, world!"}},
			TextContent: "Hello, world!",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
		{
			ID:          2,
			SessionID:   "test-session",
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello! How can I help you today?"}},
			TextContent: "Hello! How can I help you today?",
			CreatedAt:   time.Now(),
			Sequence:    1,
		},
	}

	m := New(messages, 80, 24, ui.DefaultStyles())

	if m == nil {
		t.Fatal("New returned nil")
	}

	if len(m.messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(m.messages))
	}

	if m.width != 80 {
		t.Errorf("expected width 80, got %d", m.width)
	}

	if m.height != 24 {
		t.Errorf("expected height 24, got %d", m.height)
	}
}

func TestScrolling(t *testing.T) {
	// Create a message that will result in many lines
	longText := ""
	for i := 0; i < 100; i++ {
		longText += "This is line " + string(rune('0'+i%10)) + " of the long text message.\n"
	}

	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: longText}},
			TextContent: longText,
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	m := New(messages, 80, 24, ui.DefaultStyles())

	// Should start at top
	if m.scrollY != 0 {
		t.Errorf("expected scrollY 0, got %d", m.scrollY)
	}

	// Scroll down
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if m.scrollY != 1 {
		t.Errorf("expected scrollY 1 after scrolling down, got %d", m.scrollY)
	}

	// Scroll up
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if m.scrollY != 0 {
		t.Errorf("expected scrollY 0 after scrolling up, got %d", m.scrollY)
	}

	// Go to bottom
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'G'}})
	if m.scrollY != m.maxScroll() {
		t.Errorf("expected scrollY %d (maxScroll) after G, got %d", m.maxScroll(), m.scrollY)
	}

	// Go to top
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'g'}})
	if m.scrollY != 0 {
		t.Errorf("expected scrollY 0 after g, got %d", m.scrollY)
	}
}

func TestQuit(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Test"}},
			TextContent: "Test",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	m := New(messages, 80, 24, ui.DefaultStyles())

	// Test q key
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd == nil {
		t.Error("expected non-nil command from q key")
	}

	// Execute command and check for CloseMsg
	msg := cmd()
	if _, ok := msg.(CloseMsg); !ok {
		t.Errorf("expected CloseMsg, got %T", msg)
	}
}

func TestView(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello"}},
			TextContent: "Hello",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	m := New(messages, 80, 24, ui.DefaultStyles())
	view := m.View()

	if view == "" {
		t.Error("View() returned empty string")
	}

	// Check for header
	if !contains(view, "Conversation Inspector") {
		t.Error("View() should contain header")
	}

	// Check for help text
	if !contains(view, "q:close") {
		t.Error("View() should contain help text")
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsHelper(s, substr))
}

func containsHelper(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func TestWrapLineWithTabs(t *testing.T) {
	tests := []struct {
		name     string
		line     string
		maxWidth int
		want     []string
	}{
		{
			name:     "tab converted to spaces",
			line:     "\t\"fmt\"",
			maxWidth: 80,
			want:     []string{"  \"fmt\""},
		},
		{
			name:     "multiple tabs",
			line:     "\t\tcode",
			maxWidth: 80,
			want:     []string{"    code"},
		},
		{
			name:     "tab in middle",
			line:     "key\tvalue",
			maxWidth: 80,
			want:     []string{"key  value"},
		},
		{
			name:     "no tabs unchanged",
			line:     "  indented",
			maxWidth: 80,
			want:     []string{"  indented"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := wrapLine(tt.line, tt.maxWidth)
			if len(got) != len(tt.want) {
				t.Errorf("wrapLine() returned %d lines, want %d", len(got), len(tt.want))
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("wrapLine()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestNewWithConfig(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello"}},
			TextContent: "Hello",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	cfg := &Config{
		ProviderName: "anthropic",
		ModelName:    "claude-3-opus",
		ToolSpecs: []llm.ToolSpec{
			{Name: "read_file", Description: "Read a file from disk"},
			{Name: "write_file", Description: "Write content to a file"},
		},
	}

	m := NewWithConfig(messages, 80, 24, ui.DefaultStyles(), nil, cfg)

	if m == nil {
		t.Fatal("NewWithConfig returned nil")
	}

	if m.providerName != "anthropic" {
		t.Errorf("expected providerName 'anthropic', got %q", m.providerName)
	}

	if m.modelName != "claude-3-opus" {
		t.Errorf("expected modelName 'claude-3-opus', got %q", m.modelName)
	}

	if len(m.toolSpecs) != 2 {
		t.Errorf("expected 2 toolSpecs, got %d", len(m.toolSpecs))
	}
}

func TestNewWithConfigNil(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello"}},
			TextContent: "Hello",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	// Test with nil config - should work like NewWithStore
	m := NewWithConfig(messages, 80, 24, ui.DefaultStyles(), nil, nil)

	if m == nil {
		t.Fatal("NewWithConfig with nil config returned nil")
	}

	if m.providerName != "" {
		t.Errorf("expected empty providerName with nil config, got %q", m.providerName)
	}

	if m.modelName != "" {
		t.Errorf("expected empty modelName with nil config, got %q", m.modelName)
	}

	if len(m.toolSpecs) != 0 {
		t.Errorf("expected 0 toolSpecs with nil config, got %d", len(m.toolSpecs))
	}
}

func TestViewWithModelInfo(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello"}},
			TextContent: "Hello",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	cfg := &Config{
		ProviderName: "anthropic",
		ModelName:    "claude-3-opus",
	}

	m := NewWithConfig(messages, 80, 24, ui.DefaultStyles(), nil, cfg)
	view := m.View()

	// Check for model info section
	if !contains(view, "Model Information") {
		t.Error("View() should contain 'Model Information' header when config has provider/model")
	}

	if !contains(view, "anthropic") {
		t.Error("View() should contain provider name")
	}

	if !contains(view, "claude-3-opus") {
		t.Error("View() should contain model name")
	}
}

func TestViewWithToolDefinitions(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello"}},
			TextContent: "Hello",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	}

	cfg := &Config{
		ToolSpecs: []llm.ToolSpec{
			{Name: "read_file", Description: "Read a file from disk"},
			{Name: "write_file", Description: "Write content to a file"},
		},
	}

	m := NewWithConfig(messages, 80, 24, ui.DefaultStyles(), nil, cfg)
	view := m.View()

	// Check for tool definitions section
	if !contains(view, "Tool Definitions") {
		t.Error("View() should contain 'Tool Definitions' header when tools are configured")
	}

	if !contains(view, "2 tools") {
		t.Error("View() should show tool count")
	}

	if !contains(view, "read_file") {
		t.Error("View() should contain tool name 'read_file'")
	}

	if !contains(view, "write_file") {
		t.Error("View() should contain tool name 'write_file'")
	}
}

func TestViewWithSystemMessage(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			SessionID:   "test-session",
			Role:        llm.RoleSystem,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "You are a helpful assistant."}},
			TextContent: "You are a helpful assistant.",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
		{
			ID:          2,
			SessionID:   "test-session",
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello"}},
			TextContent: "Hello",
			CreatedAt:   time.Now(),
			Sequence:    1,
		},
	}

	cfg := &Config{
		ProviderName: "anthropic",
		ModelName:    "claude-3-opus",
	}

	m := NewWithConfig(messages, 80, 24, ui.DefaultStyles(), nil, cfg)
	view := m.View()

	// Check for system prompt section
	if !contains(view, "System Prompt") {
		t.Error("View() should contain 'System Prompt' header when system message exists")
	}

	if !contains(view, "helpful assistant") {
		t.Error("View() should contain system message content")
	}

	// Check for conversation header
	if !contains(view, "Conversation") {
		t.Error("View() should contain 'Conversation' header when there's header content")
	}
}
