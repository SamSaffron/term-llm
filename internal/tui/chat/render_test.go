package chat

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestRenderMarkdown_MatchesSharedRenderer_ForTabs(t *testing.T) {
	content := "```\na\tb\n```"

	m := &Model{width: 80}
	got := m.renderMarkdown(content)
	want := ui.RenderMarkdownWithOptions(content, 80, ui.MarkdownRenderOptions{
		WrapOffset:        2,
		NormalizeTabs:     true,
		NormalizeNewlines: false,
	})

	if got != want {
		t.Fatalf("chat markdown render must match shared renderer for tabbed content\nwant:\n%q\n\ngot:\n%q", want, got)
	}
}

func TestRenderMarkdown_NarrowWidth_DoesNotFallbackToRaw(t *testing.T) {
	input := "**bold**"
	m := &Model{width: 1}
	got := m.renderMarkdown(input)

	if strings.TrimSpace(got) == strings.TrimSpace(input) {
		t.Fatalf("expected narrow-width markdown rendering, got raw fallback: %q", got)
	}
}

func TestTryAppendAltScreenStreamingContent_AppendsTailLines(t *testing.T) {
	m := &Model{}
	m.viewCache.lastContentHistoryPlusStream = true
	m.viewCache.lastContentStr = "history\nassistant"
	m.viewCache.lastStreamingContent = "assistant"

	got, ok := m.tryAppendAltScreenStreamingContent("assistant more\nnext")
	if !ok {
		t.Fatal("expected append-only streaming update to be reused incrementally")
	}

	want := []string{"history", "assistant more", "next"}
	if !slices.Equal(got, want) {
		t.Fatalf("unexpected content lines\nwant: %#v\n got: %#v", want, got)
	}
}

func TestInvalidateHistoryCache_ResetsAltScreenStreamingAppendCache(t *testing.T) {
	m := &Model{}
	m.viewCache.historyLines = []string{"old history"}
	m.viewCache.lastContentHistoryPlusStream = true
	m.viewCache.lastContentStr = "old history\nassistant"
	m.viewCache.lastStreamingContent = "assistant"
	m.contentLines = []string{"old history", "assistant"}

	m.invalidateHistoryCache()

	if m.viewCache.historyLines != nil {
		t.Fatalf("expected cached history lines to be cleared, got %#v", m.viewCache.historyLines)
	}
	if m.viewCache.lastContentHistoryPlusStream {
		t.Fatal("expected append cache to be disabled after history invalidation")
	}
	if m.viewCache.lastStreamingContent != "" {
		t.Fatalf("expected last streaming content to be cleared, got %q", m.viewCache.lastStreamingContent)
	}
	if m.viewCache.lastContentStr != "" {
		t.Fatalf("expected cached content string to be cleared, got %q", m.viewCache.lastContentStr)
	}
	if m.contentLines != nil {
		t.Fatalf("expected cached content lines to be cleared, got %#v", m.contentLines)
	}
}

func TestTryAppendAltScreenStreamingContent_FallsBackOnRewrite(t *testing.T) {
	m := &Model{}
	m.viewCache.lastContentHistoryPlusStream = true
	m.viewCache.lastContentStr = "history\nassistant"
	m.viewCache.lastStreamingContent = "assistant"

	if _, ok := m.tryAppendAltScreenStreamingContent("rewritten assistant"); ok {
		t.Fatal("expected rewrite to force full viewport rebuild")
	}
}

func TestViewAltScreen_WaveTickRerendersWithoutSetContent(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.streaming = true
	m.tracker.HandleToolStart("call-1", "read_file", "(very-long-file-name.go)", nil)
	m.tracker.WavePos = 0

	first := m.View().Content
	firstContentVersion := m.viewCache.contentVersion
	firstRenderedVersion := m.viewCache.lastRenderedVersion
	firstSetContentAt := m.viewCache.lastSetContentAt

	_, _ = m.Update(ui.WaveTickMsg{})
	second := m.View().Content

	if m.viewCache.contentVersion != firstContentVersion {
		t.Fatalf("wave-only tick should not bump contentVersion: before=%d after=%d", firstContentVersion, m.viewCache.contentVersion)
	}
	if m.viewCache.lastRenderedVersion != firstRenderedVersion {
		t.Fatalf("wave-only tick should not advance lastRenderedVersion: before=%d after=%d", firstRenderedVersion, m.viewCache.lastRenderedVersion)
	}
	if !m.viewCache.lastSetContentAt.Equal(firstSetContentAt) {
		t.Fatalf("wave-only tick should not call SetContent: before=%v after=%v", firstSetContentAt, m.viewCache.lastSetContentAt)
	}
	if first == second {
		t.Fatal("expected wave-only tick to change rendered output")
	}
}

