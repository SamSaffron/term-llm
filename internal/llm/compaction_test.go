package llm

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		input    string
		expected int
	}{
		{"", 0},
		{"hi", 1},
		{"hello world", 3},          // 11 bytes → (11+3)/4 = 3
		{strings.Repeat("a", 4), 1}, // exactly 4 bytes → 1 token
		{strings.Repeat("a", 5), 2}, // 5 bytes → (5+3)/4 = 2
		{strings.Repeat("a", 8), 2}, // 8 bytes → 2 tokens
		{strings.Repeat("a", 100), 25},
	}

	for _, tt := range tests {
		got := EstimateTokens(tt.input)
		if got != tt.expected {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tt.input, got, tt.expected)
		}
	}
}

func TestEstimateMessageTokens(t *testing.T) {
	msgs := []Message{
		UserText("hello world"),        // 11 bytes → 3 tokens
		AssistantText("goodbye world"), // 13 bytes → 4 tokens (rounded up)
	}
	got := EstimateMessageTokens(msgs)
	// 11 bytes → (11+3)/4 = 3, 13 bytes → (13+3)/4 = 4
	if got != 7 {
		t.Errorf("EstimateMessageTokens = %d, want 7", got)
	}
}

func TestEstimateMessageTokensWithToolParts(t *testing.T) {
	// Tool call arguments should be counted
	msgs := []Message{
		{
			Role: RoleAssistant,
			Parts: []Part{{
				Type:     PartToolCall,
				ToolCall: &ToolCall{Name: "read", Arguments: []byte(`{"path":"/foo"}`)},
			}},
		},
		{
			Role: RoleTool,
			Parts: []Part{{
				Type:       PartToolResult,
				ToolResult: &ToolResult{Name: "read", Content: strings.Repeat("x", 40)},
			}},
		},
	}
	got := EstimateMessageTokens(msgs)
	// args: 15 bytes → 4 tokens, content: 40 bytes → 10 tokens
	want := EstimateTokens(`{"path":"/foo"}`) + EstimateTokens(strings.Repeat("x", 40))
	if got != want {
		t.Errorf("EstimateMessageTokens with tools = %d, want %d", got, want)
	}
}

func TestEstimateMessageTokensEmpty(t *testing.T) {
	if got := EstimateMessageTokens(nil); got != 0 {
		t.Errorf("EstimateMessageTokens(nil) = %d, want 0", got)
	}
	if got := EstimateMessageTokens([]Message{}); got != 0 {
		t.Errorf("EstimateMessageTokens([]) = %d, want 0", got)
	}
}

func TestExtractRecentUserMessages(t *testing.T) {
	messages := []Message{
		UserText("first user message"), // ~5 tokens
		AssistantText("first response"),
		UserText("second user message"), // ~5 tokens
		AssistantText("second response"),
		UserText("third user message"), // ~5 tokens
	}

	// Budget large enough for all user messages
	result := extractRecentUserMessages(messages, 1000)
	if len(result) != 3 {
		t.Errorf("expected 3 user messages with large budget, got %d", len(result))
	}

	// Budget for only the last user message
	result = extractRecentUserMessages(messages, 5)
	if len(result) != 1 {
		t.Errorf("expected 1 user message with small budget, got %d", len(result))
	}
	if result[0].Parts[0].Text != "third user message" {
		t.Errorf("expected last user message, got %q", result[0].Parts[0].Text)
	}

	// Messages should be in chronological order
	result = extractRecentUserMessages(messages, 100)
	if len(result) < 2 {
		t.Fatalf("expected at least 2 messages, got %d", len(result))
	}
	if result[0].Parts[0].Text != "first user message" {
		t.Errorf("first result should be 'first user message', got %q", result[0].Parts[0].Text)
	}
}

