package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

// Test content similar to the Ruby release notes
const testMarkdown = `Great news! Ruby 4.0 was just released on December 25th, 2025 (Christmas Day) - marking 30 years since Ruby's first public release! Here are some of the coolest features:

## **ZJIT - New JIT Compiler**
ZJIT is a new just-in-time (JIT) compiler, which is developed as the next generation of YJIT. Unlike YJIT's lazy basic block versioning approach, ZJIT uses a more traditional method based compilation strategy, is designed to be more accessible to contributors, and follows a "textbook" compiler architecture that's easier to understand and modify. While ZJIT is faster than the interpreter, but not yet as fast as YJIT, it sets the foundation for future performance improvements and easier community contributions.

## **Ruby Box - Experimental Isolation Feature**
Ruby Box is a new (experimental) feature to provide separation about definitions. Ruby Box can isolate/separate monkey patches, changes of global/class variables, class/module definitions, and loaded native/ruby libraries from other boxes. This means you can load multiple versions of a library simultaneously and isolate test cases from each other!

## **Ractor Improvements**
Ractor, Ruby's parallel execution mechanism, has received several improvements, including a new class, Ractor::Port, which was introduced to address issues related to message sending and receiving.

## **Language Changes**
Some nice syntax improvements:
- Logical binary operators (||, &&, and and or) at the beginning of a line continue the previous line, like fluent dot.
- Set has been promoted from stdlib to a core class. No more ` + "`require 'set'`" + ` needed!

It's an exciting release that balances new experimental features with practical improvements!`

func TestRunOnCompleteCapture_CapturesStdout(t *testing.T) {
	result, err := runOnCompleteCapture("cat", "hello")
	if err != nil {
		t.Fatalf("runOnCompleteCapture error: %v", err)
	}
	if result.Stdout != "hello" {
		t.Errorf("stdout = %q, want %q", result.Stdout, "hello")
	}
	if result.Stderr != "" {
		t.Errorf("stderr = %q, want empty", result.Stderr)
	}
}

func TestRunOnCompleteCapture_CapturesStderrAndError(t *testing.T) {
	result, err := runOnCompleteCapture("echo err >&2; exit 7", "")
	if err == nil {
		t.Fatal("expected command error, got nil")
	}
	if result.Stdout != "" {
		t.Errorf("stdout = %q, want empty", result.Stdout)
	}
	if result.Stderr != "err\n" {
		t.Errorf("stderr = %q, want %q", result.Stderr, "err\n")
	}
}

func TestRunOnCompleteCapture_TimesOutHungHook(t *testing.T) {
	oldTimeout := onCompleteTimeout
	onCompleteTimeout = 100 * time.Millisecond
	t.Cleanup(func() { onCompleteTimeout = oldTimeout })

	started := time.Now()
	_, err := runOnCompleteCapture("sleep 10", "")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	if err.Error() != "timed out after 100ms" {
		t.Fatalf("error = %q, want %q", err.Error(), "timed out after 100ms")
	}
	if elapsed >= time.Second {
		t.Fatalf("runOnCompleteCapture took %v, want timeout well under 1s", elapsed)
	}
}

func TestRunOnCompleteCapture_ReturnsWhenBackgroundChildKeepsPipeOpen(t *testing.T) {
	oldTimeout := onCompleteTimeout
	oldWaitDelay := onCompleteWaitDelay
	onCompleteTimeout = time.Second
	onCompleteWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() {
		onCompleteTimeout = oldTimeout
		onCompleteWaitDelay = oldWaitDelay
	})

	started := time.Now()
	result, err := runOnCompleteCapture("(sleep 10) & printf done", "")
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("error = %v, want nil", err)
	}
	if result.Stdout != "done" {
		t.Fatalf("stdout = %q, want %q", result.Stdout, "done")
	}
	if elapsed >= time.Second {
		t.Fatalf("runOnCompleteCapture took %v, want wait-delay recovery well under 1s", elapsed)
	}
}

func TestMarkdownRendering(t *testing.T) {
	width := 80

	// Test full render
	fullRender, err := ui.RenderMarkdownWithError(testMarkdown, width)
	if err != nil {
		t.Fatalf("Failed to render full markdown: %v", err)
	}

	t.Logf("Full render output:\n%s", fullRender)
	t.Logf("Full render length: %d chars, %d lines", len(fullRender), strings.Count(fullRender, "\n"))
}