func TestViewAltScreen_SubagentProgressRebuildsViewportContent(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.streaming = true
	m.streamRenderMinInterval = 0

	args, err := json.Marshal(tools.SpawnAgentArgs{AgentName: "reviewer", Prompt: "review the code"})
	if err != nil {
		t.Fatal(err)
	}
	m.tracker.HandleToolStart("call-1", "spawn_agent", "reviewer", args)

	_ = m.View().Content
	initialTrackerVersion := m.tracker.Version
	initialContentVersion := m.viewCache.contentVersion
	initialSetContentAt := m.viewCache.lastSetContentAt

	_, _ = m.Update(SubagentProgressMsg{
		CallID: "call-1",
		Event:  tools.SubagentEvent{Type: tools.SubagentEventText, Text: "subagent output that adds a visible preview line"},
	})
	if m.tracker.Version <= initialTrackerVersion {
		t.Fatalf("subagent progress should bump tracker version: before=%d after=%d", initialTrackerVersion, m.tracker.Version)
	}

	content := m.View().Content
	if m.viewCache.lastRenderedVersion != m.viewCache.contentVersion {
		t.Fatalf("expected subagent progress to be rendered: lastRenderedVersion=%d contentVersion=%d", m.viewCache.lastRenderedVersion, m.viewCache.contentVersion)
	}
	if m.viewCache.contentVersion <= initialContentVersion {
		t.Fatalf("subagent progress render should bump content version: before=%d after=%d", initialContentVersion, m.viewCache.contentVersion)
	}
	if !m.viewCache.lastSetContentAt.After(initialSetContentAt) {
		t.Fatalf("subagent progress should rebuild viewport content: before=%v after=%v", initialSetContentAt, m.viewCache.lastSetContentAt)
	}
	if !strings.Contains(ui.StripANSI(content), "subagent output") {
		t.Fatalf("expected rendered content to include subagent progress, got %q", ui.StripANSI(content))
	}
}

func TestUpdate_StreamError_BumpsContentVersion(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)

	m := New(
		&config.Config{DefaultProvider: "mock"},
		provider,
		engine,
		"mock",
		"mock-model",
		nil,   // mcpManager
		20,    // maxTurns
		false, // forceExternalSearch
		false, // disableExternalWebFetch
		false, // searchEnabled
		nil,   // localTools
		"",    // toolsStr
		"",    // mcpStr
		false, // showStats
		"",    // initialText
		nil,   // store
		nil,   // sess
		true,  // altScreen
		nil,   // autoSendQueue
		false, // autoSendExitOnDone
		false, // textMode
		"",    // agentName
		"",    // platformDeveloperMessage
		false, // yolo
	)
	m.streaming = true
	before := m.viewCache.contentVersion

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})

	if m.viewCache.contentVersion <= before {
		t.Fatalf("contentVersion must advance on stream error in alt-screen mode (before=%d after=%d)", before, m.viewCache.contentVersion)
	}
}

func TestUpdate_StreamError_PreservesAltScreenStreamingContent(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.streaming = true
	m.tracker.AddTextSegment("partial streamed answer", m.width)
	before := m.viewCache.contentVersion

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})

	if m.streaming {
		t.Fatal("streaming should be false after stream error")
	}
	if m.err != nil {
		t.Fatal("stream error should be transient footer state, not durable m.err")
	}
	if !strings.Contains(ui.StripANSI(m.viewCache.completedStream), "partial streamed answer") {
		t.Fatalf("completedStream should preserve partial content, got %q", m.viewCache.completedStream)
	}
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "partial streamed answer") {
		t.Fatalf("rendered view should contain partial content, got %q", view)
	}
	if !strings.Contains(view, "Stream failed: boom") {
		t.Fatalf("rendered view should contain transient footer error, got %q", view)
	}
	if m.viewCache.contentVersion <= before {
		t.Fatalf("contentVersion must advance on stream error (before=%d after=%d)", before, m.viewCache.contentVersion)
	}
}

func TestUpdate_StreamError_MarksPendingToolsFailed(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.streaming = true
	m.tracker.HandleToolStart("call-1", "read_file", "test.go", nil)

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})

	if len(m.tracker.Segments) != 1 {
		t.Fatalf("segments = %d, want 1", len(m.tracker.Segments))
	}
	if m.tracker.Segments[0].ToolStatus != ui.ToolError {
		t.Fatalf("pending tool status = %v, want ToolError", m.tracker.Segments[0].ToolStatus)
	}
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "read_file") || !strings.Contains(view, "Stream failed: boom") {
		t.Fatalf("rendered view should contain failed tool and transient footer error, got %q", view)
	}
}

func TestUpdate_StreamErrorBeforeAssistantOutputKeepsUserVisible(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.height = 20
	m.syncAltScreenViewportHeight(m.buildFooterLayout().height)
	m.streaming = true
	m.messages = []session.Message{*session.NewMessage(m.sess.ID, llm.UserText("hello from user"), 0)}
	m.viewCache.historyValid = true

	_, _ = m.Update(streamEventMsg{event: ui.ErrorEvent(errors.New("boom"))})

	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "hello from user") {
		t.Fatalf("rendered view should keep user message visible, got %q", view)
	}
	if !strings.Contains(view, "Stream failed: boom") {
		t.Fatalf("rendered view should contain transient footer error, got %q", view)
	}
}

func TestUpdate_StreamRetryStatusIsGeneric(t *testing.T) {
	m := newTestChatModel(true)

	_, _ = m.Update(streamEventMsg{event: ui.RetryEvent(1, 2, 0.5)})

	if !strings.Contains(m.retryStatus, "Retrying stream") {
		t.Fatalf("retry status should be generic, got %q", m.retryStatus)
	}
	if strings.Contains(m.retryStatus, "Rate limited") {
		t.Fatalf("retry status should not be rate-limit-specific, got %q", m.retryStatus)
	}
}

func TestUpdate_StreamRetryStatusClearsOnRecoveryProgress(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true

	_, _ = m.Update(streamEventMsg{event: ui.RetryEvent(1, 5, 0)})
	if !strings.Contains(m.retryStatus, "Retrying stream") {
		t.Fatalf("retry status should be set, got %q", m.retryStatus)
	}
	before := m.viewCache.contentVersion

	_, _ = m.Update(streamEventMsg{event: ui.ToolStartEvent("call-1", "read_file", "(a.go)", nil)})

	if m.retryStatus != "" {
		t.Fatalf("retry status should clear on recovery progress, got %q", m.retryStatus)
	}
	if m.viewCache.contentVersion <= before {
		t.Fatalf("clearing retry status should invalidate rendered content: before=%d after=%d", before, m.viewCache.contentVersion)
	}
}