func TestExtractRecentUserMessagesEmpty(t *testing.T) {
	result := extractRecentUserMessages(nil, 1000)
	if len(result) != 0 {
		t.Errorf("expected 0 messages from nil input, got %d", len(result))
	}

	// No user messages
	messages := []Message{
		AssistantText("just an assistant message"),
	}
	result = extractRecentUserMessages(messages, 1000)
	if len(result) != 0 {
		t.Errorf("expected 0 user messages, got %d", len(result))
	}
}

func TestReconstructHistory(t *testing.T) {
	recentUser := []Message{UserText("recent question")}

	result := reconstructHistory("system prompt", "summary of conversation", recentUser)

	// Should be: system + summary + assistant ack + recent user
	if len(result) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(result))
	}

	if result[0].Role != RoleSystem {
		t.Errorf("first message should be system, got %s", result[0].Role)
	}
	if result[0].Parts[0].Text != "system prompt" {
		t.Errorf("system prompt mismatch")
	}

	if result[1].Role != RoleUser {
		t.Errorf("second message should be user (summary), got %s", result[1].Role)
	}
	if !strings.Contains(result[1].Parts[0].Text, "summary of conversation") {
		t.Errorf("summary message should contain summary text")
	}
	if !strings.Contains(result[1].Parts[0].Text, summaryPrefix) {
		t.Errorf("summary message should contain prefix")
	}

	if result[2].Role != RoleAssistant {
		t.Errorf("third message should be assistant ack, got %s", result[2].Role)
	}

	if result[3].Role != RoleUser {
		t.Errorf("fourth message should be user, got %s", result[3].Role)
	}
	if result[3].Parts[0].Text != "recent question" {
		t.Errorf("recent user message mismatch")
	}
}

func TestReconstructHistoryNoSystem(t *testing.T) {
	result := reconstructHistory("", "summary", []Message{UserText("q")})

	// Without system prompt: summary + ack + user = 3
	if len(result) != 3 {
		t.Fatalf("expected 3 messages without system prompt, got %d", len(result))
	}
	if result[0].Role != RoleUser {
		t.Errorf("first message should be user (summary), got %s", result[0].Role)
	}
}