func TestIncrementalRendering(t *testing.T) {
	width := 80

	// Simulate streaming by splitting content and rendering incrementally.
	// This mimics what the rich terminal renderer path does.
	chunks := simulateStreaming(testMarkdown)

	var content strings.Builder
	var printedLines int
	var allOutput strings.Builder

	for i, chunk := range chunks {
		content.WriteString(chunk)

		// Only render when we get a newline (like the real code does)
		if strings.Contains(chunk, "\n") {
			rendered, err := ui.RenderMarkdownWithError(content.String(), width)
			if err != nil {
				t.Fatalf("Render failed at chunk %d: %v", i, err)
			}

			lines := strings.Split(rendered, "\n")
			for j := printedLines; j < len(lines); j++ {
				if j < len(lines)-1 {
					allOutput.WriteString(lines[j])
					allOutput.WriteString("\n")
					printedLines++
				}
			}
		}
	}

	// Final render
	finalRendered, _ := ui.RenderMarkdownWithError(content.String(), width)
	lines := strings.Split(finalRendered, "\n")
	for i := printedLines; i < len(lines); i++ {
		line := lines[i]
		if line != "" || i < len(lines)-1 {
			allOutput.WriteString(line)
			allOutput.WriteString("\n")
		}
	}

	// Compare with full render
	fullRender, _ := ui.RenderMarkdownWithError(testMarkdown, width)

	t.Logf("Incremental output:\n%s", allOutput.String())
	t.Logf("Full render:\n%s", fullRender)

	// Allow small line count difference (streaming may add/remove trailing newline)
	incrementalLines := strings.Count(allOutput.String(), "\n")
	fullLines := strings.Count(fullRender, "\n")
	lineDiff := incrementalLines - fullLines
	if lineDiff < 0 {
		lineDiff = -lineDiff
	}

	if lineDiff > 1 {
		t.Errorf("Line count differs significantly!\nIncremental lines: %d\nFull lines: %d",
			incrementalLines, fullLines)
	}

	// Ensure ANSI escape codes are preserved (the main fix we're testing)
	if !strings.Contains(allOutput.String(), "\x1b[") {
		t.Error("Incremental output missing ANSI escape codes - wordwrap may be breaking styling")
	}
	if !strings.Contains(fullRender, "\x1b[") {
		t.Error("Full render missing ANSI escape codes")
	}
}

//go:noinline
func updateAskModelForReasoningCopyTest(m askStreamModel, event ui.StreamEvent) askStreamModel {
	updated, _ := m.Update(askStreamEventMsg{event: event})
	return updated.(askStreamModel)
}

func TestAskReasoningDoesNotPanicAfterModelCopy(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	model = updateAskModelForReasoningCopyTest(model, ui.ReasoningEvent(llm.ReasoningKindSummary, "first", "Reading context", "", false, true))

	if got := model.currentReasoningString(); got != "first" {
		t.Fatalf("expected first reasoning chunk to be accumulated, got %q", got)
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second reasoning update panicked after model copy: %v", r)
		}
	}()

	model = updateAskModelForReasoningCopyTest(model, ui.ReasoningEvent(llm.ReasoningKindSummary, " second", "Reading context", "", false, true))

	if got := model.phase; got != "Reading context" {
		t.Fatalf("expected reasoning to keep/update status phase, got %q", got)
	}
}

type askFailOnceMessageStore struct {
	session.NoopStore

	failErr                    error
	assistantFailuresRemaining int
	assistantAttempts          int
	messages                   []session.Message
}

func (s *askFailOnceMessageStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	if msg.Role == llm.RoleAssistant {
		s.assistantAttempts++
		if s.assistantFailuresRemaining > 0 {
			s.assistantFailuresRemaining--
			return s.failErr
		}
	}

	copied := *msg
	copied.Parts = append([]llm.Part(nil), msg.Parts...)
	s.messages = append(s.messages, copied)
	return nil
}

type askMemoryMessageStore struct {
	session.NoopStore

	nextID   int64
	messages []session.Message
	status   session.SessionStatus
	current  string
}

func (s *askMemoryMessageStore) AddMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	s.nextID++
	msg.ID = s.nextID
	copied := *msg
	copied.Parts = append([]llm.Part(nil), msg.Parts...)
	s.messages = append(s.messages, copied)
	return nil
}

func (s *askMemoryMessageStore) UpdateMessage(ctx context.Context, sessionID string, msg *session.Message) error {
	for i := range s.messages {
		if s.messages[i].ID == msg.ID {
			copied := *msg
			copied.Parts = append([]llm.Part(nil), msg.Parts...)
			s.messages[i] = copied
			return nil
		}
	}
	return session.ErrNotFound
}

