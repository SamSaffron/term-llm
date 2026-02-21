package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/testutil"
)

// fakeBotSender is a botSender that records all Send calls for test assertions.
type fakeBotSender struct {
	mu      sync.Mutex
	sent    []string // text of each Send call, in order
	nextID  int      // auto-incrementing MessageID
	sendErr error    // if non-nil, returned on the very first Send call
}

func (f *fakeBotSender) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.sendErr != nil {
		err := f.sendErr
		f.sendErr = nil // consume once
		return tgbotapi.Message{}, err
	}

	var text string
	switch v := c.(type) {
	case tgbotapi.MessageConfig:
		text = v.Text
	case tgbotapi.EditMessageTextConfig:
		text = v.Text
	}
	f.sent = append(f.sent, text)

	id := f.nextID
	f.nextID++
	return tgbotapi.Message{MessageID: id}, nil
}

func (f *fakeBotSender) lastText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return ""
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeBotSender) allTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	copy(out, f.sent)
	return out
}

// newTestMgrAndSession builds a minimal manager and session backed by h's engine.
func newTestMgrAndSession(h *testutil.EngineHarness) (*telegramSessionMgr, *telegramSession) {
	mgr := &telegramSessionMgr{
		sessions:       make(map[int64]*telegramSession),
		tickerInterval: 10 * time.Millisecond,
		settings:       Settings{MaxTurns: 5},
	}
	sess := &telegramSession{
		runtime: &SessionRuntime{
			Engine:       h.Engine,
			ProviderName: "mock",
			ModelName:    "test",
		},
	}
	return mgr, sess
}

// --- TestBuildSegment ---

func TestBuildSegment(t *testing.T) {
	cases := []struct {
		prose      string
		tool       string
		phase      string
		withCursor bool
		want       string
	}{
		{prose: "Hello", tool: "", phase: "", withCursor: false, want: "Hello"},
		{prose: "Hello", tool: "", phase: "", withCursor: true, want: "Hello▌"},
		// No leading \n\n when prose is empty.
		{prose: "", tool: "bash", phase: "", withCursor: true, want: "🔧 bash...▌"},
		{prose: "Thinking", tool: "bash", phase: "", withCursor: true, want: "Thinking\n\n🔧 bash...▌"},
		{prose: "", tool: "", phase: "Searching…", withCursor: false, want: "Searching…"},
		{prose: "Result", tool: "", phase: "Done", withCursor: false, want: "Result\n\nDone"},
		{prose: "", tool: "", phase: "", withCursor: true, want: "▌"},
	}

	for _, tc := range cases {
		got := buildSegment(tc.prose, tc.tool, tc.phase, tc.withCursor)
		if got != tc.want {
			t.Errorf("buildSegment(%q, %q, %q, %v) = %q; want %q",
				tc.prose, tc.tool, tc.phase, tc.withCursor, got, tc.want)
		}
	}
}

// --- TestActiveToolDisplay ---

func TestActiveToolDisplay(t *testing.T) {
	cases := []struct {
		tools map[string]string
		want  string
	}{
		{tools: map[string]string{}, want: ""},
		{tools: map[string]string{"id-1": "bash"}, want: "bash"},
		{tools: map[string]string{"id-1": "bash", "id-2": "search"}, want: "2 tools running..."},
		{tools: map[string]string{"id-1": "a", "id-2": "b", "id-3": "c"}, want: "3 tools running..."},
	}
	for _, tc := range cases {
		got := activeToolDisplay(tc.tools)
		if got != tc.want {
			t.Errorf("activeToolDisplay(%v) = %q; want %q", tc.tools, got, tc.want)
		}
	}
}

// --- streamReply tests ---