func TestTruncateToolResult(t *testing.T) {
	t.Run("under limit", func(t *testing.T) {
		short := "hello"
		if got := TruncateToolResult(short, 100); got != short {
			t.Errorf("short content should not be truncated")
		}
	})

	t.Run("over limit", func(t *testing.T) {
		long := strings.Repeat("x", 1000)
		result := TruncateToolResult(long, 100)
		if len(result) >= len(long) {
			t.Errorf("truncated result should be shorter than original")
		}
		if !strings.Contains(result, "900 chars truncated") {
			t.Errorf("truncated result should report 900 chars truncated, got: %s", result)
		}
		if !strings.Contains(result, "1 lines") {
			t.Errorf("single-line truncated middle should report 1 line, got: %s", result)
		}
		// First 50 and last 50 chars should be preserved
		if !strings.HasPrefix(result, strings.Repeat("x", 50)) {
			t.Errorf("truncated result should preserve first half")
		}
		if !strings.HasSuffix(result, strings.Repeat("x", 50)) {
			t.Errorf("truncated result should preserve last half")
		}
	})

	t.Run("odd limit", func(t *testing.T) {
		// With odd limit 101: head=50, tail=51
		content := strings.Repeat("a", 50) + strings.Repeat("b", 51) + strings.Repeat("c", 99)
		result := TruncateToolResult(content, 101)
		runes := []rune(result)
		// Head should be 50 'a's, tail should be 51 'c's (last 51 of the 99)
		headPart := string(runes[:50])
		if headPart != strings.Repeat("a", 50) {
			t.Errorf("head should be 50 'a's, got %q", headPart)
		}
		tailPart := string(runes[len(runes)-51:])
		if tailPart != strings.Repeat("c", 51) {
			t.Errorf("tail should be 51 'c's, got %q", tailPart)
		}
	})

	t.Run("line count accuracy", func(t *testing.T) {
		// Create content with known line structure in the middle
		head := strings.Repeat("H", 50)
		middle := "line1\nline2\nline3\n" + strings.Repeat("x", 100)
		tail := strings.Repeat("T", 50)
		content := head + middle + tail
		result := TruncateToolResult(content, 100)
		// Middle has 3 newlines → 4 lines
		if !strings.Contains(result, "4 lines") {
			t.Errorf("expected 4 lines in truncation marker, got: %s", result)
		}
	})

	t.Run("multibyte UTF-8", func(t *testing.T) {
		// Each emoji is a multi-byte rune; ensure we don't split them
		content := strings.Repeat("\U0001f600", 200) // 200 smiley faces (4 bytes each)
		result := TruncateToolResult(content, 100)
		// Should contain truncation marker
		if !strings.Contains(result, "chars truncated") {
			t.Errorf("should truncate multi-byte content")
		}
		// Head and tail should be valid UTF-8 with intact runes
		runes := []rune(result)
		// First 50 runes should be smiley faces
		for i := 0; i < 50; i++ {
			if runes[i] != '\U0001f600' {
				t.Errorf("rune %d in head should be smiley, got %U", i, runes[i])
				break
			}
		}
		// Last 50 runes should be smiley faces
		for i := len(runes) - 50; i < len(runes); i++ {
			if runes[i] != '\U0001f600' {
				t.Errorf("rune %d in tail should be smiley, got %U", i, runes[i])
				break
			}
		}
	})

	t.Run("shell exit_code preserved in tail", func(t *testing.T) {
		// Simulate shell output: large body + exit_code on last line
		body := strings.Repeat("output line\n", 2000)
		content := body + "exit_code: 0"
		result := TruncateToolResult(content, 1000)
		if !strings.HasSuffix(result, "exit_code: 0") {
			t.Errorf("exit_code should be preserved in tail, result ends with: %q",
				result[len(result)-50:])
		}
	})

	t.Run("empty string", func(t *testing.T) {
		if got := TruncateToolResult("", 100); got != "" {
			t.Errorf("empty string should return empty, got %q", got)
		}
	})

	t.Run("exact boundary", func(t *testing.T) {
		exact := strings.Repeat("a", 100)
		if got := TruncateToolResult(exact, 100); got != exact {
			t.Errorf("content exactly at limit should not be truncated")
		}
	})
}

func TestIsContextOverflowError(t *testing.T) {
	tests := []struct {
		err      error
		expected bool
	}{
		{nil, false},
		{fmt.Errorf("network timeout"), false},
		{fmt.Errorf("max_tokens must be at most 4096"), false}, // output token config error, not context overflow
		{fmt.Errorf("context length exceeded"), true},
		{fmt.Errorf("maximum context length is 128000"), true},
		{fmt.Errorf("context_length_exceeded"), true},
		{fmt.Errorf("too many tokens in request"), true},
		{fmt.Errorf("Request Too Large"), true},
		{fmt.Errorf("prompt is too long"), true},
		{fmt.Errorf("the input is too long for this model"), true},
		{fmt.Errorf("exceeds the model's maximum context"), true},
	}

	for _, tt := range tests {
		got := isContextOverflowError(tt.err)
		if got != tt.expected {
			errStr := "<nil>"
			if tt.err != nil {
				errStr = tt.err.Error()
			}
			t.Errorf("isContextOverflowError(%q) = %v, want %v", errStr, got, tt.expected)
		}
	}
}

