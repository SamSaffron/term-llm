package chat

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func TestRenderMarkdown_CachePathMatchesFreshRender_ForTabs(t *testing.T) {
	content := "```\na\tb\n```"

	cached := &Model{width: 80}
	_ = cached.renderMarkdown("warmup")
	gotCached := cached.renderMarkdown(content)

	fresh := &Model{width: 80}
	gotFresh := fresh.renderMarkdown(content)

	if gotCached != gotFresh {
		t.Fatalf("cached render must match fresh render for tabbed content\ncached:\n%q\nfresh:\n%q", gotCached, gotFresh)
	}
}

func TestRenderMarkdown_ClampsRendererWidthForNarrowTerminal(t *testing.T) {
	m := &Model{width: 1}
	_ = m.renderMarkdown("x")

	if m.rendererCache.width < 1 {
		t.Fatalf("renderer width must be clamped to >= 1, got %d", m.rendererCache.width)
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

	first := ui.StripANSI(m.View())
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

	second := ui.StripANSI(m.View())
	if strings.Contains(second, "old reply") {
		t.Fatalf("expected stale history cache to be invalidated, got %q", second)
	}
	if !strings.Contains(second, "new final reply") {
		t.Fatalf("expected refreshed render to include replacement message, got %q", second)
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
	if m.viewport.Height != wantHeight {
		t.Fatalf("viewport height = %d, want %d for footer height %d", m.viewport.Height, wantHeight, footerHeight)
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
	first := ui.StripANSI(m.View())
	if !strings.Contains(first, "world") {
		t.Fatalf("expected first render to contain 'world', got %q", first)
	}

	// Simulate height-only resize (width stays the same).
	_, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height - 5})

	// After resize, completedStream is cleared. The history cache must be
	// invalidated so renderHistory() re-includes the last assistant turn.
	second := ui.StripANSI(m.View())
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
	for _, line := range strings.Split(rendered, "\n") {
		if lipgloss.Width(line) > m.width {
			t.Fatalf("status line width = %d, want <= %d; line=%q", lipgloss.Width(line), m.width, line)
		}
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

func TestRenderStatusLine_UsesWholeSecondElapsedWhileStreaming(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.streamStartTime = time.Now().Add(-1500 * time.Millisecond)

	line := ui.StripANSI(m.renderStatusLine())
	if regexp.MustCompile(`\d+\.\d+s`).MatchString(line) {
		t.Fatalf("expected elapsed time without sub-second precision, got %q", line)
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
	if !strings.Contains(line, "cached") && !strings.Contains(line, "cache:") {
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
