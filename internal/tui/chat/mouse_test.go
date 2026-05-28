package chat

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestMouseClickMovesCursorSingleLine(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("hello world")
	_ = m.View()

	clickX := m.textareaLeftX + m.textareaPromptWidth + 5
	clickY := m.textareaTopY

	_, _ = m.Update(tea.MouseClickMsg{
		X:      clickX,
		Y:      clickY,
		Button: tea.MouseLeft,
	})

	if got := m.textarea.Line(); got != 0 {
		t.Fatalf("line = %d, want 0", got)
	}
	if got := m.textarea.LineInfo().CharOffset; got != 5 {
		t.Fatalf("char offset = %d, want 5", got)
	}
}

func TestMouseClickMovesCursorWrappedLine(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 12
	m.textarea.SetWidth(12)
	m.setTextareaValue("abcdefghijk")
	_ = m.View()

	if m.textarea.Height() < 2 {
		t.Fatalf("expected wrapped textarea height >= 2, got %d", m.textarea.Height())
	}

	clickX := m.textareaLeftX + m.textareaPromptWidth + 2
	clickY := m.textareaTopY + 1

	_, _ = m.Update(tea.MouseClickMsg{
		X:      clickX,
		Y:      clickY,
		Button: tea.MouseLeft,
	})

	if got := m.textarea.Line(); got != 0 {
		t.Fatalf("line = %d, want 0", got)
	}
	if got := m.textarea.LineInfo().RowOffset; got != 1 {
		t.Fatalf("row offset = %d, want 1 for wrapped-line click", got)
	}
	if got := m.textarea.LineInfo().CharOffset; got == 0 {
		t.Fatalf("char offset = %d, want > 0 on wrapped row", got)
	}
}

func TestMouseShiftClickDoesNotMoveCursor(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("hello world")
	_ = m.View()
	m.textarea.CursorStart()

	clickX := m.textareaLeftX + m.textareaPromptWidth + 5
	clickY := m.textareaTopY

	_, _ = m.Update(tea.MouseClickMsg{
		X:      clickX,
		Y:      clickY,
		Mod:    tea.ModShift,
		Button: tea.MouseLeft,
	})

	if got := m.textarea.LineInfo().CharOffset; got != 0 {
		t.Fatalf("char offset = %d, want 0", got)
	}
}

func TestMouseClickMovesCursorInAltScreen(t *testing.T) {
	m := newTestChatModel(true)
	m.setTextareaValue("hello world")
	_ = m.View()

	clickX := m.textareaLeftX + m.textareaPromptWidth + 4
	clickY := m.textareaTopY

	_, _ = m.Update(tea.MouseClickMsg{
		X:      clickX,
		Y:      clickY,
		Button: tea.MouseLeft,
	})

	if got := m.textarea.LineInfo().CharOffset; got != 4 {
		t.Fatalf("char offset = %d, want 4", got)
	}
}

func TestMouseWheelScrollStillWorksInAltScreen(t *testing.T) {
	m := newTestChatModel(true)
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "line")
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	m.viewport.GotoTop()

	_, _ = m.Update(tea.MouseWheelMsg{
		X:      0,
		Y:      0,
		Button: tea.MouseWheelDown,
	})

	if m.viewport.YOffset() == 0 {
		t.Fatal("expected viewport to scroll on mouse wheel down")
	}
}

func TestMouseWheelScrollsContentDialogInsteadOfViewport(t *testing.T) {
	m := newTestChatModel(true)
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "line")
	}
	m.viewport.SetContent(strings.Join(lines, "\n"))
	m.viewport.GotoTop()
	m.dialog.ShowContent("Help", strings.Join(lines, "\n"))

	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})

	if m.dialog.contentScroll == 0 {
		t.Fatal("expected modal content to scroll on mouse wheel down")
	}
	if got := m.viewport.YOffset(); got != 0 {
		t.Fatalf("underlying viewport scrolled while modal was open: %d", got)
	}

	m.dialog.Close()
	_, _ = m.Update(tea.MouseWheelMsg{Button: tea.MouseWheelDown})
	if m.viewport.YOffset() == 0 {
		t.Fatal("expected viewport to scroll after modal closes")
	}
}