func TestInputLimitForModel(t *testing.T) {
	tests := []struct {
		model    string
		expected int
	}{
		// Values are effective input limits (not total context)
		{"claude-sonnet-4-5-20250929", 136_000}, // 200K - 64K
		{"claude-sonnet-4-20250514", 136_000},   // 200K - 64K
		{"claude-opus-4-20250514", 168_000},     // 200K - 32K
		{"claude-3.5-sonnet-20241022", 192_000}, // 200K - 8K
		{"gpt-4o-2024-05-13", 112_000},          // 128K - 16K
		{"gpt-4.1-2025-04-14", 1_014_808},       // 1047K - 32K
		{"gpt-4", 8_192},
		{"gpt-4-32k", 32_768},
		{"gpt-5.3-codex", 272_000},       // explicit input=272K
		{"gpt-5.3-codex-spark", 100_000}, // explicit input=100K
		{"gpt-5.2-codex", 272_000},
		{"gpt-5.2-chat-latest", 112_000}, // 128K - 16K
		{"gpt-5.1", 272_000},
		{"gpt-5.1-chat-latest", 112_000},
		{"gpt-5", 272_000},
		{"gpt-5-mini", 272_000},
		{"o1-mini", 62_000},                 // 128K - 65K
		{"o1-pro", 100_000},                 // 200K - 100K
		{"o3-mini", 100_000},                // 200K - 100K
		{"gpt-4-turbo-2024-04-09", 124_000}, // 128K - 4K
		{"gpt-3.5-turbo-0125", 12_000},      // 16K - 4K
		{"gemini-2.5-pro-latest", 983_000},  // 1M - 65K
		{"gemini-3-pro-preview", 936_000},   // 1M - 64K
		{"gemini-3-flash-preview", 983_000}, // 1M - 65K
		{"grok-4-1-fast", 1_970_000},        // 2M - 30K
		{"grok-3-mini-fast", 123_000},       // 131K - 8K
		{"deepseek-chat", 128_000},
		{"unknown-model-xyz", 0},
		{"", 0},
		// Case insensitivity
		{"GPT-5", 272_000},
		{"Claude-Sonnet-4-5-20250929", 136_000},
	}

	for _, tt := range tests {
		got := InputLimitForModel(tt.model)
		if got != tt.expected {
			t.Errorf("InputLimitForModel(%q) = %d, want %d", tt.model, got, tt.expected)
		}
	}
}

func TestInputLimitForProviderModel(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		expected int
	}{
		// Copilot-specific effective input limits
		{"copilot", "gpt-5.3-codex", 272_000}, // copilot: 400K ctx, input=272K
		{"copilot", "gpt-5.2-codex", 144_000}, // copilot: 272K - 128K
		{"copilot", "gpt-5.2", 64_000},        // copilot: 128K - 64K
		{"copilot", "gpt-5.1-codex", 64_000},  // copilot: 128K ctx
		{"copilot", "gpt-5.1", 64_000},        // copilot: 128K - 64K
		{"copilot", "gpt-5", 64_000},          // copilot: 128K ctx
		{"copilot", "gpt-4.1", 48_000},        // copilot: 64K - 16K
		{"copilot", "gpt-4o", 48_000},         // copilot: 64K - 16K
		// Copilot falls back to canonical for unknown models
		{"copilot", "gpt-3.5-turbo", 12_000},
		// OpenAI direct uses canonical input limits
		{"openai", "gpt-5.2-codex", 272_000},
		{"openai", "gpt-5", 272_000},
		{"openai", "gpt-4.1", 1_014_808},
		// ChatGPT uses canonical input limits
		{"chatgpt", "gpt-5.2-codex", 272_000},
		// Unknown provider falls back to canonical
		{"", "gpt-5", 272_000},
		{"unknown", "gpt-5", 272_000},
	}

	for _, tt := range tests {
		got := InputLimitForProviderModel(tt.provider, tt.model)
		if got != tt.expected {
			t.Errorf("InputLimitForProviderModel(%q, %q) = %d, want %d", tt.provider, tt.model, got, tt.expected)
		}
	}
}

func TestFormatTokenCount(t *testing.T) {
	tests := []struct {
		tokens   int
		expected string
	}{
		{0, ""},
		{-1, ""},
		{128_000, "128K"},
		{200_000, "200K"},
		{400_000, "400K"},
		{1_047_576, "1M"},
		{1_048_576, "1M"},
		{2_097_152, "2.1M"},
		{2_000_000, "2M"},
		{16_385, "16K"},
		{8_192, "8K"},
		{32_768, "33K"},
		{131_072, "131K"},
		{256_000, "256K"},
	}

	for _, tt := range tests {
		got := FormatTokenCount(tt.tokens)
		if got != tt.expected {
			t.Errorf("FormatTokenCount(%d) = %q, want %q", tt.tokens, got, tt.expected)
		}
	}
}