func (s *askMemoryMessageStore) UpdateStatus(ctx context.Context, id string, status session.SessionStatus) error {
	s.status = status
	return nil
}

func (s *askMemoryMessageStore) SetCurrent(ctx context.Context, sessionID string) error {
	s.current = sessionID
	return nil
}

func TestAskAssistantPersistenceUpsertsSnapshotsAndFinalText(t *testing.T) {
	store := &askMemoryMessageStore{}
	sess := &session.Session{ID: "ask-upsert-test"}
	p := &askAssistantPersistence{store: store, sess: sess, reasoningCfg: config.DefaultReasoningConfig()}

	if err := p.persist(context.Background(), llm.AssistantText("partial"), 10, false); err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}
	if err := p.persist(context.Background(), llm.AssistantText("partial plus more"), 20, true); err != nil {
		t.Fatalf("persist final: %v", err)
	}

	if len(store.messages) != 1 {
		t.Fatalf("messages len = %d, want 1: %+v", len(store.messages), store.messages)
	}
	if got := store.messages[0].TextContent; got != "partial plus more" {
		t.Fatalf("persisted assistant text = %q, want final text", got)
	}
	if got := store.messages[0].DurationMs; got != 20 {
		t.Fatalf("duration = %d, want 20", got)
	}
}

func TestAskAssistantPersistenceInterruptedPrefersPendingSnapshotOverCumulativeCollector(t *testing.T) {
	store := &askMemoryMessageStore{}
	sess := &session.Session{ID: "ask-interrupted-tool-test"}
	p := &askAssistantPersistence{store: store, sess: sess, reasoningCfg: config.DefaultReasoningConfig()}

	if err := p.persist(context.Background(), llm.AssistantText("second turn partial"), 10, false); err != nil {
		t.Fatalf("persist snapshot: %v", err)
	}
	if err := p.persistInterrupted(context.Background(), "first turn already persisted\nsecond turn partial", 20); err != nil {
		t.Fatalf("persist interrupted: %v", err)
	}

	if len(store.messages) != 1 {
		t.Fatalf("messages len = %d, want 1: %+v", len(store.messages), store.messages)
	}
	if got := store.messages[0].TextContent; got != "second turn partial" {
		t.Fatalf("interrupted text = %q, want pending snapshot only", got)
	}
}

func TestAskStreamCancelMarksProgramCanceledAndFlushesPartialText(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80
	updated, _ := model.Update(askContentMsg("partial answer"))
	model = updated.(askStreamModel)

	updated, cmd := model.Update(askCancelledMsg{})
	model = updated.(askStreamModel)
	if !errors.Is(model.streamErr, context.Canceled) {
		t.Fatalf("streamErr = %v, want context.Canceled", model.streamErr)
	}
	if cmd == nil {
		t.Fatal("expected cancel command to flush partial text and quit")
	}
}