func TestUpdate_AttemptDiscardKeepsCommittedToolUsage(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true
	m.stats = ui.NewSessionStats()

	_, _ = m.Update(streamEventMsg{event: ui.TextEvent("before tool")})
	_, _ = m.Update(streamEventMsg{event: ui.UsageEvent(10, 5, 0, 0)})
	_, _ = m.Update(streamEventMsg{event: ui.ToolStartEvent("call-1", "read_file", "(a.go)", nil)})
	_, _ = m.Update(streamEventMsg{event: ui.ToolEndEvent("call-1", "read_file", "(a.go)", true)})
	_, _ = m.Update(streamEventMsg{event: ui.UsageEvent(3, 4, 0, 0)})
	_, _ = m.Update(streamEventMsg{event: ui.AttemptDiscardEvent()})

	if m.stats.InputTokens != 10 || m.stats.OutputTokens != 5 || m.stats.LLMCallCount != 1 {
		t.Fatalf("stats after committed usage + discard = %+v, want only committed usage", m.stats)
	}
}

func TestViewAltScreen_FirstRenderAnchorsToBottom(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)

	m := New(
		&config.Config{DefaultProvider: "mock"},
		provider,
		engine,
		"mock",
		"mock-model",
		nil,   // mcpManager
		20,    // maxTurns
		false, // forceExternalSearch
		false, // disableExternalWebFetch
		false, // searchEnabled
		nil,   // localTools
		"",    // toolsStr
		"",    // mcpStr
		false, // showStats
		"",    // initialText
		nil,   // store
		nil,   // sess
		true,  // altScreen
		nil,   // autoSendQueue
		false, // autoSendExitOnDone
		false, // textMode
		"",    // agentName
		"",    // platformDeveloperMessage
		false, // yolo
	)

	for i := 0; i < 120; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		text := "message " + strconv.Itoa(i) + " " + strings.Repeat("content ", 20)
		m.messages = append(m.messages, session.Message{
			ID:          int64(i + 1),
			SessionID:   m.sess.ID,
			Role:        role,
			TextContent: text,
			Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
			CreatedAt:   time.Now(),
			Sequence:    i,
		})
	}

	_ = m.View()

	if !m.viewport.AtBottom() {
		t.Fatalf("expected first alt-screen render to anchor at bottom for resumed history")
	}
}

func TestSendMessage_AltScreen_ScrollsToBottomEvenWhenScrolledUp(t *testing.T) {
	m := newTestChatModel(true)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	for i := 0; i < 120; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		text := "message " + strconv.Itoa(i) + " " + strings.Repeat("content ", 20)
		m.messages = append(m.messages, session.Message{
			ID:          int64(i + 1),
			SessionID:   m.sess.ID,
			Role:        role,
			TextContent: text,
			Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
			CreatedAt:   time.Now(),
			Sequence:    i,
		})
	}

	_ = m.View()
	if !m.viewport.AtBottom() {
		t.Fatalf("precondition: expected viewport at bottom after first render")
	}

	m.viewport.ScrollUp(20)
	if m.viewport.AtBottom() {
		t.Fatalf("precondition: expected viewport not at bottom after ScrollUp(20)")
	}

	_, _ = m.sendMessage("new prompt")
	_ = m.View()

	if !m.viewport.AtBottom() {
		t.Fatalf("expected viewport to scroll to bottom after submitting a new message while scrolled up")
	}
}

func TestApprovalRequest_AltScreen_ScrollsToBottomEvenWhenScrolledUp(t *testing.T) {
	m := newTestChatModel(true)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	for i := 0; i < 120; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		text := "message " + strconv.Itoa(i) + " " + strings.Repeat("content ", 20)
		m.messages = append(m.messages, session.Message{
			ID:          int64(i + 1),
			SessionID:   m.sess.ID,
			Role:        role,
			TextContent: text,
			Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
			CreatedAt:   time.Now(),
			Sequence:    i,
		})
	}

	_ = m.View()
	if !m.viewport.AtBottom() {
		t.Fatalf("precondition: expected viewport at bottom after first render")
	}

	m.viewport.ScrollUp(20)
	if m.viewport.AtBottom() {
		t.Fatalf("precondition: expected viewport not at bottom after ScrollUp(20)")
	}

	doneCh := make(chan tools.ApprovalResult, 1)
	_, _ = m.Update(ApprovalRequestMsg{
		Path:   t.TempDir() + "/file.go",
		DoneCh: doneCh,
	})
	_ = m.View()

	if !m.viewport.AtBottom() {
		t.Fatalf("expected viewport to scroll to bottom when approval prompt appears while scrolled up")
	}
}

func TestAskUserRequest_AltScreen_ScrollsToBottomEvenWhenScrolledUp(t *testing.T) {
	m := newTestChatModel(true)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	for i := 0; i < 120; i++ {
		role := llm.RoleUser
		if i%2 == 1 {
			role = llm.RoleAssistant
		}
		text := "message " + strconv.Itoa(i) + " " + strings.Repeat("content ", 20)
		m.messages = append(m.messages, session.Message{
			ID:          int64(i + 1),
			SessionID:   m.sess.ID,
			Role:        role,
			TextContent: text,
			Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
			CreatedAt:   time.Now(),
			Sequence:    i,
		})
	}

	_ = m.View()
	if !m.viewport.AtBottom() {
		t.Fatalf("precondition: expected viewport at bottom after first render")
	}

	m.viewport.ScrollUp(20)
	if m.viewport.AtBottom() {
		t.Fatalf("precondition: expected viewport not at bottom after ScrollUp(20)")
	}

	doneCh := make(chan []tools.AskUserAnswer, 1)
	_, _ = m.Update(AskUserRequestMsg{
		Questions: []tools.AskUserQuestion{{
			Header:   "Q1",
			Question: "Pick",
			Options:  []tools.AskUserOption{{Label: "A"}, {Label: "B"}},
		}},
		DoneCh: doneCh,
	})
	_ = m.View()

	if !m.viewport.AtBottom() {
		t.Fatalf("expected viewport to scroll to bottom when ask_user prompt appears while scrolled up")
	}
}

