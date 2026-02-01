package session

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestExportToMarkdown_BasicSession(t *testing.T) {
	sess := &Session{
		ID:        "abc123def456",
		Name:      "Test Session",
		Provider:  "anthropic",
		Model:     "claude-sonnet-4",
		Mode:      ModeChat,
		Agent:     "default",
		CWD:       "/home/user/project",
		CreatedAt: time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC),
		UserTurns: 2,
		LLMTurns:  3,
		ToolCalls: 5,
	}

	messages := []Message{
		{
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hello, how are you?"}},
			TextContent: "Hello, how are you?",
		},
		{
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "I'm doing well, thank you!"}},
			TextContent: "I'm doing well, thank you!",
		},
	}

	result := ExportToMarkdown(sess, messages, ExportOptions{})

	// Check title
	if !strings.Contains(result, "# Session: Test Session") {
		t.Error("expected session title in output")
	}

	// Check setup table
	if !strings.Contains(result, "| **Agent** | default |") {
		t.Error("expected agent in setup table")
	}
	if !strings.Contains(result, "| **Provider** | anthropic |") {
		t.Error("expected provider in setup table")
	}
	if !strings.Contains(result, "| **Model** | claude-sonnet-4 |") {
		t.Error("expected model in setup table")
	}

	// Check conversation
	if !strings.Contains(result, "### User") {
		t.Error("expected user section")
	}
	if !strings.Contains(result, "Hello, how are you?") {
		t.Error("expected user message content")
	}
	if !strings.Contains(result, "### Assistant") {
		t.Error("expected assistant section")
	}
	if !strings.Contains(result, "I'm doing well, thank you!") {
		t.Error("expected assistant message content")
	}
}

func TestExportToMarkdown_SystemMessage(t *testing.T) {
	sess := &Session{
		ID:        "abc123",
		Provider:  "openai",
		Model:     "gpt-4",
		CreatedAt: time.Now(),
	}

	messages := []Message{
		{
			Role:        llm.RoleSystem,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "You are a helpful assistant."}},
			TextContent: "You are a helpful assistant.",
		},
		{
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "Hi"}},
			TextContent: "Hi",
		},
	}

	// Without IncludeSystem
	result := ExportToMarkdown(sess, messages, ExportOptions{IncludeSystem: false})
	if strings.Contains(result, "### System") {
		t.Error("system message should not appear when IncludeSystem is false")
	}

	// With IncludeSystem
	result = ExportToMarkdown(sess, messages, ExportOptions{IncludeSystem: true})
	if !strings.Contains(result, "### System") {
		t.Error("system message should appear when IncludeSystem is true")
	}
	if !strings.Contains(result, "You are a helpful assistant.") {
		t.Error("system message content should appear")
	}
}

func TestExportToMarkdown_ToolCalls(t *testing.T) {
	sess := &Session{
		ID:        "abc123",
		Provider:  "anthropic",
		Model:     "claude-sonnet-4",
		CreatedAt: time.Now(),
		ToolCalls: 1,
	}

	args, _ := json.Marshal(map[string]string{"command": "ls -la"})

	messages := []Message{
		{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "Let me check the files."},
				{
					Type: llm.PartToolCall,
					ToolCall: &llm.ToolCall{
						ID:        "call_123",
						Name:      "shell",
						Arguments: args,
					},
				},
			},
			TextContent: "Let me check the files.",
		},
		{
			Role: llm.RoleTool,
			Parts: []llm.Part{
				{
					Type: llm.PartToolResult,
					ToolResult: &llm.ToolResult{
						ID:      "call_123",
						Name:    "shell",
						Content: "file1.txt\nfile2.txt",
					},
				},
			},
		},
	}

	result := ExportToMarkdown(sess, messages, ExportOptions{})

	// Check tool call is rendered
	if !strings.Contains(result, "<summary>shell</summary>") {
		t.Error("expected tool call summary")
	}
	if !strings.Contains(result, `"command": "ls -la"`) {
		t.Error("expected tool call arguments")
	}
	if !strings.Contains(result, "**Result:**") {
		t.Error("expected tool result")
	}
	if !strings.Contains(result, "file1.txt") {
		t.Error("expected tool result content")
	}

	// Verify no duplicate tool calls (the tool should only appear once, with result)
	count := strings.Count(result, "<summary>shell</summary>")
	if count != 1 {
		t.Errorf("expected exactly 1 tool call block, got %d", count)
	}
}

func TestExportToMarkdown_TableEscaping(t *testing.T) {
	sess := &Session{
		ID:        "abc123",
		Provider:  "provider|with|pipes",
		Model:     "model\nwith\nnewlines",
		Agent:     "agent|test",
		CWD:       "/path/with|pipe",
		CreatedAt: time.Now(),
	}

	result := ExportToMarkdown(sess, nil, ExportOptions{})

	// Check pipes are escaped
	if strings.Contains(result, "provider|with") && !strings.Contains(result, `provider\|with`) {
		t.Error("pipes in provider should be escaped")
	}

	// Check newlines are replaced
	if strings.Contains(result, "model\nwith") {
		t.Error("newlines in model should be replaced with spaces")
	}
}

func TestExportToMarkdown_ShortID(t *testing.T) {
	// Use a properly formatted session ID (YYYYMMDD-HHMMSS-random)
	sess := &Session{
		ID:        "20240115-143052-a1b2c3",
		Name:      "", // No name, should use short ID
		Provider:  "anthropic",
		Model:     "claude",
		CreatedAt: time.Now(),
	}

	result := ExportToMarkdown(sess, nil, ExportOptions{})

	// ShortID returns "YYMMDD-HHMM" format (240115-1430)
	if !strings.Contains(result, "# Session: 240115-1430") {
		t.Errorf("expected short ID in title when name is empty, got: %s", result[:100])
	}
}

func TestFormatCount(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{0, "0"},
		{500, "500"},
		{999, "999"},
		{1000, "1K"},
		{1500, "1.5K"},
		{2000, "2K"},
		{10000, "10K"},
		{100000, "100K"},
		{1000000, "1M"},
		{1500000, "1.5M"},
		{2000000, "2M"},
	}

	for _, tt := range tests {
		result := formatCount(tt.input)
		if result != tt.expected {
			t.Errorf("formatCount(%d) = %s, want %s", tt.input, result, tt.expected)
		}
	}
}

func TestFormatTokens(t *testing.T) {
	tests := []struct {
		input, output int
		expected      string
	}{
		{0, 0, "-"},
		{1000, 500, "1K in / 500 out"},
		{45000, 12000, "45K in / 12K out"},
	}

	for _, tt := range tests {
		result := formatTokens(tt.input, tt.output)
		if result != tt.expected {
			t.Errorf("formatTokens(%d, %d) = %s, want %s", tt.input, tt.output, result, tt.expected)
		}
	}
}

func TestEscapeTableCell(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"normal text", "normal text"},
		{"text|with|pipes", `text\|with\|pipes`},
		{"text\nwith\nnewlines", "text with newlines"},
		{"mixed|pipe\nand\nnewline", `mixed\|pipe and newline`},
	}

	for _, tt := range tests {
		result := escapeTableCell(tt.input)
		if result != tt.expected {
			t.Errorf("escapeTableCell(%q) = %q, want %q", tt.input, result, tt.expected)
		}
	}
}
