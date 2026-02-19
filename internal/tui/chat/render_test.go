package chat

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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
		"mock-model",
		nil,   // mcpManager
		20,    // maxTurns
		false, // forceExternalSearch
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
		"mock-model",
		nil,   // mcpManager
		20,    // maxTurns
		false, // forceExternalSearch
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

func TestStreamEventDiffFlushUsesOrderedCommandComposition(t *testing.T) {
	provider := llm.NewMockProvider("mock")
	engine := llm.NewEngine(provider, nil)

	m := New(
		&config.Config{DefaultProvider: "mock"},
		provider,
		engine,
		"mock-model",
		nil,
		20,
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
		"mock-model",
		nil,
		20,
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
	if !strings.Contains(stripped, "queued") {
		t.Fatalf("expected (queued) label in output, got %q", stripped)
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
	if strings.Contains(stripped, "queued") {
		t.Fatalf("expected no (queued) when pendingInterjection is empty, got %q", stripped)
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