func TestStreamReply_TextOnly(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("Hello")

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSender{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hi")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	last := bot.lastText()
	if last != "Hello" {
		t.Errorf("lastText = %q; want %q", last, "Hello")
	}
}

func TestStreamReply_ForwardsForceExternalSearch(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("Hello")

	mgr, sess := newTestMgrAndSession(h)
	mgr.settings.Search = true
	mgr.settings.ForceExternalSearch = true
	bot := &fakeBotSender{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hi")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	if len(h.Provider.Requests) == 0 {
		t.Fatal("expected provider request to be recorded")
	}
	lastReq := h.Provider.Requests[len(h.Provider.Requests)-1]
	if !lastReq.Search {
		t.Fatalf("expected request Search=true")
	}
	if !lastReq.ForceExternalSearch {
		t.Fatalf("expected request ForceExternalSearch=true")
	}
}

func TestStreamReply_ToolThenText(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.AddMockTool("my_tool", "tool output")
	h.Provider.AddToolCall("id-1", "my_tool", map[string]any{})
	h.Provider.AddTextResponse("Result")

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSender{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("run tool")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	last := bot.lastText()
	if last != "Result" {
		t.Errorf("lastText = %q; want %q", last, "Result")
	}
}

func TestStreamReply_ToolOnlyNoText(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.AddMockTool("my_tool", "tool output")
	h.Provider.AddToolCall("id-1", "my_tool", map[string]any{})
	h.Provider.AddTextResponse("") // empty final response

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSender{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("run tool")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	last := bot.lastText()
	if last != "(done)" {
		t.Errorf("lastText = %q; want %q", last, "(done)")
	}
}

func TestStreamReply_NoResponse(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("") // no text, no tools

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSender{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hi")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	last := bot.lastText()
	if last != "(no response)" {
		t.Errorf("lastText = %q; want %q", last, "(no response)")
	}
}

// TestStreamReply_ToolNameShownDuringExec verifies that the tool indicator
// appears in at least one interim message while a tool is executing.
// Uses a channel-based synchronization so the test does not rely on timing alone.
func TestStreamReply_ToolNameShownDuringExec(t *testing.T) {
	h := testutil.NewEngineHarness()

	toolStarted := make(chan struct{})
	toolRelease := make(chan struct{})

	slowTool := &testutil.MockTool{
		SpecData: llm.ToolSpec{
			Name:        "slow_tool",
			Description: "slow tool for testing",
			Schema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		ExecuteFn: func(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
			close(toolStarted) // signal: tool execution has begun
			select {
			case <-toolRelease:
			case <-ctx.Done():
			}
			return llm.TextOutput("done"), nil
		},
	}
	h.Registry.Register(slowTool)

	h.Provider.AddToolCall("id-1", "slow_tool", map[string]any{})
	h.Provider.AddTextResponse("ok")

	mgr, sess := newTestMgrAndSession(h)
	mgr.tickerInterval = 5 * time.Millisecond
	bot := &fakeBotSender{}

	done := make(chan error, 1)
	go func() {
		done <- mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("go slow"))
	}()

	// Wait for the tool to start executing (guarantees EventToolExecStart is in-flight).
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	// Give the ticker several intervals to fire and pick up the activeTool state.
	time.Sleep(40 * time.Millisecond) // 8 × 5ms ticks
	close(toolRelease)

	if err := <-done; err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	found := false
	for _, text := range bot.allTexts() {
		if strings.Contains(text, "🔧 slow_tool...") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected tool indicator in sent texts; got: %v", bot.allTexts())
	}

	if last := bot.lastText(); last != "ok" {
		t.Errorf("lastText = %q; want %q", last, "ok")
	}
}

// TestStreamReply_ExactChunkBoundary verifies that a response whose length is
// exactly telegramMaxMessageLen does not produce a bogus "(no response)" or
// "(done)" fallback in a dangling placeholder message.
func TestStreamReply_ExactChunkBoundary(t *testing.T) {
	h := testutil.NewEngineHarness()
	// Build a response that is exactly telegramMaxMessageLen bytes.
	exactResponse := strings.Repeat("x", telegramMaxMessageLen)
	h.Provider.AddTextResponse(exactResponse)

	mgr, sess := newTestMgrAndSession(h)
	// Use a very short ticker so it fires during streaming and triggers the split path.
	mgr.tickerInterval = 1 * time.Millisecond
	bot := &fakeBotSender{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hi")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	texts := bot.allTexts()
	// The content must appear verbatim (possibly with cursor appended in interim edits).
	foundContent := false
	for _, text := range texts {
		if strings.HasPrefix(text, exactResponse) || text == exactResponse {
			foundContent = true
		}
	}
	if !foundContent {
		t.Errorf("expected the exact response in sent texts; no match found in %d messages", len(texts))
	}

	// Must NOT end with a bogus fallback message.
	last := bot.lastText()
	if last == "(no response)" || last == "(done)" {
		t.Errorf("lastText = %q — bogus fallback after exact-chunk-size response", last)
	}
}

func TestStreamReply_PlaceholderSendFails(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("Hello")

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSender{sendErr: errors.New("telegram: forbidden")}

	err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hi"))
	if err == nil {
		t.Fatal("expected streamReply to return error when placeholder Send fails")
	}
	if !strings.Contains(err.Error(), "send placeholder") {
		t.Errorf("error should mention 'send placeholder'; got: %v", err)
	}
}

func TestStreamReply_StreamEventErrorReturnsError(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddError(errors.New("anthropic streaming error: upstream unavailable"))

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSender{}

	err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hi"))
	if err == nil {
		t.Fatal("expected streamReply to return error when stream emits EventError")
	}
	if !strings.Contains(err.Error(), "upstream unavailable") {
		t.Fatalf("expected upstream error in streamReply error, got: %v", err)
	}

	for _, text := range bot.allTexts() {
		if text == "(no response)" || text == "(done)" {
			t.Fatalf("unexpected fallback text after stream error: %q", text)
		}
	}
}

// --- existing tests (unchanged) ---

func TestTelegramSessionMgrGetOrCreate_DoesNotBlockOtherChatsWhileCreating(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	mgr := &telegramSessionMgr{
		sessions: make(map[int64]*telegramSession),
		settings: Settings{
			NewSession: func(ctx context.Context) (*SessionRuntime, error) {
				started <- struct{}{}
				<-release
				return &SessionRuntime{
					ProviderName: "mock",
					ModelName:    "model",
				}, nil
			},
		},
	}

	errCh := make(chan error, 2)
	go func() {
		_, err := mgr.getOrCreate(context.Background(), 1)
		errCh <- err
	}()
	select {
	case <-started:
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("first NewSession call did not start")
	}

	go func() {
		_, err := mgr.getOrCreate(context.Background(), 2)
		errCh <- err
	}()
	select {
	case <-started:
		// This confirms the second chat reached NewSession before the first one completed.
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("second getOrCreate blocked while first session was creating")
	}

	close(release)
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("getOrCreate returned error: %v", err)
		}
	}
}

func TestTelegramSessionMgrResetSessionIfCurrent_RejectsStaleExpectedSession(t *testing.T) {
	var cleanupCalls atomic.Int32
	mgr := &telegramSessionMgr{
		sessions: make(map[int64]*telegramSession),
		settings: Settings{
			NewSession: func(ctx context.Context) (*SessionRuntime, error) {
				return &SessionRuntime{
					ProviderName: "mock",
					ModelName:    "model",
					Cleanup: func() {
						cleanupCalls.Add(1)
					},
				}, nil
			},
		},
	}

	original, err := mgr.getOrCreate(context.Background(), 42)
	if err != nil {
		t.Fatalf("getOrCreate failed: %v", err)
	}

	replacement, replaced, err := mgr.resetSessionIfCurrent(context.Background(), 42, original)
	if err != nil {
		t.Fatalf("resetSessionIfCurrent failed: %v", err)
	}
	if !replaced {
		t.Fatalf("expected first reset to replace session")
	}
	if replacement == original {
		t.Fatalf("expected a new replacement session")
	}

	got, replaced, err := mgr.resetSessionIfCurrent(context.Background(), 42, original)
	if err != nil {
		t.Fatalf("second resetSessionIfCurrent failed: %v", err)
	}
	if replaced {
		t.Fatalf("expected stale reset to be ignored")
	}
	if got != replacement {
		t.Fatalf("expected current replacement session to be returned for stale reset")
	}
	if cleanupCalls.Load() != 2 {
		t.Fatalf("cleanup calls = %d, want 2 (original closed + stale duplicate closed)", cleanupCalls.Load())
	}
}

// --- interrupt tests ---

func TestHandleMessage_InterruptCancelsActiveStream(t *testing.T) {
	h := testutil.NewEngineHarness()

	toolStarted := make(chan struct{})
	toolRelease := make(chan struct{})

	slowTool := &testutil.MockTool{
		SpecData: llm.ToolSpec{
			Name:        "slow_tool",
			Description: "slow tool for testing",
			Schema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		ExecuteFn: func(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
			close(toolStarted)
			select {
			case <-toolRelease:
			case <-ctx.Done():
				return llm.TextOutput("cancelled"), ctx.Err()
			}
			return llm.TextOutput("done"), nil
		},
	}
	h.Registry.Register(slowTool)

	h.Provider.AddToolCall("id-1", "slow_tool", map[string]any{})
	h.Provider.AddTextResponse("final answer")

	mgr, sess := newTestMgrAndSession(h)
	mgr.interruptTimeout = 50 * time.Millisecond
	bot := &fakeBotSender{}

	// Start the stream in a goroutine (simulates first message handling).
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("do something slow"))
	}()

	// Wait for the tool to start executing.
	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	// Now simulate a new message arriving: read cancel state and interrupt.
	sess.cancelMu.Lock()
	doneCh := sess.replyDone
	cancelFn := sess.streamCancel
	sess.cancelMu.Unlock()

	if cancelFn == nil || doneCh == nil {
		t.Fatal("expected streamCancel and replyDone to be set during active stream")
	}

	// Wait the interrupt timeout, then cancel.
	select {
	case <-doneCh:
		t.Fatal("stream should not have finished yet")
	case <-time.After(mgr.interruptTimeout):
		cancelFn()
	}

	// Wait for stream to finish.
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not stop after cancel")
	}

	// streamReply should return nil (not an error) on user interrupt.
	err := <-streamDone
	if err != nil {
		t.Fatalf("streamReply should return nil on user interrupt, got: %v", err)
	}

	// Check that "(interrupted)" appears in the sent messages.
	found := false
	for _, text := range bot.allTexts() {
		if strings.Contains(text, "(interrupted)") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected '(interrupted)' in sent texts; got: %v", bot.allTexts())
	}
}

func TestHandleMessage_GracePeriodAllowsNaturalCompletion(t *testing.T) {
	h := testutil.NewEngineHarness()
	h.Provider.AddTextResponse("quick answer")

	mgr, sess := newTestMgrAndSession(h)
	mgr.interruptTimeout = 2 * time.Second
	bot := &fakeBotSender{}

	// Start the stream.
	streamDone := make(chan error, 1)
	go func() {
		streamDone <- mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("hello"))
	}()

	// Give the stream a moment to start and publish cancel state.
	time.Sleep(30 * time.Millisecond)

	sess.cancelMu.Lock()
	doneCh := sess.replyDone
	cancelFn := sess.streamCancel
	sess.cancelMu.Unlock()

	// doneCh might be nil if stream already finished, or non-nil if still running.
	if cancelFn != nil && doneCh != nil {
		// Simulate interrupt-and-wait: the stream should finish within the grace period.
		select {
		case <-doneCh:
			// Good — finished naturally.
		case <-time.After(mgr.interruptTimeout):
			t.Fatal("stream should have finished within the grace period")
		}
	}

	// Wait for streamReply to return.
	err := <-streamDone
	if err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	// Should NOT contain "(interrupted)".
	for _, text := range bot.allTexts() {
		if strings.Contains(text, "(interrupted)") {
			t.Errorf("unexpected '(interrupted)' in sent texts; got: %v", bot.allTexts())
			break
		}
	}

	// Last message should be the actual answer.
	last := bot.lastText()
	if last != "quick answer" {
		t.Errorf("lastText = %q; want %q", last, "quick answer")
	}
}