func TestViewAltScreen_RefreshesWhenMessagesReplacedWithSameCount(t *testing.T) {
	m := newTestChatModel(true)
	sessionID := m.sess.ID

	m.messages = []session.Message{
		{
			ID:          1,
			SessionID:   sessionID,
			Role:        llm.RoleUser,
			TextContent: "first prompt",
			Parts:       []llm.Part{{Type: llm.PartText, Text: "first prompt"}},
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
		{
			ID:          2,
			SessionID:   sessionID,
			Role:        llm.RoleAssistant,
			TextContent: "old reply",
			Parts:       []llm.Part{{Type: llm.PartText, Text: "old reply"}},
			CreatedAt:   time.Now(),
			Sequence:    1,
		},
	}

	first := ui.StripANSI(m.View().Content)
	if !strings.Contains(first, "old reply") {
		t.Fatalf("expected initial render to include old reply, got %q", first)
	}

	replacement := []session.Message{
		{
			ID:          1,
			SessionID:   sessionID,
			Role:        llm.RoleUser,
			TextContent: "first prompt",
			Parts:       []llm.Part{{Type: llm.PartText, Text: "first prompt"}},
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
		{
			ID:          2,
			SessionID:   sessionID,
			Role:        llm.RoleAssistant,
			TextContent: "new final reply",
			Parts:       []llm.Part{{Type: llm.PartText, Text: "new final reply"}},
			CreatedAt:   time.Now(),
			Sequence:    1,
		},
	}

	_, _ = m.Update(sessionLoadedMsg{
		sess:     &session.Session{ID: sessionID},
		messages: replacement,
	})

	second := ui.StripANSI(m.View().Content)
	if strings.Contains(second, "old reply") {
		t.Fatalf("expected stale history cache to be invalidated, got %q", second)
	}
	if !strings.Contains(second, "new final reply") {
		t.Fatalf("expected refreshed render to include replacement message, got %q", second)
	}
}

func TestViewAltScreen_CompletionsOverlayStaysOnScreen(t *testing.T) {
	m := newTestChatModel(true)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.completions.Show()
	m.setTextareaValue("/")
	m.updateCompletions()

	view := m.View().Content
	stripped := ui.StripANSI(view)

	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("alt-screen view height = %d, want <= %d when completions are visible", got, m.height)
	}
	if !strings.Contains(stripped, "/help") {
		t.Fatalf("expected completions popup to remain visible, got %q", stripped)
	}
	if !strings.Contains(stripped, "mock-model") {
		t.Fatalf("expected footer to remain visible with completions popup, got %q", stripped)
	}
}

func TestViewAltScreen_ViewportHeightAccountsForMultilineFooter(t *testing.T) {
	m := newTestChatModel(true)
	m.setTextareaValue("line one\nline two\nline three\nline four")

	footerHeight := lipgloss.Height(m.renderInputInline())
	if footerHeight <= 4 {
		t.Fatalf("expected multiline footer height > 4, got %d", footerHeight)
	}

	_ = m.View()

	wantHeight := m.height - footerHeight
	if wantHeight < 1 {
		wantHeight = 1
	}
	if m.viewport.Height() != wantHeight {
		t.Fatalf("viewport height = %d, want %d for footer height %d", m.viewport.Height(), wantHeight, footerHeight)
	}
}

func TestViewAltScreen_HeightOnlyResizePreservesLastMessage(t *testing.T) {
	m := newTestChatModel(true)
	sessionID := m.sess.ID

	m.messages = []session.Message{
		{
			ID:          1,
			SessionID:   sessionID,
			Role:        llm.RoleUser,
			TextContent: "hello",
			Parts:       []llm.Part{{Type: llm.PartText, Text: "hello"}},
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
		{
			ID:          2,
			SessionID:   sessionID,
			Role:        llm.RoleAssistant,
			TextContent: "world",
			Parts:       []llm.Part{{Type: llm.PartText, Text: "world"}},
			CreatedAt:   time.Now(),
			Sequence:    1,
		},
	}

	// Simulate completed stream state: completedStream is showing the last turn.
	m.viewCache.completedStream = "rendered world content"
	m.invalidateHistoryCache()

	// First render — history cache is built with the skip (last turn excluded),
	// but completedStream supplies it. Content should include "world".
	first := ui.StripANSI(m.View().Content)
	if !strings.Contains(first, "world") {
		t.Fatalf("expected first render to contain 'world', got %q", first)
	}

	// Simulate height-only resize (width stays the same).
	_, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height - 5})

	// After resize, completedStream is cleared. The history cache must be
	// invalidated so renderHistory() re-includes the last assistant turn.
	second := ui.StripANSI(m.View().Content)
	if !strings.Contains(second, "world") {
		t.Fatalf("expected 'world' to remain visible after height-only resize, got %q", second)
	}
}