func TestHorizontalMouseWheelDoesNotShiftAltScreenViewport(t *testing.T) {
	m := newTestChatModel(true)
	m.viewport.SetContent(strings.Repeat("x", 200))
	m.viewport.SetXOffset(12)
	if m.viewport.XOffset() == 0 {
		t.Fatal("precondition: expected horizontal offset to be set")
	}

	_, _ = m.Update(tea.MouseWheelMsg{
		X:      0,
		Y:      0,
		Button: tea.MouseWheelRight,
	})
	if got := m.viewport.XOffset(); got != 0 {
		t.Fatalf("horizontal wheel left x-offset = %d, want 0", got)
	}

	m.viewport.SetXOffset(12)
	_, _ = m.Update(tea.MouseWheelMsg{
		X:      0,
		Y:      0,
		Button: tea.MouseWheelUp,
		Mod:    tea.ModShift,
	})
	if got := m.viewport.XOffset(); got != 0 {
		t.Fatalf("shift-wheel x-offset = %d, want 0", got)
	}
}

func TestChatDisableMouseEnvDisablesMouseReporting(t *testing.T) {
	t.Setenv(chatDisableMouseEnv, "1")
	m := newTestChatModel(true)

	if got := m.View().MouseMode; got != tea.MouseModeNone {
		t.Fatalf("MouseMode = %v, want MouseModeNone", got)
	}
}

func TestMiddleClickPasteWorksWhileStreaming(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true

	orig := readPrimarySelection
	readPrimarySelection = func() (string, error) { return "interject from primary", nil }
	t.Cleanup(func() { readPrimarySelection = orig })

	_, _ = m.Update(tea.MouseClickMsg{
		X:      0,
		Y:      0,
		Button: tea.MouseMiddle,
	})

	if got := m.textarea.Value(); got != "interject from primary" {
		t.Fatalf("textarea value = %q, want pasted primary selection", got)
	}
}

func TestMouseClickThinkingHeaderTogglesOnlyThatBlock(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = false
	m.setTextareaValue("")
	m.tracker = ui.NewToolTracker()

	first := llm.Part{
		Type:                  llm.PartText,
		ReasoningContent:      "first hidden body",
		ReasoningKind:         llm.ReasoningKindRaw,
		ReasoningSummaryTitle: "First plan",
	}
	second := llm.Part{
		Type:                  llm.PartText,
		ReasoningContent:      "second hidden body",
		ReasoningKind:         llm.ReasoningKindRaw,
		ReasoningSummaryTitle: "Second plan",
	}
	m.tracker.AddReasoningSegment(ui.NormalizeReasoningSegmentRendered(m.renderReasoningPartBlock(first)), reasoningSegmentFromPart(first))
	m.tracker.AddReasoningSegment(ui.NormalizeReasoningSegmentRendered(m.renderReasoningPartBlock(second)), reasoningSegmentFromPart(second))
	m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(m.tracker.CompletedSegments(), m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
	_ = m.View()
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}

	secondLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Second plan") {
			secondLine = i
			break
		}
	}
	if secondLine < 0 {
		t.Fatalf("could not find second collapsed thought header in %#v", m.contentLines)
	}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: secondLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	rendered := ui.StripANSI(m.viewCache.completedStream)
	if !strings.Contains(rendered, "▸ Thought: First plan") || strings.Contains(rendered, "first hidden body") {
		t.Fatalf("first thought should remain collapsed, got %q", rendered)
	}
	if !strings.Contains(rendered, "▾ Thought: Second plan") || !strings.Contains(rendered, "second hidden body") {
		t.Fatalf("clicked second thought should expand, got %q", rendered)
	}

	updated, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})
	m = updated.(*Model)
	rendered = ui.StripANSI(m.viewCache.completedStream)
	if !strings.Contains(rendered, "▾ Thought: First plan") || !strings.Contains(rendered, "first hidden body") ||
		!strings.Contains(rendered, "▾ Thought: Second plan") || !strings.Contains(rendered, "second hidden body") {
		t.Fatalf("ctrl+e should override per-block click state and expand all thoughts, got %q", rendered)
	}
}