func TestHandleMessage_InterruptPreservesHistory(t *testing.T) {
	h := testutil.NewEngineHarness()

	toolStarted := make(chan struct{})

	slowTool := &testutil.MockTool{
		SpecData: llm.ToolSpec{
			Name:        "slow_tool",
			Description: "slow tool for testing",
			Schema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		ExecuteFn: func(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
			close(toolStarted)
			// Block until cancelled.
			<-ctx.Done()
			return llm.TextOutput("cancelled"), ctx.Err()
		},
	}
	h.Registry.Register(slowTool)

	h.Provider.AddToolCall("id-1", "slow_tool", map[string]any{})
	h.Provider.AddTextResponse("should not reach")

	mgr, sess := newTestMgrAndSession(h)
	mgr.interruptTimeout = 50 * time.Millisecond
	bot := &fakeBotSender{}

	// Seed some existing history.
	sess.history = []llm.Message{
		llm.UserText("previous question"),
		llm.AssistantText("previous answer"),
	}

	streamDone := make(chan error, 1)
	go func() {
		streamDone <- mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("do slow thing"))
	}()

	select {
	case <-toolStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("tool never started")
	}

	// Cancel the stream.
	sess.cancelMu.Lock()
	cancelFn := sess.streamCancel
	sess.cancelMu.Unlock()
	cancelFn()

	err := <-streamDone
	if err != nil {
		t.Fatalf("streamReply should return nil on interrupt, got: %v", err)
	}

	// Verify history was preserved: should contain old history + new user message.
	sess.mu.Lock()
	histLen := len(sess.history)
	sess.mu.Unlock()

	// At minimum: 2 (old) + 1 (new user) = 3
	if histLen < 3 {
		t.Fatalf("expected at least 3 messages in history after interrupt, got %d", histLen)
	}

	// The user message should be present.
	sess.mu.Lock()
	foundUser := false
	for _, msg := range sess.history {
		if msg.Role == llm.RoleUser {
			for _, p := range msg.Parts {
				if p.Text == "do slow thing" {
					foundUser = true
				}
			}
		}
	}
	sess.mu.Unlock()

	if !foundUser {
		t.Error("expected interrupted user message in history")
	}
}