func TestRenderStatusLine_FitsViewportWidth(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 24
	m.modelName = "claude-sonnet-4-20250514"
	m.yolo = true
	m.searchEnabled = true
	m.localTools = []string{"read_file", "write_file", "shell", "grep"}
	m.streaming = true
	m.phase = "Responding"
	m.currentTokens = 12345
	m.streamStartTime = time.Now().Add(-45 * time.Second)
	m.stats = ui.NewSessionStats()
	m.stats.CachedInputTokens = 500_000

	rendered := ui.StripANSI(m.renderStatusLine())
	if strings.Contains(rendered, "\n") {
		t.Fatalf("expected status line to stay on one line, got %q", rendered)
	}
	if lipgloss.Width(rendered) > m.width {
		t.Fatalf("status line width = %d, want <= %d; line=%q", lipgloss.Width(rendered), m.width, rendered)
	}
}

func TestRenderStatusLine_RightAlignsStreamingPhase(t *testing.T) {
	width := 64
	started := time.Now().Add(-42 * time.Second)

	minimal := newTestChatModel(false)
	minimal.width = width
	minimal.streaming = true
	minimal.phase = "Thinking"
	minimal.streamStartTime = started

	busy := newTestChatModel(false)
	busy.width = width
	busy.agentName = "developer"
	busy.modelName = "gpt-5.5-medium"
	busy.yolo = true
	busy.searchEnabled = true
	busy.localTools = []string{"read_file", "write_file", "shell", "grep", "edit_file", "web_search", "read_url", "glob"}
	busy.stats.CachedInputTokens = 1_700_000
	busy.streaming = true
	busy.phase = "Thinking"
	busy.streamStartTime = started

	minimalLine := ui.StripANSI(minimal.renderStatusLine())
	busyLine := ui.StripANSI(busy.renderStatusLine())
	minimalIdx := strings.Index(minimalLine, "Thinking")
	busyIdx := strings.Index(busyLine, "Thinking")
	if minimalIdx < 0 || busyIdx < 0 {
		t.Fatalf("expected both status lines to contain Thinking, got %q and %q", minimalLine, busyLine)
	}
	minimalCol := lipgloss.Width(minimalLine[:minimalIdx])
	busyCol := lipgloss.Width(busyLine[:busyIdx])
	if minimalCol != busyCol {
		t.Fatalf("expected Thinking to be right-aligned at same column, got %d in %q and %d in %q", minimalCol, minimalLine, busyCol, busyLine)
	}
	if lipgloss.Width(minimalLine) != width || lipgloss.Width(busyLine) != width {
		t.Fatalf("expected streaming lines to fill width %d, got widths %d (%q) and %d (%q)", width, lipgloss.Width(minimalLine), minimalLine, lipgloss.Width(busyLine), busyLine)
	}
}

func TestRenderStatusLine_NarrowDropsNonEssentialParts(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 48
	m.agentName = "developer"
	m.modelName = "gpt-5.5-medium"
	m.yolo = true
	m.searchEnabled = true
	m.localTools = []string{"read_file", "write_file", "shell", "grep", "edit_file", "web_search", "read_url", "glob"}
	m.streaming = true
	m.phase = "Responding"
	m.currentTokens = 2500
	m.streamStartTime = time.Now().Add(-7 * time.Second)
	m.stats.CachedInputTokens = 1_700_000

	line := ui.StripANSI(m.renderStatusLine())
	if strings.Contains(line, "\n") || lipgloss.Width(line) > m.width {
		t.Fatalf("expected one narrow status line within width %d, got width %d: %q", m.width, lipgloss.Width(line), line)
	}
	for _, omitted := range []string{"tools:", "web", "mcp:off"} {
		if strings.Contains(line, omitted) {
			t.Fatalf("expected narrow status line to omit %q, got %q", omitted, line)
		}
	}
	if !strings.Contains(line, "Responding") {
		t.Fatalf("expected narrow status line to retain streaming phase, got %q", line)
	}
}

func TestRenderStatusLine_CachedAbbreviatesToCWhenNeeded(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 24
	m.modelName = "gpt-5.5-medium"
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now().Add(-3 * time.Second)
	m.stats.CachedInputTokens = 500_000

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "500K C") {
		t.Fatalf("expected cached usage to abbreviate to C when narrow, got %q", line)
	}
	if strings.Contains(line, "cached") || strings.Contains(line, "cache:") {
		t.Fatalf("expected narrow cached usage not to use old labels, got %q", line)
	}
}

func TestRenderStatusLine_IdleUsesProviderBaselineWithoutHeuristicInflation(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 120
	m.providerName = "openai"
	m.modelName = "gpt-5"
	m.engine.ConfigureContextManagement(m.provider, m.providerName, m.modelName, false)
	m.engine.SetContextEstimateBaseline(130_715, 1)
	m.messages = []session.Message{
		{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}, TextContent: "hello"},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: strings.Repeat("large heuristic text ", 2500)}}, TextContent: strings.Repeat("large heuristic text ", 2500)},
	}

	if inflated := m.engine.EstimateTokens(m.buildMessagesForContextEstimate()); inflated <= 130_715 {
		t.Fatalf("test setup expected heuristic estimate > provider baseline, got %d", inflated)
	}

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "~131K/272K") {
		t.Fatalf("expected idle status line to use provider baseline ~131K/272K, got %q", line)
	}
	if strings.Contains(line, "~142K/272K") {
		t.Fatalf("idle status line used inflated heuristic estimate, got %q", line)
	}
}

func TestStreamEventDiffFlushUsesOrderedCommandComposition(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)

	m := New(
		&config.Config{DefaultProvider: "mock"},
		provider,
		engine,
		"mock",
		"mock-model",
		nil,
		20,
		false,
		false,
		false,
		nil,
		"",
		"",
		false,
		"",
		nil,
		nil,
		false,
		nil,
		false,
		false,
		"",
		"",
		false, // yolo
	)
	m.streaming = true

	_, cmd := m.Update(streamEventMsg{event: ui.DiffEvent("a.txt", "old", "new", 1)})
	if cmd == nil {
		t.Fatal("expected command from diff flush during streaming")
	}

	msg := cmd()
	if _, isBatch := msg.(tea.BatchMsg); isBatch {
		t.Fatalf("expected ordered (sequence) command composition, got concurrent batch")
	}
}