func TestStreamPlainTextReturnsContextErrorOnCancel(t *testing.T) {
	ch := make(chan ui.StreamEvent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := streamPlainText(ctx, ch, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("streamPlainText err = %v, want context.Canceled", err)
	}
}

func TestAskResponsePersistenceErrorRedeliversAssistantToTurnCallback(t *testing.T) {
	storeErr := errors.New("sqlite busy")
	store := &askFailOnceMessageStore{
		failErr:                    storeErr,
		assistantFailuresRemaining: 1,
	}
	sess := &session.Session{ID: "ask-persistence-test"}
	reasoningPersistenceCfg := config.DefaultReasoningConfig()
	turnStartTime := time.Now()

	tool := &progressiveTestTool{}
	toolSpec := tool.Spec()
	registry := llm.NewToolRegistry()
	registry.Register(tool)

	provider := llm.NewMockProvider("mock")
	provider.AddTurn(llm.MockTurn{
		Text: "I'll count.",
		ToolCalls: []llm.ToolCall{{
			ID:        "call-1",
			Name:      toolSpec.Name,
			Arguments: json.RawMessage(`{}`),
		}},
	})

	engine := llm.NewEngine(provider, registry)
	engine.SetResponseCompletedCallback(func(ctx context.Context, turnIndex int, assistantMsg llm.Message, metrics llm.TurnMetrics) error {
		durationMs := time.Since(turnStartTime).Milliseconds()
		return persistAskAssistantResponse(ctx, store, sess, assistantMsg, durationMs, reasoningPersistenceCfg)
	})
	engine.SetTurnCompletedCallback(func(ctx context.Context, turnIndex int, turnMessages []llm.Message, metrics llm.TurnMetrics) error {
		for _, msg := range turnMessages {
			sessionMsg := session.NewMessageWithReasoningPolicy(sess.ID, msg, -1, reasoningPersistenceCfg)
			if msg.Role == llm.RoleAssistant {
				sessionMsg.DurationMs = time.Since(turnStartTime).Milliseconds()
			}
			_ = store.AddMessage(ctx, sess.ID, sessionMsg)
		}
		return nil
	})

	stream, err := engine.Stream(context.Background(), llm.Request{
		Messages: []llm.Message{llm.UserText("test")},
		Tools:    []llm.ToolSpec{toolSpec},
	})
	if err != nil {
		t.Fatalf("Stream() error = %v", err)
	}
	defer stream.Close()

	for {
		_, recvErr := stream.Recv()
		if recvErr == io.EOF {
			break
		}
		if recvErr != nil {
			t.Fatalf("Recv() error = %v", recvErr)
		}
	}

	if store.assistantAttempts != 2 {
		t.Fatalf("assistant AddMessage attempts = %d, want response failure plus turn retry", store.assistantAttempts)
	}

	var gotAssistant bool
	var gotToolResult bool
	for _, msg := range store.messages {
		switch msg.Role {
		case llm.RoleAssistant:
			if msg.TextContent == "I'll count." {
				gotAssistant = true
			}
		case llm.RoleTool:
			gotToolResult = true
		}
	}
	if !gotAssistant {
		t.Fatalf("assistant response was not retried through TurnCompletedCallback; persisted messages: %+v", store.messages)
	}
	if !gotToolResult {
		t.Fatalf("tool result was not persisted; persisted messages: %+v", store.messages)
	}
}

func TestAskDoneRendersMarkdown(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	// Note: markdown rendering expects paragraph structure (trailing newlines) to
	// recognize inline markdown as complete content.
	updated, _ := model.Update(askContentMsg("**bold**\n\n"))
	model = updated.(askStreamModel)

	_, cmd := model.Update(askDoneMsg{})
	if cmd == nil {
		t.Fatal("expected a command from askDoneMsg")
	}

	// We can't easily inspect the content of tea.Printf command in a unit test
	// but we can verify that segments are now marked as flushed
	if !model.tracker.Segments[0].Flushed {
		t.Error("expected segments to be flushed on done")
	}
}

func TestAskDoneFlushesSegments(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askToolStartMsg{CallID: "call-1", Name: "shell", Info: "(git status)"})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolEndMsg{CallID: "call-1", Success: true})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askDoneMsg{})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) == 0 {
		t.Fatal("expected tool segment to be tracked")
	}
	if !model.tracker.Segments[0].Flushed {
		t.Fatalf("expected segments to be flushed on done")
	}
}

func TestAskToolStartFlushesCompletedTextBoundary(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askContentMsg("Let me grab the announcement.\n\n"))
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) != 2 {
		t.Fatalf("expected 2 segments (text + tool), got %d", len(model.tracker.Segments))
	}

	textSeg := model.tracker.Segments[0]
	if textSeg.Type != ui.SegmentText {
		t.Fatalf("expected first segment to be text, got %v", textSeg.Type)
	}
	if !textSeg.Complete {
		t.Fatal("expected pre-tool text segment to be complete")
	}
	if !textSeg.Flushed {
		t.Fatal("expected pre-tool text segment to flush at tool boundary")
	}

	toolSeg := model.tracker.Segments[1]
	if toolSeg.Type != ui.SegmentTool {
		t.Fatalf("expected second segment to be tool, got %v", toolSeg.Type)
	}
	if toolSeg.ToolStatus != ui.ToolPending {
		t.Fatalf("expected tool to be pending, got %v", toolSeg.ToolStatus)
	}
}

func TestAskToolEndFlushesCompletedToolBoundary(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askToolStartMsg{CallID: "call-1", Name: "shell", Info: "(pwd)"})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolEndMsg{CallID: "call-1", Success: true})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) != 1 {
		t.Fatalf("expected 1 tool segment, got %d", len(model.tracker.Segments))
	}
	if model.tracker.Segments[0].ToolStatus != ui.ToolSuccess {
		t.Fatalf("expected tool to be successful, got %v", model.tracker.Segments[0].ToolStatus)
	}
	if !model.tracker.Segments[0].Flushed {
		t.Fatal("expected completed tool segment to flush at tool-end boundary")
	}
}

