package inspector

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
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

func TestInspectorRendersToolDiscoveryDiagnostics(t *testing.T) {
	diagnostics := &llm.ToolDiscoveryDiagnostics{
		ConfiguredMode: "auto", ResolvedMode: "deferred", ConfiguredStrategy: "auto", Strategy: "native",
		Reason: "42 MCP tools exceed configured threshold 24", StrategyReason: "exact verified support", PinnedCount: 2, ActiveMCPCount: 3,
		DeferredCount: 37, PinnedTokens: 100, ActiveMCPTokens: 200, DeferredTokens: 9000,
		DynamicActive: 3, DynamicLimit: 16,
		Recent: []llm.ToolActivationDiagnostic{{Name: "github__merge_pull_request", Reason: "search: merge pull request"}},
	}
	m := NewWithConfig([]session.Message{{Role: llm.RoleUser, TextContent: "hello"}}, 100, 40, ui.DefaultStyles(), nil, &Config{ToolDiscovery: diagnostics})
	content := strings.Join(m.contentLines, "\n")
	for _, want := range []string{"Tool Discovery", "auto / deferred", "auto / native", "exact verified support", "42 MCP tools exceed configured threshold 24", "37 tools, ~9000 tokens avoided", "github__merge_pull_request"} {
		if !strings.Contains(content, want) {
			t.Fatalf("inspector diagnostics missing %q:\n%s", want, content)
		}
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
	m, _ = m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	if m.scrollY != 1 {
		t.Errorf("expected scrollY 1 after scrolling down, got %d", m.scrollY)
	}

	// Scroll up
	m, _ = m.Update(tea.KeyPressMsg{Code: 'k', Text: "k"})
	if m.scrollY != 0 {
		t.Errorf("expected scrollY 0 after scrolling up, got %d", m.scrollY)
	}

	// Go to bottom
	m, _ = m.Update(tea.KeyPressMsg{Code: 'G', Text: "G"})
	if m.scrollY != m.maxScroll() {
		t.Errorf("expected scrollY %d (maxScroll) after G, got %d", m.maxScroll(), m.scrollY)
	}

	// Go to top
	m, _ = m.Update(tea.KeyPressMsg{Code: 'g', Text: "g"})
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
	_, cmd := m.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
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
	view := m.View().Content

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

func TestViewMarksCompactionBoundaryAndExpandsCompactionStory(t *testing.T) {
	body := "[Context Compaction]\nInternal context only; not a user command.\n\n<PREVIOUS_TURNS>\nimportant debug transcript\n</PREVIOUS_TURNS>"
	messages := []session.Message{
		{
			ID:          1,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "before compaction"}},
			TextContent: "before compaction",
			Sequence:    0,
		},
		{
			ID:          2,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: body}},
			TextContent: body,
			Sequence:    1,
		},
		{
			ID:             3,
			Role:           llm.RoleAssistant,
			Parts:          []llm.Part{{Type: llm.PartText, Text: "retained assistant turn"}},
			TextContent:    "retained assistant turn",
			Sequence:       2,
			CompactionTail: true,
		},
		{
			ID:             4,
			Role:           llm.RoleUser,
			Parts:          []llm.Part{{Type: llm.PartText, Text: "retained user turn"}},
			TextContent:    "retained user turn",
			Sequence:       3,
			CompactionTail: true,
		},
	}
	cfg := &Config{HasCompactionBoundary: true, CompactionBoundaryIndex: 1, CompactionBoundarySeq: 1, CompactionCount: 1}

	m := NewWithConfig(messages, 120, 40, ui.DefaultStyles(), nil, cfg)
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "Context compaction #1") || !strings.Contains(view, "active context starts here") {
		t.Fatalf("inspector should mark the compaction boundary, got:\n%s", view)
	}
	if !strings.Contains(view, "Compaction Debug #1") || !strings.Contains(view, "Retained raw tail: 2 messages") {
		t.Fatalf("inspector should show a collapsed compaction debug block, got:\n%s", view)
	}
	if strings.Contains(view, "important debug transcript") || strings.Contains(view, "retained user turn") {
		t.Fatalf("collapsed compaction block should hide full summary/tail until expanded, got:\n%s", view)
	}

	m, _ = m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	expanded := ui.StripANSI(m.View().Content)
	for _, want := range []string{"[Context Compaction]", "important debug transcript", "Retained raw tail", "retained user turn", "compaction_tail=true"} {
		if !strings.Contains(expanded, want) {
			t.Fatalf("expanded inspector compaction story missing %q, got:\n%s", want, expanded)
		}
	}
}