func TestRenderStatusLine_ShowsCompactModelLabel(t *testing.T) {
	m := newTestChatModel(false)
	m.providerName = "ChatGPT (gpt-5.3-codex, effort=xhigh)"
	m.modelName = "gpt-5.3-codex-xhigh"

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "gpt-5.3-codex-xhigh") {
		t.Fatalf("expected status line to include model name, got %q", line)
	}
	if strings.Contains(line, "ChatGPT (") {
		t.Fatalf("expected status line to omit verbose provider metadata, got %q", line)
	}
}

func TestRenderStatusLine_UsesReadableElapsedWhileStreaming(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now().Add(-8732 * time.Second)

	line := ui.StripANSI(m.renderStatusLine())
	if regexp.MustCompile(`\d+\.\d+s`).MatchString(line) {
		t.Fatalf("expected elapsed time without sub-second precision, got %q", line)
	}
	if strings.Contains(line, "8732s") || strings.Contains(line, "8732.0s") {
		t.Fatalf("expected elapsed time not to use raw seconds, got %q", line)
	}
	if !strings.Contains(line, "Thinking 2h25m32s") {
		t.Fatalf("expected readable elapsed time in status line, got %q", line)
	}
}

func TestRenderStatusLine_UsesEstimatedContextBeforeUsageArrives(t *testing.T) {
	m := newTestChatModel(false)
	m.engine.SetContextTracking(200_000)
	userText := strings.Repeat("architecture tradeoffs and implementation details ", 80)
	m.messages = append(m.messages,
		session.Message{
			SessionID:   m.sess.ID,
			Role:        llm.RoleUser,
			TextContent: userText,
			Parts:       []llm.Part{{Type: llm.PartText, Text: userText}},
			CreatedAt:   time.Now(),
			Sequence:    0,
		},
	)

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "/200K") {
		t.Fatalf("expected estimated context usage in status line before usage event, got %q", line)
	}
	if strings.Contains(line, "~0K/") {
		t.Fatalf("expected estimated context usage to stay above zero, got %q", line)
	}
}

func TestNew_ResumeSession_ConfiguresContextEstimateFromLoadedSession(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)
	sess := &session.Session{
		ID:               session.NewID(),
		Provider:         "OpenAI",
		ProviderKey:      "openai",
		Model:            "gpt-5.2",
		Mode:             session.ModeChat,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
		CompactionSeq:    -1,
		LastTotalTokens:  336_000,
		LastMessageCount: 1,
	}
	userText := strings.Repeat("architecture tradeoffs and implementation details ", 80)
	store := &mockStore{
		messages: map[string][]session.Message{
			sess.ID: {{
				SessionID:   sess.ID,
				Role:        llm.RoleUser,
				TextContent: userText,
				Parts:       []llm.Part{{Type: llm.PartText, Text: userText}},
				CreatedAt:   time.Now(),
				Sequence:    0,
			}},
		},
	}

	m := New(
		&config.Config{DefaultProvider: "openai"},
		provider,
		engine,
		"openai",
		"gpt-5.2",
		nil,
		20,
		false,
		false,
		false,
		nil,
		"",
		"",
		false,
		"",
		store,
		sess,
		false,
		nil,
		false,
		false,
		"",
		"",
		false,
	)

	wantLimit := llm.InputLimitForProviderModel("openai", "gpt-5.2")
	if got := m.engine.InputLimit(); got != wantLimit {
		t.Fatalf("engine input limit = %d, want %d", got, wantLimit)
	}

	line := ui.StripANSI(m.renderStatusLine())
	wantUsage := "~336K/" + llm.FormatTokenCount(wantLimit)
	if !strings.Contains(line, wantUsage) {
		t.Fatalf("expected resumed status line to include %q, got %q", wantUsage, line)
	}
}

func TestUpdate_SessionLoadedMsg_ReseedsStatsAndContextTracking(t *testing.T) {
	m := newTestChatModel(false)
	m.providerKey = "openai"
	m.modelName = "gpt-5.2"
	m.config = &config.Config{DefaultProvider: "openai"}
	m.stats = ui.NewSessionStats()
	m.stats.SeedTotals(0, 0, 999_000, 0, 0, 0)

	loadedSess := &session.Session{
		ID:                session.NewID(),
		Provider:          "OpenAI",
		ProviderKey:       "openai",
		Model:             "gpt-5.2",
		Mode:              session.ModeChat,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		CachedInputTokens: 250_000,
		LastTotalTokens:   127_637,
		LastMessageCount:  1,
	}
	userText := strings.Repeat("architecture tradeoffs and implementation details ", 80)
	messages := []session.Message{{
		SessionID:   loadedSess.ID,
		Role:        llm.RoleUser,
		TextContent: userText,
		Parts:       []llm.Part{{Type: llm.PartText, Text: userText}},
		CreatedAt:   time.Now(),
		Sequence:    0,
	}}

	_, _ = m.Update(sessionLoadedMsg{sess: loadedSess, messages: messages})

	if got := m.stats.CachedInputTokens; got != 250_000 {
		t.Fatalf("cached input tokens after session load = %d, want 250000", got)
	}
	wantLimit := llm.InputLimitForProviderModel("openai", "gpt-5.2")
	if got := m.engine.InputLimit(); got != wantLimit {
		t.Fatalf("engine input limit after session load = %d, want %d", got, wantLimit)
	}

	line := ui.StripANSI(m.renderStatusLine())
	if strings.Contains(line, "999K") {
		t.Fatalf("expected stale cached usage to be cleared, got %q", line)
	}
	if !strings.Contains(line, "250K cached") && !strings.Contains(line, "cache:250K") {
		t.Fatalf("expected reseeded cached usage in status line, got %q", line)
	}
	wantUsage := "~128K/" + llm.FormatTokenCount(wantLimit)
	if !strings.Contains(line, wantUsage) {
		t.Fatalf("expected loaded status line to include %q, got %q", wantUsage, line)
	}
}