func TestCompactEndToEnd(t *testing.T) {
	// Set up mock provider that returns a summary
	provider := NewMockProvider("test")
	provider.AddTextResponse("## Summary\nUser was debugging a Go test. Fixed the nil pointer in main.go:42.")

	messages := []Message{
		UserText("Help me debug this Go test"),
		AssistantText("I'll look at the test file."),
		UserText("The test is in main_test.go"),
		AssistantText("I see the issue - nil pointer at line 42."),
		UserText("Can you fix it?"),
		AssistantText("Fixed the nil pointer by adding a nil check."),
	}

	config := DefaultCompactionConfig()
	result, err := Compact(context.Background(), provider, "test-model", "You are a helpful assistant.", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	if result.Summary == "" {
		t.Error("summary should not be empty")
	}
	if result.OriginalCount != 6 {
		t.Errorf("original count = %d, want 6", result.OriginalCount)
	}
	if len(result.NewMessages) == 0 {
		t.Error("new messages should not be empty")
	}

	// First message should be system
	if result.NewMessages[0].Role != RoleSystem {
		t.Errorf("first message should be system, got %s", result.NewMessages[0].Role)
	}

	// Second should be user with summary
	if result.NewMessages[1].Role != RoleUser {
		t.Errorf("second message should be user (summary), got %s", result.NewMessages[1].Role)
	}
	if !strings.Contains(result.NewMessages[1].Parts[0].Text, "Summary") {
		t.Errorf("summary message should contain the summary text")
	}
}

func TestCompactEmptyMessages(t *testing.T) {
	provider := NewMockProvider("test")
	config := DefaultCompactionConfig()

	_, err := Compact(context.Background(), provider, "test-model", "", nil, config)
	if err == nil {
		t.Error("Compact with nil messages should return error")
	}
}

func TestCompactProviderError(t *testing.T) {
	// When the provider stream errors, Compact should return an error
	// (either the stream error or "empty summary" if the stream closes cleanly)
	provider := NewMockProvider("test")
	provider.AddTurn(MockTurn{Error: fmt.Errorf("rate limited")})
	config := DefaultCompactionConfig()

	_, err := Compact(context.Background(), provider, "test-model", "", []Message{UserText("hi")}, config)
	if err == nil {
		t.Error("Compact should return error when provider fails")
	}
}

func TestCompactProviderNoTurns(t *testing.T) {
	// When provider has no turns configured, Stream itself returns an error
	provider := NewMockProvider("test")
	config := DefaultCompactionConfig()

	_, err := Compact(context.Background(), provider, "test-model", "", []Message{UserText("hi")}, config)
	if err == nil {
		t.Error("Compact should return error when provider has no turns")
	}
	if !strings.Contains(err.Error(), "stream failed") {
		t.Errorf("expected stream failed error, got: %v", err)
	}
}

func TestCompactNoSystemPrompt(t *testing.T) {
	provider := NewMockProvider("test")
	provider.AddTextResponse("Summary of conversation.")

	messages := []Message{
		UserText("question"),
		AssistantText("answer"),
	}

	config := DefaultCompactionConfig()
	result, err := Compact(context.Background(), provider, "test-model", "", messages, config)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}

	// Without system prompt: summary + ack + recent user = 3
	if result.NewMessages[0].Role != RoleUser {
		t.Errorf("first message should be user (summary) when no system prompt, got %s", result.NewMessages[0].Role)
	}
}