func TestExpandAllWorksAtViewportBottom(t *testing.T) {
	body := "[Context Compaction]\nInternal context only; not a user command.\n\n<PREVIOUS_TURNS>\nimportant debug transcript\n</PREVIOUS_TURNS>"
	var messages []session.Message
	messages = append(messages, session.Message{
		ID:          1,
		Role:        llm.RoleSystem,
		Parts:       []llm.Part{{Type: llm.PartText, Text: "system prompt used by the real inspector path"}},
		TextContent: "system prompt used by the real inspector path",
		Sequence:    0,
	})
	for i := 0; i < 24; i++ {
		text := fmt.Sprintf("older filler message %02d\n%s", i, strings.Repeat("filler line\n", 3))
		messages = append(messages, session.Message{
			ID:          int64(i + 2),
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
			TextContent: text,
			Sequence:    i + 1,
		})
	}
	summaryIdx := len(messages)
	messages = append(messages,
		session.Message{
			ID:          100,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: body}},
			TextContent: body,
			Sequence:    summaryIdx,
		},
		session.Message{
			ID:             101,
			Role:           llm.RoleAssistant,
			Parts:          []llm.Part{{Type: llm.PartText, Text: "retained assistant tail"}},
			TextContent:    "retained assistant tail",
			Sequence:       summaryIdx + 1,
			CompactionTail: true,
		},
	)
	cfg := &Config{
		ProviderName:            "anthropic",
		ModelName:               "claude-opus",
		ToolSpecs:               []llm.ToolSpec{{Name: "debug_tool", Description: "debug tool", Schema: map[string]interface{}{"type": "object"}}},
		HasCompactionBoundary:   true,
		CompactionBoundaryIndex: summaryIdx,
		CompactionBoundarySeq:   summaryIdx,
		CompactionCount:         1,
	}

	m := NewWithConfig(messages, 120, 20, ui.DefaultStyles(), nil, cfg)
	m.scrollY = m.maxScroll()
	collapsed := ui.StripANSI(m.View().Content)
	if !strings.Contains(collapsed, "Compaction Debug #1") || !strings.Contains(collapsed, "Retained raw tail: 1 message") {
		t.Fatalf("precondition expected collapsed compaction debug block at bottom, got:\n%s", collapsed)
	}
	if strings.Contains(collapsed, "important debug transcript") {
		t.Fatalf("collapsed compaction block should not show full summary, got:\n%s", collapsed)
	}

	truncatedBefore := 0
	for _, item := range m.items {
		if item.IsTruncated {
			truncatedBefore++
		}
	}
	if truncatedBefore < 2 {
		t.Fatalf("precondition expected multiple collapsed inspector items before e, got %d: %#v", truncatedBefore, m.items)
	}

	oldScrollY := m.scrollY
	m, _ = m.Update(tea.KeyPressMsg{Code: 'e', Text: "e"})
	expanded := ui.StripANSI(m.View().Content)
	if !strings.Contains(expanded, "Compaction Debug #1 (expanded)") || strings.Contains(expanded, "Boundary: active model context starts at this summary") {
		t.Fatalf("pressing e should expand the compaction debug block, got:\n%s", expanded)
	}
	if m.scrollY != oldScrollY {
		t.Fatalf("expand all should preserve scroll position, got %d want %d", m.scrollY, oldScrollY)
	}
	for _, item := range m.items {
		if item.IsTruncated {
			t.Fatalf("pressing e should expand every inspector item; still truncated: %#v", item)
		}
	}
}

func TestViewMarksCompactionBoundaryBeforePlainActiveMessage(t *testing.T) {
	messages := []session.Message{
		{
			ID:          1,
			Role:        llm.RoleUser,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "older scrollback"}},
			TextContent: "older scrollback",
			Sequence:    3,
		},
		{
			ID:          2,
			Role:        llm.RoleAssistant,
			Parts:       []llm.Part{{Type: llm.PartText, Text: "active answer"}},
			TextContent: "active answer",
			Sequence:    4,
		},
	}
	cfg := &Config{HasCompactionBoundary: true, CompactionBoundaryIndex: 0, CompactionBoundarySeq: 4, CompactionCount: 2}

	m := NewWithConfig(messages, 120, 40, ui.DefaultStyles(), nil, cfg)
	view := ui.StripANSI(m.View().Content)
	older := strings.Index(view, "older scrollback")
	boundary := strings.Index(view, "Context compaction #2")
	active := strings.Index(view, "active answer")
	if older < 0 || boundary < 0 || active < 0 || older >= boundary || boundary >= active {
		t.Fatalf("inspector should resolve sequence and mark boundary before plain active message, got:\n%s", view)
	}
	if strings.Contains(view, "full compacted summary below") {
		t.Fatalf("plain boundary marker should not claim a summary follows, got:\n%s", view)
	}
}