func TestAskCompletedConcurrentToolWaitsBehindPendingBarrier(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askToolStartMsg{CallID: "call-long", Name: "shell", Info: "(sleep 5)"})
	model = updated.(askStreamModel)
	updated, _ = model.Update(askBoundaryFlushedMsg{CallID: "call-long", Name: "shell"})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolStartMsg{CallID: "call-short", Name: "shell", Info: "(sleep 2)"})
	model = updated.(askStreamModel)
	updated, _ = model.Update(askBoundaryFlushedMsg{CallID: "call-short", Name: "shell"})
	model = updated.(askStreamModel)

	updated, _ = model.Update(askToolEndMsg{CallID: "call-short", Success: true})
	model = updated.(askStreamModel)

	if len(model.tracker.Segments) != 2 {
		t.Fatalf("expected 2 tool segments, got %d", len(model.tracker.Segments))
	}
	if model.tracker.Segments[1].Flushed {
		t.Fatal("completed short tool should not flush ahead of the earlier pending long tool")
	}

	plain := stripAnsi(model.View().Content)
	if !strings.Contains(plain, "sleep 5") {
		t.Fatalf("expected pending long-running tool to be visible, got %q", plain)
	}
	if !strings.Contains(plain, "sleep 2") {
		t.Fatalf("expected completed short-running concurrent tool to remain visible, got %q", plain)
	}
}

// TestAskDoneNoDoubleRendering verifies that content appears exactly once
// in the final flush, preventing double rendering issues.
func TestAskDoneNoDoubleRendering(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	// Add unique content
	updated, _ := model.Update(askContentMsg("UNIQUE_MARKER_12345\n\n"))
	model = updated.(askStreamModel)

	// Complete
	_, cmd := model.Update(askDoneMsg{})
	if cmd == nil {
		t.Fatal("expected a command from askDoneMsg")
	}

	// Verify it's flushed
	if !model.tracker.Segments[0].Flushed {
		t.Error("segment should be flushed")
	}
}

func TestAskStreamsTextOnSmoothTicks(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askContentMsg("hello world"))
	model = updated.(askStreamModel)

	// Text should be buffered first; no segment until a smooth tick is processed.
	if len(model.tracker.Segments) != 0 {
		t.Fatalf("expected 0 segments before smooth tick, got %d", len(model.tracker.Segments))
	}

	updated, _ = model.Update(ui.SmoothTickMsg{})
	model = updated.(askStreamModel)
	if len(model.tracker.Segments) == 0 {
		t.Fatal("expected text segment after first smooth tick")
	}

	first := model.tracker.Segments[0].GetText()
	if first == "hello world" {
		t.Fatalf("expected partial word-paced content after first tick, got %q", first)
	}
	if !strings.Contains(first, "hello") {
		t.Fatalf("expected first word after first tick, got %q", first)
	}

	updated, _ = model.Update(ui.SmoothTickMsg{})
	model = updated.(askStreamModel)
	got := model.tracker.Segments[0].GetText()
	if got != "hello world" {
		t.Fatalf("expected full content after second tick, got %q", got)
	}
}

func TestAskCoalescesSmoothTickSchedulingForBurstTextEvents(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, firstCmd := model.Update(askContentMsg("hello"))
	model = updated.(askStreamModel)
	if firstCmd == nil {
		t.Fatal("expected first text chunk to schedule smooth tick")
	}

	updated, secondCmd := model.Update(askContentMsg(" world"))
	model = updated.(askStreamModel)
	if secondCmd != nil {
		t.Fatal("expected no additional smooth tick while one is already pending")
	}
	if !model.smoothTickPending {
		t.Fatal("expected smoothTickPending to remain true until tick is handled")
	}
}

func TestAskDefersNextStreamReadUntilSmoothTick(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80
	model.streamChan = make(chan ui.StreamEvent)

	updated, cmd := model.Update(askStreamEventMsg{event: ui.TextEvent("hello world")})
	model = updated.(askStreamModel)
	if cmd == nil {
		t.Fatal("expected text event to schedule a smooth tick")
	}
	if !model.smoothTickPending {
		t.Fatal("expected smooth tick to be pending after text event")
	}
	if !model.deferredStreamRead {
		t.Fatal("expected next stream read to be deferred until the smooth tick")
	}

	updated, cmd = model.Update(ui.SmoothTickMsg{})
	model = updated.(askStreamModel)
	if model.deferredStreamRead {
		t.Fatal("expected deferred stream read to clear after smooth tick")
	}
	if cmd == nil {
		t.Fatal("expected smooth tick to resume stream reading")
	}
}