func TestDefaultCompactionConfig(t *testing.T) {
	config := DefaultCompactionConfig()
	if config.ThresholdRatio != 0.80 {
		t.Errorf("ThresholdRatio = %f, want 0.80", config.ThresholdRatio)
	}
	if config.RecentUserTokenBudget != 20_000 {
		t.Errorf("RecentUserTokenBudget = %d, want 20000", config.RecentUserTokenBudget)
	}
	if config.MaxToolResultChars != 80_000 {
		t.Errorf("MaxToolResultChars = %d, want 80000", config.MaxToolResultChars)
	}
}

func TestEstimatedTokens(t *testing.T) {
	e := NewEngine(NewMockProvider("test"), nil)

	msgs := []Message{
		SystemText("system prompt"),
		UserText("hello world"),
	}

	// With no API data yet, should fall back to pure heuristic
	got := e.estimatedTokens(msgs)
	want := EstimateMessageTokens(msgs)
	if got != want {
		t.Errorf("estimatedTokens (no API data) = %d, want %d", got, want)
	}

	// Simulate API response: 100 input + 50 output, with 2 messages at call time
	e.lastTotalTokens = 150
	e.lastMessageCount = 2

	// Now add an assistant response and a new user message after the API call
	msgs = append(msgs, AssistantText("response from model"))
	msgs = append(msgs, UserText("follow-up question"))

	got = e.estimatedTokens(msgs)
	// Should be: lastTotalTokens + estimate(msgs[2:])
	newMsgsEstimate := EstimateMessageTokens(msgs[2:])
	want = 150 + newMsgsEstimate
	if got != want {
		t.Errorf("estimatedTokens (with API data) = %d, want %d (150 + %d)", got, want, newMsgsEstimate)
	}
}

func TestEstimatedTokensFallback(t *testing.T) {
	e := NewEngine(NewMockProvider("test"), nil)

	// Edge case: lastMessageCount >= len(messages) — should fall back
	e.lastTotalTokens = 100
	e.lastMessageCount = 5

	msgs := []Message{UserText("short")}
	got := e.estimatedTokens(msgs)
	want := EstimateMessageTokens(msgs)
	if got != want {
		t.Errorf("estimatedTokens (stale data) = %d, want fallback %d", got, want)
	}
}

func TestEffortVariantsFor(t *testing.T) {
	tests := []struct {
		model    string
		expected int // number of variants (0 = nil)
	}{
		{"gpt-5", 4},
		{"gpt-5.3-codex", 4},
		{"gpt-5-mini", 4},
		{"gpt-5.2-chat", 4},
		{"claude-sonnet-4-5", 0},
		{"gpt-4o", 0},
		{"o3-mini", 0},
		{"", 0},
	}

	for _, tt := range tests {
		got := EffortVariantsFor(tt.model)
		if len(got) != tt.expected {
			t.Errorf("EffortVariantsFor(%q) returned %d variants, want %d", tt.model, len(got), tt.expected)
		}
	}

	// Check the actual variant values
	variants := EffortVariantsFor("gpt-5")
	expected := []string{"low", "medium", "high", "xhigh"}
	for i, v := range variants {
		if v != expected[i] {
			t.Errorf("EffortVariantsFor variant[%d] = %q, want %q", i, v, expected[i])
		}
	}
}

func TestExpandWithEffortVariants(t *testing.T) {
	models := []string{"gpt-5", "claude-sonnet-4-5"}
	expanded := ExpandWithEffortVariants(models)

	// gpt-5 + 4 variants + claude-sonnet-4-5 (no variants) = 6
	if len(expanded) != 6 {
		t.Errorf("ExpandWithEffortVariants returned %d entries, want 6", len(expanded))
	}
	if expanded[0] != "gpt-5" {
		t.Errorf("first entry should be base model, got %q", expanded[0])
	}
	if expanded[1] != "gpt-5-low" {
		t.Errorf("second entry should be gpt-5-low, got %q", expanded[1])
	}
	if expanded[5] != "claude-sonnet-4-5" {
		t.Errorf("last entry should be claude-sonnet-4-5, got %q", expanded[5])
	}
}