func TestRenderStatusLine_ShowsTransientFooterMessage(t *testing.T) {
	m := newTestChatModel(false)
	m.footerMessage = "Web search enabled."

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "Web search enabled.") {
		t.Fatalf("expected transient footer message in status line, got %q", line)
	}
	if strings.Contains(line, "mock-model") {
		t.Fatalf("expected transient footer message to temporarily replace normal status line, got %q", line)
	}
}

func TestRenderStatusLine_ShowsCachedUsageWhenPresent(t *testing.T) {
	m := newTestChatModel(false)
	m.stats.CachedInputTokens = 500_000

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "500K cached") && !strings.Contains(line, "cache:500K") {
		t.Fatalf("expected cached usage in status line, got %q", line)
	}
}

func TestRenderStatusLine_ShowsSeededCachedUsageFromSession(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)
	sess := &session.Session{
		ID:                session.NewID(),
		Provider:          provider.Name(),
		Model:             "mock-model",
		Mode:              session.ModeChat,
		CreatedAt:         time.Now(),
		UpdatedAt:         time.Now(),
		CachedInputTokens: 250_000,
	}

	m := New(
		&config.Config{DefaultProvider: "mock"},
		provider,
		engine,
		"mock",
		"mock-model",
		nil,
		20,
		false,
		false,
		false,
		nil,
		"",
		"",
		false,
		"",
		nil,
		sess,
		false,
		nil,
		false,
		false,
		"",
		"",
		false, // yolo
	)

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "250K cached") && !strings.Contains(line, "cache:250K") {
		t.Fatalf("expected seeded cached usage in status line, got %q", line)
	}
}

func TestRenderStatusLine_HidesCachedUsageWhenZero(t *testing.T) {
	m := newTestChatModel(false)
	m.stats.CachedInputTokens = 0

	line := ui.StripANSI(m.renderStatusLine())
	if strings.Contains(line, "cached") || strings.Contains(line, "cache:") {
		t.Fatalf("expected no cached usage segment when cached tokens are zero, got %q", line)
	}
}

func TestRenderStatusLine_WithCachedUsageNarrowWidthStillRenders(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 12
	m.stats.CachedInputTokens = 500_000
	m.localTools = []string{"read_file", "write_file", "shell", "grep"}

	line := ui.StripANSI(m.renderStatusLine())
	if strings.TrimSpace(line) == "" {
		t.Fatalf("expected non-empty status line for narrow width")
	}
	if !strings.Contains(line, "cached") && !strings.Contains(line, "cache:") && !strings.Contains(line, " C") {
		t.Fatalf("expected cached usage segment in narrow-width status line, got %q", line)
	}
}

func TestUpdate_StreamEventUsage_CacheOnlyUpdatesStats(t *testing.T) {
	m := newTestChatModel(false)
	m.stats = ui.NewSessionStats()

	_, _ = m.Update(streamEventMsg{event: ui.UsageEvent(0, 0, 1234, 0)})
	if got := m.stats.CachedInputTokens; got != 1234 {
		t.Fatalf("expected cached input tokens to update from cache-only usage event, got %d", got)
	}
}

func TestRenderInputInline_ShowsPendingInterjection(t *testing.T) {
	m := newTestChatModel(false)
	m.pendingInterjection = "stop doing that"
	m.width = 80

	output := m.renderInputInline()
	stripped := ui.StripANSI(output)

	if !strings.Contains(stripped, "⏳") {
		t.Fatalf("expected pending indicator ⏳ in output, got %q", stripped)
	}
	if !strings.Contains(stripped, "stop doing that") {
		t.Fatalf("expected interjection text in output, got %q", stripped)
	}
	if !strings.Contains(stripped, "will incorporate") {
		t.Fatalf("expected inject label in output, got %q", stripped)
	}
}

func TestRenderInputInline_ShowsInterruptNotice(t *testing.T) {
	m := newTestChatModel(false)
	m.interruptNotice = "✕ cancelled current response — draft restored below"
	m.width = 80

	output := m.renderInputInline()
	stripped := ui.StripANSI(output)

	if !strings.Contains(stripped, "cancelled current response") {
		t.Fatalf("expected interrupt notice in output, got %q", stripped)
	}
}

func TestRenderInputInline_HidesPendingWhenEmpty(t *testing.T) {
	m := newTestChatModel(false)
	m.pendingInterjection = ""
	m.width = 80

	output := m.renderInputInline()
	stripped := ui.StripANSI(output)

	if strings.Contains(stripped, "⏳") {
		t.Fatalf("expected no ⏳ when pendingInterjection is empty, got %q", stripped)
	}
	if strings.Contains(stripped, "will incorporate") {
		t.Fatalf("expected no inject label when pendingInterjection is empty, got %q", stripped)
	}
}

func TestRenderInputInline_TruncatesLongInterjection(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 40
	m.pendingInterjection = strings.Repeat("x", 100)

	output := m.renderInputInline()
	stripped := ui.StripANSI(output)

	if !strings.Contains(stripped, "…") {
		t.Fatalf("expected truncation marker … for long interjection, got %q", stripped)
	}
	if strings.Contains(stripped, strings.Repeat("x", 100)) {
		t.Fatalf("expected long interjection to be truncated, got %q", stripped)
	}
}