func TestAskStreamingFlushThresholdAdaptsToBufferSize(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80
	model.adaptiveFlushThreshold = true

	if got := model.streamingFlushThreshold(); got != 0 {
		t.Fatalf("expected zero threshold for empty buffer, got %d", got)
	}

	model.smoothBuffer.Write(strings.Repeat("word ", 80)) // ~400 bytes
	mid := model.streamingFlushThreshold()
	if mid <= 0 {
		t.Fatalf("expected positive threshold for medium buffer, got %d", mid)
	}

	model.smoothBuffer.Write(strings.Repeat("word ", 400)) // +~2000 bytes
	high := model.streamingFlushThreshold()
	if high <= mid {
		t.Fatalf("expected larger threshold for large buffer, got mid=%d high=%d", mid, high)
	}
}

func TestAskToolStartFlushUsesOrderedCommandComposition(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	updated, _ := model.Update(askContentMsg("Before tool boundary.\n\n"))
	model = updated.(askStreamModel)

	updated, cmd := model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if cmd == nil {
		t.Fatal("expected command from tool-start boundary flush")
	}

	msg := cmd()
	if _, isBatch := msg.(tea.BatchMsg); isBatch {
		t.Fatalf("expected ordered (sequence) command composition, got concurrent batch")
	}
}

func TestAskToolStartDefersCachedContentRefreshUntilBoundaryFlushAck(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	model.tracker.AddTextSegment("Before tool boundary.\n\n", model.width)
	model.tracker.MarkCurrentTextComplete(func(text string) string {
		return renderMd(text, model.width)
	})
	model.cachedContent = model.tracker.RenderUnflushed(model.width, renderMd, false)
	model.contentDirty = false

	updated, cmd := model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if cmd == nil {
		t.Fatal("expected command from tool-start boundary flush")
	}

	if model.pendingBoundaryFlushes != 1 {
		t.Fatalf("expected one pending boundary flush, got %d", model.pendingBoundaryFlushes)
	}
	if !model.contentDirty {
		t.Fatal("expected contentDirty to remain true until boundary flush ack")
	}

	updated, _ = model.Update(askBoundaryFlushedMsg{CallID: "call-1", Name: "read_file"})
	model = updated.(askStreamModel)

	expected := model.tracker.RenderUnflushed(model.width, renderMd, false)
	if model.cachedContent != expected {
		t.Fatalf("cached content should refresh after boundary flush ack\nexpected: %q\ngot: %q", expected, model.cachedContent)
	}
	if model.contentDirty {
		t.Fatal("expected contentDirty to be false after boundary flush ack refresh")
	}
}

func TestAskToolStartViewDefersPendingToolRowUntilBoundaryFlushAck(t *testing.T) {
	model := newAskStreamModel()
	model.width = 80

	model.tracker.AddTextSegment("Before tool boundary.\n\n", model.width)
	model.tracker.MarkCurrentTextComplete(func(text string) string {
		return renderMd(text, model.width)
	})
	model.cachedContent = model.tracker.RenderUnflushed(model.width, renderMd, false)
	model.contentDirty = false

	updated, cmd := model.Update(askToolStartMsg{CallID: "call-1", Name: "read_file", Info: "(announcement.md)"})
	model = updated.(askStreamModel)

	if cmd == nil {
		t.Fatal("expected command from tool-start boundary flush")
	}

	beforeAck := stripAnsi(model.View().Content)
	if strings.Contains(beforeAck, "read_file") {
		t.Fatalf("expected pending tool row to be hidden before boundary flush ack, got: %q", beforeAck)
	}

	updated, _ = model.Update(askBoundaryFlushedMsg{CallID: "call-1", Name: "read_file"})
	model = updated.(askStreamModel)

	afterAck := stripAnsi(model.View().Content)
	if !strings.Contains(afterAck, "read_file") {
		t.Fatalf("expected pending tool row after boundary flush ack, got: %q", afterAck)
	}
}

