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

func TestMouseClickThinkingHeaderUsesSetContentSnapshotWhenHistoryRenderThrottled(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 24
	m.streaming = true
	m.streamRenderMinInterval = time.Hour
	m.tracker = ui.NewToolTracker()
	m.setTextareaValue("")
	m.messages = []session.Message{{
		ID:        301,
		SessionID: "test-session",
		Role:      llm.RoleAssistant,
		Parts: []llm.Part{
			{
				Type:                  llm.PartText,
				Text:                  "first answer",
				ReasoningContent:      "first hidden body\nwith enough lines\nto shift later headers",
				ReasoningKind:         llm.ReasoningKindRaw,
				ReasoningSummaryTitle: "First throttled plan",
			},
			{
				Type:                  llm.PartText,
				Text:                  "second answer",
				ReasoningContent:      "second hidden body",
				ReasoningKind:         llm.ReasoningKindRaw,
				ReasoningSummaryTitle: "Second throttled plan",
			},
		},
		TextContent: "first answer\nsecond answer",
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}
	m.invalidateHistoryCache()
	_ = m.View()
	if m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}
	secondLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Second throttled plan") {
			secondLine = i
			break
		}
	}
	if secondLine < 0 {
		t.Fatalf("could not find second header in %#v", m.contentLines)
	}

	// Simulate a streaming frame in the throttle window: renderHistory rebuilds
	// the live renderer map with the first block expanded, but SetContent is
	// skipped so the viewport still shows the old collapsed content.
	m.reasoningExpansionOverrides = map[int]bool{0: true}
	m.viewCache.historyValid = false
	m.bumpContentVersion()
	_ = m.View()

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: secondLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	if !m.reasoningExpansionOverrides[0] {
		t.Fatalf("preexisting first-block override was lost: %#v", m.reasoningExpansionOverrides)
	}
	if !m.reasoningExpansionOverrides[1] {
		t.Fatalf("click should toggle the second block using the SetContent-time snapshot, got overrides %#v", m.reasoningExpansionOverrides)
	}
}

func TestMouseClickThinkingHeaderUsesTrackerSegmentIndexSnapshot(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = false
	m.setTextareaValue("")
	m.tracker = ui.NewToolTracker()

	first := llm.Part{Type: llm.PartText, ReasoningContent: "first body", ReasoningKind: llm.ReasoningKindRaw, ReasoningSummaryTitle: "Visible first"}
	second := llm.Part{Type: llm.PartText, ReasoningContent: "second body", ReasoningKind: llm.ReasoningKindRaw, ReasoningSummaryTitle: "Visible second"}
	m.tracker.AddReasoningSegment(ui.NormalizeReasoningSegmentRendered(m.renderReasoningPartBlock(first)), reasoningSegmentFromPart(first))
	empty := reasoningSegmentFromPart(llm.Part{Type: llm.PartText, ReasoningContent: "empty body", ReasoningKind: llm.ReasoningKindRaw, ReasoningSummaryTitle: "Empty skipped"})
	m.tracker.Segments = append(m.tracker.Segments, ui.Segment{Type: ui.SegmentReasoning, Reasoning: &empty, Complete: true})
	m.tracker.Version++
	m.tracker.AddReasoningSegment(ui.NormalizeReasoningSegmentRendered(m.renderReasoningPartBlock(second)), reasoningSegmentFromPart(second))
	m.viewCache.completedStream = ui.RenderSegmentsWithImageRenderer(m.tracker.CompletedSegments(), m.width, -1, m.renderMd, true, m.toolsExpanded, m.imageArtifactRenderer())
	_ = m.View()
	if m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}
	secondLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Visible second") {
			secondLine = i
			break
		}
	}
	if secondLine < 0 {
		t.Fatalf("could not find second visible header in %#v", m.contentLines)
	}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: secondLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	if m.tracker.Segments[1].Reasoning == nil {
		t.Fatal("precondition: segment 1 should be the empty reasoning segment")
	}
	if m.tracker.Segments[1].Reasoning.Expanded != nil {
		t.Fatalf("empty skipped reasoning segment was toggled: %#v", m.tracker.Segments[1].Reasoning.Expanded)
	}
	if m.tracker.Segments[2].Reasoning == nil || m.tracker.Segments[2].Reasoning.Expanded == nil || !*m.tracker.Segments[2].Reasoning.Expanded {
		t.Fatalf("second visible reasoning segment was not toggled: %#v", m.tracker.Segments[2].Reasoning)
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

func TestMouseClickLiveThinkingHeaderTogglesBeforeCommit(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = true
	m.setTextareaValue("")
	m.tracker = ui.NewToolTracker()

	updated, _ := m.Update(streamEventMsg{event: ui.StreamEvent{
		Type:           ui.StreamEventReasoning,
		ReasoningKind:  llm.ReasoningKindRaw,
		ReasoningText:  "live hidden body",
		ReasoningTitle: "Live plan",
	}})
	m = updated.(*Model)
	_ = m.View()
	if m.contentLines == nil && m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}

	headerLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Live plan") {
			headerLine = i
			break
		}
	}
	if headerLine < 0 {
		t.Fatalf("could not find live collapsed thought header in %#v", m.contentLines)
	}

	updated, _ = m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	if m.selection.Active || m.selection.Dragging {
		t.Fatal("click on live thought header should not start a selection")
	}
	if m.currentReasoningExpanded == nil || !*m.currentReasoningExpanded {
		t.Fatal("click should set the live reasoning expansion override to expanded")
	}
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▾ Thought: Live plan") || !strings.Contains(view, "live hidden body") {
		t.Fatalf("click should expand live reasoning block, got %q", view)
	}

	updated, _ = m.Update(streamEventMsg{event: ui.StreamEvent{Type: ui.StreamEventText, Text: "Answer."}})
	m = updated.(*Model)
	var committed *ui.ReasoningSegment
	for i := range m.tracker.Segments {
		if m.tracker.Segments[i].Reasoning != nil {
			committed = m.tracker.Segments[i].Reasoning
		}
	}
	if committed == nil {
		t.Fatal("text event should commit live reasoning block to tracker")
	}
	if committed.Expanded == nil || !*committed.Expanded {
		t.Fatal("committed reasoning segment should preserve the click expansion override")
	}
	view = ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▾ Thought: Live plan") || !strings.Contains(view, "live hidden body") {
		t.Fatalf("toggled state should be preserved after commit, got %q", view)
	}
}