func TestMouseClickCustomHiddenLabelThinkingHeaderTogglesBlock(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = false
	m.setTextareaValue("")
	m.tracker = ui.NewToolTracker()
	m.reasoningConfig = config.DefaultReasoningConfig()
	m.reasoningConfig.HiddenLabel = "Pondering..."
	if m.chatRenderer != nil {
		m.chatRenderer.SetReasoningConfig(m.reasoningConfig)
	}

	part := llm.Part{
		Type:             llm.PartText,
		ReasoningContent: "custom label body",
		ReasoningKind:    llm.ReasoningKindRaw,
	}
	m.tracker.AddReasoningSegment(ui.NormalizeReasoningSegmentRendered(m.renderReasoningPartBlock(part)), reasoningSegmentFromPart(part))
	m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(m.tracker.CompletedSegments(), m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
	_ = m.View()
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}

	headerLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Pondering...") {
			headerLine = i
			break
		}
	}
	if headerLine < 0 {
		t.Fatalf("could not find custom hidden label header in %#v", m.contentLines)
	}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	rendered := ui.StripANSI(m.viewCache.completedStream)
	if !strings.Contains(rendered, "▾ Pondering...") || !strings.Contains(rendered, "custom label body") {
		t.Fatalf("custom hidden label thought should expand on click, got %q", rendered)
	}
}

func TestMouseClickReloadedHistoryThinkingHeaderTogglesBlock(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = false
	m.setTextareaValue("")
	m.messages = []session.Message{{
		ID:        101,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:                  llm.PartText,
			Text:                  "Final answer.",
			ReasoningContent:      "persisted qwen thinking body",
			ReasoningKind:         llm.ReasoningKindRaw,
			ReasoningSummaryTitle: "Loaded plan",
		}},
		TextContent: "Final answer.",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}
	m.tracker = nil
	m.invalidateHistoryCache()
	_ = m.View()
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}

	headerLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Loaded plan") {
			headerLine = i
			break
		}
	}
	if headerLine < 0 {
		t.Fatalf("could not find collapsed persisted thought header in %#v", m.contentLines)
	}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▾ Thought: Loaded plan") || !strings.Contains(view, "persisted qwen thinking body") {
		t.Fatalf("click should expand persisted history reasoning block, got %q", view)
	}

	updated, _ = m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	view = ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▸ Thought: Loaded plan") || strings.Contains(view, "persisted qwen thinking body") {
		t.Fatalf("second click should collapse persisted history reasoning block, got %q", view)
	}
}

func TestMouseClickReloadedHistoryThoughtHeaderIgnoresPlainTextDecoys(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = false
	m.setTextareaValue("")
	m.tracker = nil
	m.messages = []session.Message{
		{
			ID:        201,
			SessionID: "test-session",
			Role:      llm.RoleAssistant,
			Parts: []llm.Part{{
				Type: llm.PartText,
				Text: "▸ Thought: this is ordinary assistant text, not a collapsible thought block.\n\nCarry on.",
			}},
			TextContent: "▸ Thought: this is ordinary assistant text, not a collapsible thought block.\n\nCarry on.",
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
		{
			ID:        202,
			SessionID: "test-session",
			Role:      llm.RoleAssistant,
			Parts: []llm.Part{{
				Type:                  llm.PartText,
				Text:                  "Final answer.",
				ReasoningContent:      "body that should expand",
				ReasoningKind:         llm.ReasoningKindRaw,
				ReasoningSummaryTitle: "Clickable plan",
			}},
			TextContent: "Final answer.",
			CreatedAt:   time.Now(),
			Sequence:    1,
		},
	}
	m.invalidateHistoryCache()
	_ = m.View()
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}

	headerLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Clickable plan") {
			headerLine = i
			break
		}
	}
	if headerLine < 0 {
		t.Fatalf("could not find collapsed thought header in %#v", m.contentLines)
	}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▾ Thought: Clickable plan") || !strings.Contains(view, "body that should expand") {
		t.Fatalf("click should expand actual thought header despite earlier plain text decoy, got %q", view)
	}
}