func TestProgressiveCollectorCapturesBridgeTextWhenDrained(t *testing.T) {
	bridge := newAskProgressiveBridge(ui.DefaultStreamBufferSize)
	collector := &textCollector{}
	events := collector.wrapEvents(bridge.Events())

	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for range events {
		}
	}()

	if err := bridge.HandleEvent(llm.Event{Type: llm.EventTextDelta, Text: "hello "}); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	if err := bridge.HandleEvent(llm.Event{Type: llm.EventTextDelta, Text: "world"}); err != nil {
		t.Fatalf("HandleEvent() error = %v", err)
	}
	bridge.CloseSuccess()

	collector.Wait()
	<-drained

	if got := collector.Text(); got != "hello world" {
		t.Fatalf("collector.Text() = %q, want %q", got, "hello world")
	}
}

type nonProgressiveBlockingStream struct {
	ctx    context.Context
	sent   int
	total  int
	closed chan struct{}
}

func (s *nonProgressiveBlockingStream) Recv() (llm.Event, error) {
	if s.sent < s.total {
		s.sent++
		return llm.Event{Type: llm.EventTextDelta, Text: "chunk"}, nil
	}
	<-s.ctx.Done()
	return llm.Event{}, s.ctx.Err()
}

func (s *nonProgressiveBlockingStream) Close() error {
	close(s.closed)
	return nil
}