func TestMouseClickThinkingHeaderTogglesAfterStreamDone(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.streaming = true
	m.setTextareaValue("")
	m.tracker = ui.NewToolTracker()

	stream := func(ev ui.StreamEvent) {
		updated, _ := m.Update(streamEventMsg{event: ev})
		m = updated.(*Model)
	}
	stream(ui.StreamEvent{
		Type:           ui.StreamEventReasoning,
		ReasoningKind:  llm.ReasoningKindRaw,
		ReasoningText:  "done hidden body",
		ReasoningTitle: "Done plan",
	})
	stream(ui.StreamEvent{Type: ui.StreamEventText, Text: "Final answer."})
	stream(ui.StreamEvent{Type: ui.StreamEventDone, Done: true})

	if m.streaming {
		t.Fatal("precondition: stream should be done")
	}

	expand := ui.StripANSI(m.View().Content)
	if !strings.Contains(expand, "▸ Thought: Done plan") {
		t.Fatalf("reasoning header should be collapsed after done, got %q", expand)
	}

	if m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}
	headerLine := -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▸ Thought: Done plan") {
			headerLine = i
			break
		}
	}
	if headerLine < 0 {
		t.Fatalf("could not find collapsed header after done in %#v", m.contentLines)
	}

	updated, _ := m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▾ Thought: Done plan") || !strings.Contains(view, "done hidden body") {
		t.Fatalf("click should expand reasoning block after stream done, got %q", view)
	}

	if m.viewCache.lastContentStr != "" {
		m.contentLines = strings.Split(m.viewCache.lastContentStr, "\n")
	}
	headerLine = -1
	for i, line := range m.contentLines {
		if strings.Contains(ui.StripANSI(line), "▾ Thought: Done plan") {
			headerLine = i
			break
		}
	}
	if headerLine < 0 {
		t.Fatalf("could not find expanded header after done in %#v", m.contentLines)
	}
	updated, _ = m.Update(tea.MouseClickMsg{X: 0, Y: headerLine - m.viewport.YOffset(), Button: tea.MouseLeft})
	m = updated.(*Model)
	view = ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "▸ Thought: Done plan") || strings.Contains(view, "done hidden body") {
		t.Fatalf("second click should collapse reasoning block after stream done, got %q", view)
	}
}

// After a stream finishes, the tracker is intentionally kept populated in
// alt-screen mode so reasoning headers stay clickable. A following /compact or
// manual /handover starts streaming without going through sendMessage, so it
// must drop that retained tracker — otherwise, once completedStream has been
// cleared (e.g. by a resize), the finished turn renders twice: once from
// message history and once from the stale tracker via renderStreamingInline.
func TestRetainedTrackerDoesNotDuplicateTurnOnNextStream(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 24
	m.streaming = true
	m.setTextareaValue("")
	m.tracker = ui.NewToolTracker()

	stream := func(ev ui.StreamEvent) {
		updated, _ := m.Update(streamEventMsg{event: ev})
		m = updated.(*Model)
	}
	stream(ui.StreamEvent{
		Type:           ui.StreamEventReasoning,
		ReasoningKind:  llm.ReasoningKindRaw,
		ReasoningText:  "thinking body",
		ReasoningTitle: "Plan A",
	})
	stream(ui.StreamEvent{Type: ui.StreamEventText, Text: "UNIQUE_ANSWER_TOKEN"})
	stream(ui.StreamEvent{Type: ui.StreamEventDone, Done: true})
	_ = m.View()

	// Simulate a resize: clears completedStream but leaves the tracker populated.
	m.viewCache.completedStream = ""
	m.invalidateHistoryCache()
	m.bumpContentVersion()

	// The finished turn now renders from message history exactly once.
	historyView := ui.StripANSI(m.renderHistory())
	if got := strings.Count(historyView, "UNIQUE_ANSWER_TOKEN"); got != 1 {
		t.Fatalf("precondition: history should render the turn once, got %d", got)
	}

	// Start a fresh non-sendMessage stream the way /compact does.
	m.streaming = true
	m.resetRetainedStreamTracker()

	streamingView := ui.StripANSI(m.renderStreamingInline())
	if got := strings.Count(streamingView, "UNIQUE_ANSWER_TOKEN"); got != 0 {
		t.Fatalf("retained tracker should be cleared before a fresh stream; streaming render duplicated the finished turn %d time(s): %q", got, streamingView)
	}
}