func TestView_EnablesMouseModeForWheelScroll(t *testing.T) {
	m := New(nil, 80, 24, ui.DefaultStyles())
	view := m.View()

	if view.MouseMode != tea.MouseModeCellMotion {
		t.Fatalf("expected inspector view mouse mode %v, got %v", tea.MouseModeCellMotion, view.MouseMode)
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
	view := m.View().Content

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
	view := m.View().Content

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

func TestViewWithReasoningSummary(t *testing.T) {
	messages := []session.Message{{
		ID:        1,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:                  llm.PartText,
			Text:                  "Final answer.",
			ReasoningContent:      "**Inspecting files**\n\nI checked the renderer.",
			ReasoningKind:         llm.ReasoningKindSummary,
			ReasoningSummaryTitle: "Inspecting files",
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	m := New(messages, 100, 24, ui.DefaultStyles())
	view := ui.StripANSI(m.View().Content)
	if !contains(view, "Thought: Inspecting files") {
		t.Fatalf("inspector should include reasoning summary header, got:\n%s", view)
	}
	if !contains(view, "I checked the renderer.") {
		t.Fatalf("inspector should include reasoning summary body, got:\n%s", view)
	}
	if !contains(view, "Final answer.") {
		t.Fatalf("inspector should still include assistant answer, got:\n%s", view)
	}
}

func TestViewWithReasoningRawVisibleByDefault(t *testing.T) {
	messages := []session.Message{{
		ID:        1,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Final answer.",
			ReasoningContent: "raw qwen thinking chain",
			ReasoningKind:    llm.ReasoningKindRaw,
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	m := New(messages, 100, 24, ui.DefaultStyles())
	view := ui.StripANSI(m.View().Content)
	if !contains(view, "Thinking...") {
		t.Fatalf("inspector should include raw reasoning header by default, got:\n%s", view)
	}
	if !contains(view, "raw qwen thinking chain") {
		t.Fatalf("inspector should include raw reasoning body by default, got:\n%s", view)
	}
	if contains(view, "Raw thinking") {
		t.Fatalf("inspector should render raw reasoning as a normal thought, got:\n%s", view)
	}
	if !contains(view, "Final answer.") {
		t.Fatalf("inspector should still include assistant answer, got:\n%s", view)
	}
}

func TestViewWithReasoningUnknownVisibleInInspector(t *testing.T) {
	messages := []session.Message{{
		ID:        1,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Final answer.",
			ReasoningContent: "provider thought without classification",
			ReasoningKind:    llm.ReasoningKindUnknown,
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	m := New(messages, 100, 24, ui.DefaultStyles())
	view := ui.StripANSI(m.View().Content)
	if !contains(view, "Thinking...") || !contains(view, "provider thought without classification") {
		t.Fatalf("inspector should include non-encrypted reasoning even when kind is unknown, got:\n%s", view)
	}
}

func TestViewWithReasoningEncryptedHidden(t *testing.T) {
	messages := []session.Message{{
		ID:        1,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:             llm.PartText,
			Text:             "Final answer.",
			ReasoningContent: "encrypted thought plaintext should not appear",
			ReasoningKind:    llm.ReasoningKindEncrypted,
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	m := New(messages, 100, 24, ui.DefaultStyles())
	view := ui.StripANSI(m.View().Content)
	if contains(view, "encrypted thought plaintext should not appear") || contains(view, "Thinking...") {
		t.Fatalf("inspector should hide encrypted reasoning, got:\n%s", view)
	}
	if !contains(view, "Final answer.") {
		t.Fatalf("inspector should still include assistant answer, got:\n%s", view)
	}
}

func TestViewWithReasoningRawUsesProviderTitle(t *testing.T) {
	messages := []session.Message{{
		ID:        1,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:                  llm.PartText,
			Text:                  "Final answer.",
			ReasoningContent:      "raw visible chain",
			ReasoningKind:         llm.ReasoningKindRaw,
			ReasoningSummaryTitle: "Exploring options",
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	m := New(messages, 100, 24, ui.DefaultStyles())
	view := ui.StripANSI(m.View().Content)
	if !contains(view, "Thought: Exploring options") {
		t.Fatalf("inspector should include provider raw reasoning title, got:\n%s", view)
	}
	if !contains(view, "raw visible chain") {
		t.Fatalf("inspector should include raw reasoning body, got:\n%s", view)
	}
}

func TestViewWithReasoningSummaryWrapsLongThoughtText(t *testing.T) {
	longBody := "This reasoning summary is intentionally long so it must wrap inside the inspector box instead of being clipped at the right edge of the terminal viewport."
	messages := []session.Message{{
		ID:        1,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:                  llm.PartText,
			Text:                  "Final answer.",
			ReasoningContent:      "**Analyzing wrapping**\n\n" + longBody,
			ReasoningKind:         llm.ReasoningKindSummary,
			ReasoningSummaryTitle: "Analyzing wrapping",
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	m := New(messages, 62, 24, ui.DefaultStyles())
	view := ui.StripANSI(m.View().Content)
	if !contains(view, "right edge") || !contains(view, "terminal viewport") {
		t.Fatalf("expected wrapped reasoning body to remain visible, got:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "This reasoning summary") && lipgloss.Width(line) > 62 {
			t.Fatalf("reasoning line was not wrapped to inspector width: %q", line)
		}
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
	view := m.View().Content

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