func TestNonProgressiveConsumerWriteErrorCancellationUnblocksProducer(t *testing.T) {
	adapter := ui.NewStreamAdapter(1)
	streamCtx, cancelStream := context.WithCancel(context.Background())
	defer cancelStream()

	stream := &nonProgressiveBlockingStream{
		ctx:    streamCtx,
		total:  8,
		closed: make(chan struct{}),
	}
	errChan := make(chan error, 1)
	go func() {
		defer stream.Close()
		adapter.ProcessStream(streamCtx, stream)
		errChan <- nil
	}()

	_, _, writeErr := streamJSONEvents(streamCtx, adapter.Events(), newJSONEmitter(failingJSONWriter{}))
	if writeErr == nil {
		t.Fatal("expected streamJSONEvents to fail")
	}

	cancelStream()

	select {
	case err := <-errChan:
		if err != nil {
			t.Fatalf("producer error = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("producer remained blocked after consumer cancellation")
	}

	select {
	case <-stream.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stream.Close was not reached after consumer cancellation")
	}
}

func TestAskViewNoForcedTrailingNewline(t *testing.T) {
	model := newAskStreamModel()
	model.pausedForExternalUI = true
	model.cachedContent = "content"
	model.contentDirty = false

	view := model.View().Content
	if strings.HasSuffix(view, "\n") {
		t.Fatalf("unexpected trailing newline in view output: %q", view)
	}
}

func TestNewAskRendererProgramUsesProvidedEventChannel(t *testing.T) {
	events := make(chan ui.StreamEvent)
	cfg := &config.Config{DefaultProvider: "test"}

	model, program := newAskRendererProgram(cfg, nil, nil, nil, events)
	if program == nil {
		t.Fatal("expected Bubble Tea program")
	}
	if model.streamChan != events {
		t.Fatal("expected program model to listen on the provided event channel")
	}
}

// simulateStreaming splits content into chunks like an LLM would stream it
func simulateStreaming(content string) []string {
	var chunks []string
	words := strings.Fields(content)

	for i, word := range words {
		if i > 0 {
			chunks = append(chunks, " ")
		}
		chunks = append(chunks, word)

		// Add newlines where they appear in original
		idx := strings.Index(content, word)
		if idx >= 0 {
			afterWord := idx + len(word)
			if afterWord < len(content) && content[afterWord] == '\n' {
				chunks = append(chunks, "\n")
				if afterWord+1 < len(content) && content[afterWord+1] == '\n' {
					chunks = append(chunks, "\n")
				}
			}
		}
	}

	return chunks
}

func TestHeadingSpacing(t *testing.T) {
	md := `## Headers at All Levels

# Header 1

## Header 2

### Header 3

#### Header 4

##### Header 5

###### Header 6`

	width := 80
	tracker := ui.NewToolTracker()

	// Simulate streaming - write in small chunks
	chunkSize := 20
	for i := 0; i < len(md); i += chunkSize {
		end := i + chunkSize
		if end > len(md) {
			end = len(md)
		}
		chunk := md[i:end]
		tracker.AddTextSegment(chunk, width)
	}

	// Complete and get final output
	tracker.CompleteTextSegments(func(text string) string {
		return renderMd(text, width)
	})

	result := tracker.FlushAllRemaining(width, 0, renderMd)

	// Strip ANSI for counting
	clean := stripAnsi(result.ToPrint)

	// Count lines
	lines := strings.Split(clean, "\n")

	// Should be 14 lines: 7 headers + 6 blank lines + 1 leading blank
	// Allow for trimming variations
	lineCount := len(lines)
	for lineCount > 0 && strings.TrimSpace(lines[lineCount-1]) == "" {
		lineCount--
	}
	if lineCount == 0 {
		t.Fatalf("No output produced")
	}

	// Log actual output for debugging
	t.Logf("Line count: %d", lineCount)
	for i, line := range lines[:lineCount] {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			t.Logf("Line %d: (blank)", i)
		} else {
			t.Logf("Line %d: %q", i, trimmed)
		}
	}

	// We expect around 14 lines (7 headers + 6 blank lines + potentially leading blank)
	if lineCount < 13 || lineCount > 15 {
		t.Errorf("Expected 13-15 lines, got %d", lineCount)
	}
}

// TestAskJSONFlagIsRegistered verifies the --json flag is wired onto askCmd.
func TestAskJSONFlagIsRegistered(t *testing.T) {
	f := askCmd.Flags().Lookup("json")
	if f == nil {
		t.Fatal("expected --json flag to be registered on askCmd")
	}
	if f.Value.Type() != "bool" {
		t.Fatalf("expected --json to be bool, got %s", f.Value.Type())
	}
	if f.DefValue != "false" {
		t.Fatalf("expected --json default = false, got %s", f.DefValue)
	}
}

// TestAskJSONEndToEndSmoke drives a realistic ask session through the JSON
// emitter and verifies the full JSONL stream is well-formed with the
// expected ordering and event types.
func TestAskJSONEndToEndSmoke(t *testing.T) {
	events := []ui.StreamEvent{
		ui.TextEvent("Let me check that file.\n"),
		ui.ToolStartEvent("call-1", "read_file", "(announcement.md)", json.RawMessage(`{"path":"announcement.md"}`)),
		ui.ToolEndEvent("call-1", "read_file", "(announcement.md)", true),
		ui.TextEvent("Here is what I found:\n"),
		ui.TextEvent("Ruby 4.0 was released.\n"),
		ui.UsageEvent(120, 45, 30, 0),
		ui.DoneEvent(45),
	}

	ch := make(chan ui.StreamEvent, len(events))
	for _, ev := range events {
		ch <- ev
	}
	close(ch)

	var buf bytes.Buffer
	emitter := newJSONEmitter(&buf)
	stats := ui.NewSessionStats()

	info := sessionInfo{
		SessionID: "smoke-1",
		Provider:  "mock",
		Model:     "mock-model",
		Tools:     "read_file",
		Search:    false,
	}

	if err := streamJSON(context.Background(), ch, emitter, stats, info); err != nil {
		t.Fatalf("streamJSON returned error: %v", err)
	}

	var decoded []map[string]any
	for i, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			t.Fatalf("line %d is not valid JSON: %v\nline: %q", i, err, line)
		}
		decoded = append(decoded, obj)
	}

	var gotTypes []string
	for _, ev := range decoded {
		gotTypes = append(gotTypes, ev["type"].(string))
	}

	wantTypes := []string{
		"session.started",
		"text.delta",
		"tool.started",
		"tool.completed",
		"text.delta",
		"text.delta",
		"usage",
		"stats",
		"done",
	}
	if len(gotTypes) != len(wantTypes) {
		t.Fatalf("event count = %d (%v), want %d (%v)", len(gotTypes), gotTypes, len(wantTypes), wantTypes)
	}
	for i, want := range wantTypes {
		if gotTypes[i] != want {
			t.Errorf("event %d type = %q, want %q", i, gotTypes[i], want)
		}
	}

	for i, ev := range decoded {
		if got, ok := ev["seq"].(float64); !ok || int(got) != i {
			t.Errorf("event %d seq = %v, want %d", i, ev["seq"], i)
		}
		if _, ok := ev["ts"].(string); !ok {
			t.Errorf("event %d missing ts", i)
		}
	}

	last := decoded[len(decoded)-1]
	secondLast := decoded[len(decoded)-2]
	if secondLast["type"] != "stats" || last["type"] != "done" {
		t.Fatalf("last two events must be stats,done; got %v,%v", secondLast["type"], last["type"])
	}
}

// stripAnsi removes ANSI escape sequences for testing
func stripAnsi(s string) string {
	result := ""
	inEscape := false
	for _, c := range s {
		if c == '\x1b' {
			inEscape = true
			continue
		}
		if inEscape {
			if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
				inEscape = false
			}
			continue
		}
		result += string(c)
	}
	return result
}