func TestTelegramSessionMgrRunStoreOpWithTimeout_UsesLiveContext(t *testing.T) {
	mgr := &telegramSessionMgr{
		store: &session.NoopStore{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var sawCanceled bool
	mgr.runStoreOp(ctx, "sess-1", "op", func(opCtx context.Context) error {
		sawCanceled = opCtx.Err() != nil
		return nil
	})
	if !sawCanceled {
		t.Fatalf("runStoreOp should pass through canceled context")
	}

	var sawLive bool
	var sawDeadline bool
	mgr.runStoreOpWithTimeout("sess-1", "op_timeout", func(opCtx context.Context) error {
		sawLive = opCtx.Err() == nil
		_, sawDeadline = opCtx.Deadline()
		return nil
	})
	if !sawLive {
		t.Fatalf("runStoreOpWithTimeout should use a live context")
	}
	if !sawDeadline {
		t.Fatalf("runStoreOpWithTimeout should set a deadline")
	}
}

// --- fakeFileGetter for photo download testing ---

type fakeFileGetter struct {
	fileURL string
	fileErr error
}

func (f *fakeFileGetter) GetFile(config tgbotapi.FileConfig) (tgbotapi.File, error) {
	return tgbotapi.File{FileID: config.FileID}, f.fileErr
}

func (f *fakeFileGetter) GetFileDirectURL(fileID string) (string, error) {
	if f.fileErr != nil {
		return "", f.fileErr
	}
	return f.fileURL, nil
}

func TestDownloadTelegramPhoto_PicksLargest(t *testing.T) {
	// Start a test HTTP server serving an image
	ts := newTestImageServer(t, []byte("fake-jpeg-data"))
	defer ts.Close()

	fg := &fakeFileGetter{fileURL: ts.URL}
	photos := []tgbotapi.PhotoSize{
		{FileID: "small", Width: 100, Height: 100},
		{FileID: "large", Width: 800, Height: 600},
	}

	mediaType, b64, filePath, err := downloadTelegramPhoto(fg, photos)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if filePath != "" {
		defer os.Remove(filePath)
	}
	if mediaType == "" {
		t.Fatal("expected non-empty media type")
	}
	if b64 == "" {
		t.Fatal("expected non-empty base64 data")
	}
	if filePath == "" {
		t.Fatal("expected non-empty file path")
	}
}

func TestDownloadTelegramPhoto_EmptyPhotos(t *testing.T) {
	fg := &fakeFileGetter{}
	_, _, _, err := downloadTelegramPhoto(fg, nil)
	if err == nil {
		t.Fatal("expected error for empty photos")
	}
}

func TestCollectUserText(t *testing.T) {
	msg := llm.UserImageMessage("image/jpeg", "data", "caption text")
	got := collectUserText(msg)
	if got != "caption text" {
		t.Fatalf("collectUserText = %q, want %q", got, "caption text")
	}

	msg2 := llm.UserText("hello world")
	got2 := collectUserText(msg2)
	if got2 != "hello world" {
		t.Fatalf("collectUserText = %q, want %q", got2, "hello world")
	}
}

// --- image output test ---

func TestStreamReply_ToolImagesAreSent(t *testing.T) {
	h := testutil.NewEngineHarness()
	// Create a temp image file.
	tmpFile := t.TempDir() + "/test.png"
	if err := os.WriteFile(tmpFile, []byte("PNG-DATA"), 0644); err != nil {
		t.Fatal(err)
	}

	imageTool := &testutil.MockTool{
		SpecData: llm.ToolSpec{
			Name:        "image_tool",
			Description: "generates images",
			Schema: map[string]interface{}{
				"type":       "object",
				"properties": map[string]interface{}{},
			},
		},
		ExecuteFn: func(ctx context.Context, args json.RawMessage) (llm.ToolOutput, error) {
			return llm.ToolOutput{
				Content: "Generated image",
				Images:  []string{tmpFile},
			}, nil
		},
	}
	h.Registry.Register(imageTool)

	h.Provider.AddToolCall("id-1", "image_tool", map[string]any{})
	h.Provider.AddTextResponse("Here's your image")

	mgr, sess := newTestMgrAndSession(h)
	bot := &fakeBotSenderWithPhotos{}

	if err := mgr.streamReply(context.Background(), bot, sess, 42, llm.UserText("generate image")); err != nil {
		t.Fatalf("streamReply returned error: %v", err)
	}

	if len(bot.photos) == 0 {
		t.Fatal("expected at least one photo to be sent")
	}
}

// fakeBotSenderWithPhotos extends fakeBotSender to detect PhotoConfig sends.
type fakeBotSenderWithPhotos struct {
	fakeBotSender
	photos []tgbotapi.PhotoConfig
}

func (f *fakeBotSenderWithPhotos) Send(c tgbotapi.Chattable) (tgbotapi.Message, error) {
	if photo, ok := c.(tgbotapi.PhotoConfig); ok {
		f.mu.Lock()
		f.photos = append(f.photos, photo)
		f.mu.Unlock()
	}
	return f.fakeBotSender.Send(c)
}

// newTestImageServer creates a test HTTP server that serves the given data.
func newTestImageServer(t *testing.T, data []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(data)
	}))
}