func TestRenderStatusLine_ShowsAgentNameBeforeModel(t *testing.T) {
	m := newTestChatModel(false)
	m.agentName = "jarvis"
	m.modelName = "mock-model"

	line := ui.StripANSI(m.renderStatusLine())

	if !strings.Contains(line, "jarvis") {
		t.Fatalf("expected agent name in status line, got %q", line)
	}
	agentIdx := strings.Index(line, "jarvis")
	modelIdx := strings.Index(line, "mock-model")
	if modelIdx == -1 {
		t.Fatalf("expected model name in status line, got %q", line)
	}
	if agentIdx > modelIdx {
		t.Fatalf("expected agent name before model name in status line, got %q", line)
	}
}

func TestRenderStatusLine_OmitsAgentNameWhenUnset(t *testing.T) {
	m := newTestChatModel(false)
	m.agentName = ""
	m.modelName = "mock-model"

	line := ui.StripANSI(m.renderStatusLine())

	// Status line should begin with the model segment, not a blank " · " prefix.
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, " · ") {
		t.Fatalf("expected no leading separator when agentName is empty, got %q", line)
	}
}

func TestViewAutoSend_ShowsAgentNameFirstWhenStreaming(t *testing.T) {
	m := newTestChatModel(false)
	m.agentName = "jarvis"
	m.providerName = "anthropic"
	m.modelName = "claude-sonnet"
	m.streaming = true
	m.streamStartTime = time.Now()

	out := m.viewAutoSend()

	if !strings.HasPrefix(out, "jarvis · ") {
		t.Fatalf("expected viewAutoSend to start with 'jarvis · ', got %q", out)
	}
}

func TestViewAutoSend_OmitsAgentPrefixWhenUnset(t *testing.T) {
	m := newTestChatModel(false)
	m.agentName = ""
	m.providerName = "anthropic"
	m.modelName = "claude-sonnet"
	m.streaming = true
	m.streamStartTime = time.Now()

	out := m.viewAutoSend()

	if strings.HasPrefix(out, " · ") {
		t.Fatalf("expected no leading ' · ' when agentName is empty, got %q", out)
	}
	if !strings.Contains(out, "anthropic") {
		t.Fatalf("expected provider name in auto-send output, got %q", out)
	}
}

func TestRenderStreamingInline_ExpandHintShownOncePerSession(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 80
	m.streaming = true

	_, _ = m.Update(streamEventMsg{event: ui.ToolStartEvent("call-1", "read_file", "(a.go)", nil)})
	first := ui.StripANSI(m.renderStreamingInline())
	if count := strings.Count(first, "(CTRL+e to expand)"); count != 1 {
		t.Fatalf("expected first tool of session to show one hint, got %d in %q", count, first)
	}
	m.tracker.HandleToolEnd("call-1", true)
	m.resetTracker()

	_, _ = m.Update(streamEventMsg{event: ui.ToolStartEvent("call-2", "read_file", "(b.go)", nil)})
	second := ui.StripANSI(m.renderStreamingInline())
	if strings.Contains(second, "(CTRL+e to expand)") {
		t.Fatalf("expected later turn not to show expand hint, got %q", second)
	}
}

func TestRenderStreamingInline_ExpandedPendingShellTool(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 80
	m.streaming = true
	m.toolsExpanded = true
	m.tracker.SetExpanded(true)

	args, err := json.Marshal(tools.ShellArgs{Command: "git status --short", Description: "Check repo status"})
	if err != nil {
		t.Fatalf("marshal shell args: %v", err)
	}
	m.tracker.HandleToolStart("call-1", "shell", "Check repo status", args)

	plain := ui.StripANSI(m.renderStreamingInline())
	if !strings.Contains(plain, "git status --short") {
		t.Fatalf("expected in-progress shell tool to render expanded command, got %q", plain)
	}
	if strings.Contains(plain, "(CTRL+e to expand)") {
		t.Fatalf("expected expand hint hidden while expanded, got %q", plain)
	}
}

func TestRenderStreamingInline_TextToPendingToolUsesBlankLine(t *testing.T) {
	m := newTestChatModel(false)
	m.width = 80
	m.tracker.AddTextSegment("Let me check that file.", m.width)
	m.tracker.MarkCurrentTextComplete(func(s string) string { return s })
	m.tracker.HandleToolStart("call-1", "read_file", "(test.go)", nil)

	plain := ui.StripANSI(m.renderStreamingInline())
	toolLabel := "read_file(test.go)"
	textIdx := strings.Index(plain, "Let me check that file.")
	if textIdx == -1 {
		t.Fatalf("expected text in output, got %q", plain)
	}
	toolIdx := strings.Index(plain, toolLabel)
	if toolIdx == -1 {
		t.Fatalf("expected pending tool label %q in output, got %q", toolLabel, plain)
	}
	if textIdx >= toolIdx {
		t.Fatalf("expected text before tool, text=%d tool=%d output=%q", textIdx, toolIdx, plain)
	}

	between := plain[textIdx+len("Let me check that file.") : toolIdx]
	if got := strings.Count(between, "\n"); got != 2 {
		t.Fatalf("expected exactly 2 newlines between text and pending tool, got %d; between=%q output=%q", got, between, plain)
	}
}

func TestRenderStatusLineShowsFastMode(t *testing.T) {
	m := newTestChatModel(true)
	m.width = 80
	m.fastMode = true

	line := ui.StripANSI(m.renderStatusLine())
	if !strings.Contains(line, "fast") {
		t.Fatalf("expected fast in status line, got %q", line)
	}
}
