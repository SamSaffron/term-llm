package chat

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/config"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	sessionsui "github.com/samsaffron/term-llm/internal/tui/sessions"
	"github.com/samsaffron/term-llm/internal/ui"
)

func pressPromptHistoryKey(t *testing.T, m *Model, msg tea.KeyPressMsg) *Model {
	t.Helper()
	updated, cmd := m.handleKeyMsg(msg)
	rm, ok := updated.(*Model)
	if !ok {
		t.Fatalf("handleKeyMsg returned %T, want *Model", updated)
	}
	if cmd != nil {
		lookupMsg := cmd()
		updated, cmd = rm.Update(lookupMsg)
		rm, ok = updated.(*Model)
		if !ok {
			t.Fatalf("Update(%T) returned %T, want *Model", lookupMsg, updated)
		}
		if cmd != nil {
			t.Fatalf("Update(%T) returned unexpected follow-up command", lookupMsg)
		}
	}
	return rm
}

func assertQuitCommand(t *testing.T, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected quit command, got nil")
	}
	msg := cmd()
	if _, ok := msg.(tea.QuitMsg); !ok {
		t.Fatalf("expected tea.QuitMsg, got %T", msg)
	}
}

func TestCtrlCCopiesActiveSelection(t *testing.T) {
	m := newTestChatModel(true)
	m.contentLines = []string{"hello world"}
	m.selection = Selection{
		Active: true,
		Anchor: ContentPos{Line: 0, Col: 0},
		Cursor: ContentPos{Line: 0, Col: 5},
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if cmd != nil {
		t.Fatalf("copy selection should not return quit command, got %T", cmd())
	}
	if rm.quitting {
		t.Fatal("Ctrl+C with active selection should copy, not quit")
	}
	if !rm.ctrlCExitArmedUntil.IsZero() {
		t.Fatal("Ctrl+C with active selection should not arm exit confirmation")
	}
	if rm.selection.Active {
		t.Fatal("expected selection to be cleared after copy")
	}
	if rm.copyStatus == "" {
		t.Fatal("expected copy status after Ctrl+C selection copy")
	}
}

func TestCtrlCRequiresConfirmationToQuitWhenIdle(t *testing.T) {
	m := newTestChatModel(true)

	updated, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if cmd == nil {
		t.Fatal("expected footer confirmation command")
	}
	if rm.quitting {
		t.Fatal("first Ctrl+C should not quit")
	}
	if !strings.Contains(rm.footerMessage, "Press Ctrl-C again to exit") {
		t.Fatalf("footerMessage = %q, want confirmation", rm.footerMessage)
	}

	updated, cmd = rm.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm = updated.(*Model)
	if !rm.quitting {
		t.Fatal("second Ctrl+C inside confirmation window should quit")
	}
	assertQuitCommand(t, cmd)
}

func TestCtrlCFirstCancelsEmbeddedApprovalThenRequiresConfirmation(t *testing.T) {
	m := newTestChatModel(true)
	m.approvalModel = tools.NewEmbeddedApprovalModel(t.TempDir()+"/file.go", false, 80)
	doneCh := make(chan tools.ApprovalResult, 1)
	m.approvalDoneCh = doneCh
	m.pausedForExternalUI = true

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if rm.quitting {
		t.Fatal("first Ctrl+C during approval should cancel, not quit")
	}
	if rm.approvalModel != nil || rm.approvalDoneCh != nil || rm.pausedForExternalUI {
		t.Fatal("expected embedded approval to be cleared on Ctrl+C")
	}
	select {
	case result := <-doneCh:
		if !result.Cancelled || result.Choice != tools.ApprovalChoiceCancelled {
			t.Fatalf("approval result = %#v, want cancelled", result)
		}
	default:
		t.Fatal("expected Ctrl+C to unblock approval waiter")
	}

	updated, _ = rm.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm = updated.(*Model)
	if rm.quitting {
		t.Fatal("first idle Ctrl+C after cancellation should only arm exit")
	}
	updated, cmd := rm.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm = updated.(*Model)
	if !rm.quitting {
		t.Fatal("second idle Ctrl+C should quit")
	}
	assertQuitCommand(t, cmd)
}

func TestCtrlCFirstArmsExitFromDialog(t *testing.T) {
	m := newTestChatModel(true)
	m.dialog.ShowContent("Help", "body")

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if rm.quitting {
		t.Fatal("first Ctrl+C should not quit from dialog")
	}
	if rm.dialog.IsOpen() {
		t.Fatal("expected dialog to be closed when arming Ctrl+C exit")
	}
	if !strings.Contains(rm.footerMessage, "Press Ctrl-C again to exit") {
		t.Fatalf("footerMessage = %q, want confirmation", rm.footerMessage)
	}
}

func TestCtrlCFirstCancelsStreamingThenRequiresConfirmation(t *testing.T) {
	m := newTestChatModel(true)
	cancelled := false
	m.streaming = true
	m.streamCancelFunc = func() { cancelled = true }

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if rm.quitting {
		t.Fatal("first Ctrl+C while streaming should cancel, not quit")
	}
	if !cancelled {
		t.Fatal("expected Ctrl+C to cancel active stream")
	}
	if rm.streamCancelFunc != nil {
		t.Fatal("expected streamCancelFunc to be cleared")
	}
	if !rm.ctrlCExitArmedUntil.IsZero() {
		t.Fatal("stream cancellation should not arm immediate exit")
	}

	updated, _ = rm.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm = updated.(*Model)
	if rm.quitting {
		t.Fatal("first idle Ctrl+C after stream cancellation should only arm exit")
	}
	updated, cmd := rm.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm = updated.(*Model)
	if !rm.quitting {
		t.Fatal("second idle Ctrl+C should quit")
	}
	assertQuitCommand(t, cmd)
}

func TestCtrlCRequiresConfirmationAfterStreamDone(t *testing.T) {
	m := newTestChatModel(true)
	m.streaming = true
	m.streamStartTime = time.Now().Add(-time.Second)
	cancelled := false
	m.streamCancelFunc = func() { cancelled = true }

	updated, _ := m.Update(streamEventMsg{event: ui.DoneEvent(0)})
	rm := updated.(*Model)
	if rm.streaming {
		t.Fatal("stream should be settled after done event")
	}
	if rm.streamCancelFunc != nil {
		t.Fatal("streamCancelFunc should be cleared after normal stream completion")
	}
	if !cancelled {
		t.Fatal("stream cancel func should be called to release context resources")
	}

	updated, cmd := rm.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm = updated.(*Model)
	if rm.quitting {
		t.Fatal("first Ctrl+C after stream completion should arm exit, not quit")
	}
	if cmd == nil {
		t.Fatal("expected footer confirmation command")
	}
	if strings.Contains(rm.footerMessage, "Interrupted current response/tool") {
		t.Fatalf("footerMessage = %q, want exit confirmation not phantom interrupt", rm.footerMessage)
	}
	if !strings.Contains(rm.footerMessage, "Press Ctrl-C again to exit") {
		t.Fatalf("footerMessage = %q, want exit confirmation", rm.footerMessage)
	}
}

func TestCtrlCConfirmationExpires(t *testing.T) {
	m := newTestChatModel(true)
	m.ctrlCExitArmedUntil = time.Now().Add(-time.Second)

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if rm.quitting {
		t.Fatal("expired Ctrl+C confirmation should re-arm, not quit")
	}
	if !rm.ctrlCExitArmedUntil.After(time.Now()) {
		t.Fatal("expected Ctrl+C confirmation window to be re-armed")
	}
}

func TestNonCtrlCDisarmsExitConfirmation(t *testing.T) {
	m := newTestChatModel(true)
	m.ctrlCExitArmedUntil = time.Now().Add(time.Second)

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'x', Text: "x"})
	rm := updated.(*Model)
	if !rm.ctrlCExitArmedUntil.IsZero() {
		t.Fatal("expected non-Ctrl+C keypress to disarm exit confirmation")
	}
}

func TestCtrlCCancelsStreamingApprovalWithoutQuitting(t *testing.T) {
	m := newTestChatModel(true)
	cancelled := false
	m.streaming = true
	m.streamCancelFunc = func() { cancelled = true }
	m.approvalModel = tools.NewEmbeddedApprovalModel(t.TempDir()+"/file.go", false, 80)
	doneCh := make(chan tools.ApprovalResult, 1)
	m.approvalDoneCh = doneCh
	m.pausedForExternalUI = true

	updated, _ := m.Update(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	rm := updated.(*Model)
	if rm.quitting {
		t.Fatal("first Ctrl+C during streaming approval should cancel, not quit")
	}
	if !cancelled {
		t.Fatal("expected stream cancel func to be called")
	}
	if rm.approvalModel != nil || rm.approvalDoneCh != nil || rm.pausedForExternalUI {
		t.Fatal("expected embedded approval to be cleared")
	}
	select {
	case result := <-doneCh:
		if !result.Cancelled || result.Choice != tools.ApprovalChoiceCancelled {
			t.Fatalf("approval result = %#v, want cancelled", result)
		}
	default:
		t.Fatal("expected Ctrl+C to unblock approval waiter")
	}
}

func TestPromptHistoryRecallsCurrentSessionThenCrossSessionByDate(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	current := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock-model", Mode: session.ModeChat, Agent: "jarvis"}
	if err := store.Create(ctx, current); err != nil {
		t.Fatalf("Create current: %v", err)
	}
	otherAgent := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock-model", Mode: session.ModeChat, Agent: "reviewer"}
	if err := store.Create(ctx, otherAgent); err != nil {
		t.Fatalf("Create otherAgent: %v", err)
	}
	defaultAgent := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock-model", Mode: session.ModeChat}
	if err := store.Create(ctx, defaultAgent); err != nil {
		t.Fatalf("Create defaultAgent: %v", err)
	}

	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	addPrompt := func(sess *session.Session, text string, at time.Time) session.Message {
		t.Helper()
		msg := session.NewMessage(sess.ID, llm.UserText(text), -1)
		msg.CreatedAt = at
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%q): %v", text, err)
		}
		return *msg
	}
	currentOlder := addPrompt(current, "current older words", base.Add(time.Minute))
	_ = addPrompt(defaultAgent, "default agent external words", base.Add(2*time.Minute))
	_ = addPrompt(otherAgent, "other agent external words", base.Add(3*time.Minute))
	currentNewer := addPrompt(current, "current newer words", base.Add(4*time.Minute))

	m := newTestChatModel(false)
	m.store = session.NewLoggingStore(store, nil)
	m.sess = current
	m.agentName = "jarvis"
	m.messages = []session.Message{currentOlder, currentNewer}
	m.setTextareaValue("draft words")

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.textarea.Value(); got != "current newer words" {
		t.Fatalf("after up textarea = %q, want latest current-session prompt", got)
	}
	if got := m.textarea.Column(); got != len("current newer words") {
		t.Fatalf("after up cursor column = %d, want end", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.textarea.Value(); got != "current older words" {
		t.Fatalf("after second up textarea = %q, want older current-session prompt", got)
	}
	if got := m.textarea.Column(); got != len("current older words") {
		t.Fatalf("after second up cursor column = %d, want end", got)
	}

	updated, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("expected async global prompt history lookup command")
	}
	if got := m.textarea.Value(); got != "current older words" {
		t.Fatalf("before async global lookup textarea = %q, want current prompt to remain", got)
	}
	if !m.promptHistory.lookupPending {
		t.Fatal("expected global prompt history lookup to be marked pending")
	}
	updated, followup := m.Update(cmd())
	m = updated.(*Model)
	if followup != nil {
		t.Fatal("global prompt history lookup returned unexpected follow-up command")
	}
	if m.promptHistory.lookupPending {
		t.Fatal("expected global prompt history lookup pending flag to clear")
	}
	if got := m.textarea.Value(); got != "other agent external words" {
		t.Fatalf("after third up textarea = %q, want newest cross-session prompt across agents", got)
	}
	if got := m.textarea.Column(); got != len("other agent external words") {
		t.Fatalf("after third up cursor column = %d, want end", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.textarea.Value(); got != "default agent external words" {
		t.Fatalf("after fourth up textarea = %q, want older cross-session prompt across agents", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.textarea.Value(); got != "other agent external words" {
		t.Fatalf("after down textarea = %q, want newer cross-session prompt", got)
	}
	if got := m.textarea.Column(); got != 0 {
		t.Fatalf("after down cursor column = %d, want start", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.textarea.Value(); got != "current older words" {
		t.Fatalf("after second down textarea = %q, want oldest current-session prompt", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.textarea.Value(); got != "current newer words" {
		t.Fatalf("after third down textarea = %q, want newer current-session prompt", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.textarea.Value(); got != "draft words" {
		t.Fatalf("after fourth down textarea = %q, want restored draft", got)
	}
	if got := m.textarea.Column(); got != 0 {
		t.Fatalf("after fourth down cursor column = %d, want start", got)
	}
}

func TestPromptHistoryWorksDuringStreamingInterjectionComposer(t *testing.T) {
	store, err := session.NewStore(session.Config{Enabled: true, Path: t.TempDir() + "/sessions.db"})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	sess := &session.Session{ID: session.NewID(), Provider: "mock", Model: "mock-model", Mode: session.ModeChat, Agent: "jarvis"}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create sess: %v", err)
	}
	msg := session.NewMessage(sess.ID, llm.UserText("streaming history prompt"), -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	m := newTestChatModel(false)
	m.store = store
	m.sess = sess
	m.agentName = "jarvis"
	m.streaming = true
	m.messages = []session.Message{*msg}
	m.setTextareaValue("interrupt draft")

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.textarea.Value(); got != "streaming history prompt" {
		t.Fatalf("streaming up textarea = %q, want prompt history", got)
	}
}

func TestPromptHistoryFallsBackToInMemoryMessagesWithoutStore(t *testing.T) {
	m := newTestChatModel(true)
	m.store = &session.NoopStore{}
	m.messages = []session.Message{
		{Role: llm.RoleUser, TextContent: "first in-memory prompt"},
		{Role: llm.RoleAssistant, TextContent: "reply"},
		{Role: llm.RoleUser, TextContent: "second in-memory prompt"},
	}
	m.setTextareaValue("")

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.textarea.Value(); got != "second in-memory prompt" {
		t.Fatalf("first up textarea = %q, want latest in-memory prompt", got)
	}
	if got := m.textarea.Column(); got != len("second in-memory prompt") {
		t.Fatalf("first up cursor column = %d, want end", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.textarea.Value(); got != "first in-memory prompt" {
		t.Fatalf("second up textarea = %q, want older in-memory prompt", got)
	}
	if got := m.textarea.Column(); got != len("first in-memory prompt") {
		t.Fatalf("second up cursor column = %d, want end", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.textarea.Value(); got != "second in-memory prompt" {
		t.Fatalf("down textarea = %q, want newer in-memory prompt", got)
	}
	if got := m.textarea.Column(); got != 0 {
		t.Fatalf("down cursor column = %d, want start", got)
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("second down textarea = %q, want restored empty draft", got)
	}
	if got := m.textarea.Column(); got != 0 {
		t.Fatalf("second down cursor column = %d, want start", got)
	}
}

func TestPromptHistoryBoundaryKeysDoNotScrollViewportWhenStoreUnavailable(t *testing.T) {
	m := newTestChatModel(true)
	m.store = nil
	m.setTextareaValue("")
	m.viewport.SetContent(strings.Repeat("line\n", 200))
	m.viewport.GotoBottom()
	bottomOffset := m.viewport.YOffset()
	if bottomOffset == 0 {
		t.Fatal("precondition: expected scrollable viewport at bottom")
	}

	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyUp})
	if got := m.viewport.YOffset(); got != bottomOffset {
		t.Fatalf("up at empty composer scrolled viewport from %d to %d", bottomOffset, got)
	}

	m.viewport.GotoTop()
	m = pressPromptHistoryKey(t, m, tea.KeyPressMsg{Code: tea.KeyDown})
	if got := m.viewport.YOffset(); got != 0 {
		t.Fatalf("down at inactive empty composer scrolled viewport to %d", got)
	}
}

func TestHandlePasteMsg_SlashPathDoesNotLeaveCompletionsVisible(t *testing.T) {
	m := newTestChatModel(false)

	_, _ = m.Update(tea.PasteMsg{Content: "/tmp/not-a-chat-command"})

	if got := m.textarea.Value(); got != "/tmp/not-a-chat-command" {
		t.Fatalf("textarea value = %q, want pasted path", got)
	}
	if m.completions.IsVisible() {
		t.Fatal("pasting a non-command absolute path should not leave completions visible")
	}
}

func TestHandleKeyMsg_SlashPathSubmitsAsChatMessage(t *testing.T) {
	m := newTestChatModel(false)
	const pastedPath = "/tmp/not-a-chat-command"

	_, _ = m.Update(tea.PasteMsg{Content: pastedPath})
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(m.messages) == 0 {
		t.Fatal("expected pasted slash path to submit as a chat message")
	}
	last := m.messages[len(m.messages)-1]
	if last.Role != llm.RoleUser {
		t.Fatalf("last message role = %q, want user", last.Role)
	}
	if last.TextContent != pastedPath {
		t.Fatalf("last message text = %q, want %q", last.TextContent, pastedPath)
	}
}

func TestHandleKeyMsg_StreamingSlashShowsLocalCompletions(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: '/', Text: "/"})
	rm := result.(*Model)
	if !rm.completions.IsVisible() {
		t.Fatal("expected slash completions while streaming")
	}
	got := completionNames(rm.completions.filtered)
	for _, want := range []string{"help", "stats", "effort", "thinking"} {
		if !containsString(got, want) {
			t.Fatalf("streaming completions missing %q: %v", want, got)
		}
	}
	for _, reject := range []string{"model", "new", "handover"} {
		if containsString(got, reject) {
			t.Fatalf("streaming completions included unsafe command %q: %v", reject, got)
		}
	}
}

func TestHandleKeyMsg_StreamingEffortAutocomplete(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.providerKey = "openai"
	m.providerName = "openai"
	m.modelName = "gpt-5.4-high"
	m.sess = &session.Session{ID: "sess-stream-complete", ProviderKey: "openai", Model: "gpt-5.4-high"}
	m.setTextareaValue("/effort m")

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'e', Text: "e"})
	rm := result.(*Model)
	if !rm.completions.IsVisible() {
		t.Fatal("expected effort argument completions while streaming")
	}
	got := completionNames(rm.completions.filtered)
	if !containsString(got, "effort medium") {
		t.Fatalf("streaming effort completions missing medium: %v", got)
	}
	if containsString(got, "effort max") {
		t.Fatalf("streaming effort completions included unsupported max: %v", got)
	}
}

func TestHandleKeyMsg_StreamingCommandPaletteShowsLocalCompletions(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
	rm := result.(*Model)
	if got := rm.textarea.Value(); got != "/" {
		t.Fatalf("textarea = %q, want slash", got)
	}
	if !rm.completions.IsVisible() {
		t.Fatal("expected command palette completions while streaming")
	}
	if containsString(completionNames(rm.completions.filtered), "model") {
		t.Fatalf("streaming command palette included unsafe /model: %v", completionNames(rm.completions.filtered))
	}
}

func TestHandleKeyMsg_ShiftTabTogglesYoloDuringStreaming(t *testing.T) {
	m := newTestChatModel(false)
	approvalMgr := tools.NewApprovalManager(tools.NewToolPermissions())
	approvalMgr.PolicyReviewFunc = func(ctx context.Context, req tools.PolicyReviewRequest) (tools.PolicyDecision, error) {
		return tools.PolicyDecision{Allowed: true, Rationale: "ok"}, nil
	}
	m.SetApprovalManager(approvalMgr)
	m.streaming = true
	m.phase = "Thinking"
	m.setTextareaValue("draft interjection")

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})

	if !m.streaming {
		t.Fatal("expected stream to keep running")
	}
	if m.currentApprovalMode() != tools.ModeAuto {
		t.Fatalf("expected first Shift+Tab to enable auto mode, got %v", m.currentApprovalMode())
	}
	if got := m.textarea.Value(); got != "draft interjection" {
		t.Fatalf("expected composer draft to remain unchanged, got %q", got)
	}

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if !m.yolo {
		t.Fatal("expected second Shift+Tab to enable yolo mode")
	}
	if !approvalMgr.YoloEnabled() {
		t.Fatal("expected approval manager yolo mode to be enabled")
	}

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.yolo {
		t.Fatal("expected third Shift+Tab to disable yolo mode")
	}
	if approvalMgr.YoloEnabled() {
		t.Fatal("expected approval manager yolo mode to be disabled")
	}
}

func TestHandleKeyMsg_ShiftTabAutoApprovesActiveApprovalPrompt(t *testing.T) {
	m := newTestChatModel(true)
	approvalMgr := tools.NewApprovalManager(tools.NewToolPermissions())
	m.SetApprovalManager(approvalMgr)
	m.approvalModel = tools.NewEmbeddedApprovalModel(t.TempDir()+"/file.go", false, 80)
	doneCh := make(chan tools.ApprovalResult, 1)
	m.approvalDoneCh = doneCh
	m.pausedForExternalUI = true

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})

	select {
	case result := <-doneCh:
		if result.Choice != tools.ApprovalChoiceOnce {
			t.Fatalf("expected active approval to proceed once, got %q", result.Choice)
		}
	default:
		t.Fatal("expected active approval prompt to receive a result")
	}
	if m.approvalModel != nil {
		t.Fatal("expected active approval model to be cleared")
	}
	if m.approvalDoneCh != nil {
		t.Fatal("expected approval done channel to be cleared")
	}
	if m.pausedForExternalUI {
		t.Fatal("expected external UI pause to clear")
	}
}

func TestUpdate_ApprovalRequestAutoApprovesWhenYoloAlreadyEnabled(t *testing.T) {
	m := newTestChatModel(true)
	m.yolo = true
	doneCh := make(chan tools.ApprovalResult, 1)

	_, _ = m.Update(ApprovalRequestMsg{
		Path:   t.TempDir() + "/file.go",
		DoneCh: doneCh,
	})

	select {
	case result := <-doneCh:
		if result.Choice != tools.ApprovalChoiceOnce {
			t.Fatalf("expected yolo approval to proceed once, got %q", result.Choice)
		}
	default:
		t.Fatal("expected approval request to auto-approve in yolo mode")
	}
	if m.approvalModel != nil {
		t.Fatal("expected no embedded approval model in yolo mode")
	}
}

func TestHandleKeyMsg_SessionListEnterResumesSession(t *testing.T) {
	sessionID := "sess-handler-resume-1"
	sess := &session.Session{ID: sessionID, Number: 11, Name: "picked session"}
	store := &mockStore{
		sessions: map[string]*session.Session{sessionID: sess},
	}

	m := newCmdTestModel(store)
	m.dialog.ShowSessionList([]DialogItem{
		{ID: sessionID, Label: "picked session"},
	}, "")

	result, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	rm := result.(*Model)

	if rm.dialog.IsOpen() {
		t.Fatal("expected dialog to close after selecting a session")
	}
	if !rm.quitting {
		t.Fatal("expected selecting a session to quit for relaunch")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestResumeBrowserEnterRequestsRelaunch(t *testing.T) {
	sessionID := "sess-handler-resume-browser-1"
	store := &mockStore{
		summaries: []session.SessionSummary{{
			ID:        sessionID,
			Number:    11,
			Name:      "picked session",
			Summary:   "Discussed rollout checks and release notes",
			UpdatedAt: time.Now(),
		}},
	}

	m := newCmdTestModel(store)
	result, _ := m.cmdResume(nil)
	rm := result.(*Model)

	result, cmd := rm.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	rm = result.(*Model)
	if cmd == nil {
		t.Fatal("expected enter to return a resume selection command")
	}

	msg := cmd()
	chatMsg, ok := msg.(sessionsui.ChatMsg)
	if !ok {
		t.Fatalf("expected sessions ChatMsg, got %T", msg)
	}
	if chatMsg.SessionID != sessionID {
		t.Fatalf("expected selected session ID %q, got %q", sessionID, chatMsg.SessionID)
	}

	result, quitCmd := rm.Update(chatMsg)
	rm = result.(*Model)
	if quitCmd == nil {
		t.Fatal("expected resume selection to request program quit")
	}
	if !rm.quitting {
		t.Fatal("expected selecting a browser session to quit for relaunch")
	}
	if rm.RequestedResumeSessionID() != sessionID {
		t.Fatalf("expected pending resume session ID %q, got %q", sessionID, rm.RequestedResumeSessionID())
	}
}

func TestResumeBrowserCloseReturnsToChatWithoutLosingDraft(t *testing.T) {
	sessionID := "sess-handler-resume-browser-close-1"
	store := &mockStore{
		summaries: []session.SessionSummary{{
			ID:        sessionID,
			Number:    12,
			Name:      "picked session",
			UpdatedAt: time.Now(),
		}},
	}

	m := newCmdTestModel(store)
	m.setTextareaValue("draft follow-up")
	result, _ := m.cmdResume(nil)
	rm := result.(*Model)

	result, cmd := rm.Update(tea.KeyPressMsg{Code: 'q', Text: "q"})
	rm = result.(*Model)
	if cmd == nil {
		t.Fatal("expected q to return a close command")
	}

	msg := cmd()
	if _, ok := msg.(sessionsui.CloseMsg); !ok {
		t.Fatalf("expected sessions CloseMsg, got %T", msg)
	}

	result, _ = rm.Update(msg)
	rm = result.(*Model)
	if rm.resumeBrowserMode {
		t.Fatal("expected resume browser mode to close")
	}
	if rm.quitting {
		t.Fatal("expected close to return to chat instead of quitting")
	}
	if got := rm.textarea.Value(); got != "draft follow-up" {
		t.Fatalf("expected draft input to be preserved, got %q", got)
	}
}

func TestHandleKeyMsg_StreamingCancelInterjectionRestoresComposerAndShowsStopping(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Running shell sleep"
	m.pendingInterjection = "old"
	m.setTextareaValue("stop sleeping")

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cancelCalls != 1 {
		t.Fatalf("expected stream cancel to be called once, got %d", cancelCalls)
	}
	if got := m.textarea.Value(); got != "stop sleeping" {
		t.Fatalf("expected textarea draft restored after cancel interjection, got %q", got)
	}
	if m.pendingInterjection != "" {
		t.Fatalf("expected pendingInterjection to be cleared, got %q", m.pendingInterjection)
	}
	if m.phase != "Stopping..." {
		t.Fatalf("expected stopping phase after cancel interjection, got %q", m.phase)
	}
	if got := m.interruptNotice; got == "" {
		t.Fatal("expected interrupt notice after cancellation")
	}
}

func TestHandleKeyMsg_StreamingEnterOnEmptyComposerShowsHint(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.setTextareaValue("   ")

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})

	if cancelCalls != 0 {
		t.Fatalf("expected empty enter to avoid cancellation, got %d cancel calls", cancelCalls)
	}
	if m.phase != "Type to interject, attach an image, or press Esc to cancel" {
		t.Fatalf("expected empty enter hint phase, got %q", m.phase)
	}
}

func TestHandleKeyMsg_CancelsSelectedPendingInterjection(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	firstID := m.nextPendingInterjectionID()
	secondID := m.nextPendingInterjectionID()
	m.applyInterruptAction(firstID, "first note", llm.InterruptInterject)
	m.applyInterruptAction(secondID, "second note", llm.InterruptInterject)

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.selectedInterjection != 0 {
		t.Fatalf("selectedInterjection after up = %d, want 0", m.selectedInterjection)
	}
	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyBackspace})

	if got := m.engine.DrainInterjection(); got != "second note" {
		t.Fatalf("queued interjections after cancel = %q, want second note", got)
	}
	if len(m.pendingInterjections) != 1 || m.pendingInterjections[0].ID != secondID {
		t.Fatalf("pending stack = %#v, want only second", m.pendingInterjections)
	}
}

func TestHandleKeyMsg_StreamingAsyncClassificationFeelsImmediate(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse("interject")
	m.setTextareaValue("also check the schema")

	_, cmd := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected async classification command")
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("expected textarea to clear immediately, got %q", got)
	}
	if got := m.pendingInterjection; got != "also check the schema" {
		t.Fatalf("expected pending interjection to render immediately, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "deciding" {
		t.Fatalf("expected deciding state immediately, got %q", got)
	}

	msg := cmd()
	if _, ok := msg.(interruptClassifiedMsg); !ok {
		t.Fatalf("expected interruptClassifiedMsg, got %T", msg)
	}
	_, _ = m.handleInterruptClassified(msg.(interruptClassifiedMsg))

	if got := m.pendingInterruptUI; got != "interject" {
		t.Fatalf("expected interject state after classification, got %q", got)
	}
	if got := m.engine.DrainInterjection(); got != "also check the schema" {
		t.Fatalf("expected engine interjection to be queued, got %q", got)
	}
}

func TestHandleInterruptClassified_StreamAlreadyFinishedRestoresDraft(t *testing.T) {
	m := newTestChatModel(false)
	m.activeInterruptSeq = 7
	m.pendingInterjection = "keep sleeping"
	m.pendingInterruptUI = "deciding"

	_, cmd := m.handleInterruptClassified(interruptClassifiedMsg{
		RequestID: 7,
		Content:   "keep sleeping",
		Action:    llm.InterruptInterject,
	})
	if cmd != nil {
		t.Fatal("expected no follow-up command when restoring draft")
	}
	if got := m.textarea.Value(); got != "keep sleeping" {
		t.Fatalf("expected interjection text restored to composer, got %q", got)
	}
	if m.streaming {
		t.Fatal("expected stream to remain finished")
	}
	if got := m.pendingInterjection; got != "" {
		t.Fatalf("expected pending interjection cleared after restore, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "" {
		t.Fatalf("expected pending interrupt UI cleared after restore, got %q", got)
	}
	if got := len(m.messages); got != 0 {
		t.Fatalf("expected restored draft not to auto-send, got %d messages", got)
	}
}

func TestStreamEventInterjection_DoesNotDiscardNewerPendingClassification(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse("interject")

	firstID := m.nextPendingInterjectionID()
	m.applyInterruptAction(firstID, "first note", llm.InterruptInterject)
	secondID := m.nextPendingInterjectionID()
	cmd := m.queueInterruptClassification(secondID, "second note", nil)
	if cmd == nil {
		t.Fatal("expected async interrupt classification command")
	}
	requestID := m.activeInterruptSeq
	if requestID == 0 {
		t.Fatal("expected active interrupt request id")
	}

	_, _ = m.Update(streamEventMsg{event: ui.InterjectionEvent("first note", firstID)})

	if got := m.pendingInterjection; got != "second note" {
		t.Fatalf("pendingInterjection after first event = %q, want %q", got, "second note")
	}
	if got := m.pendingInterjectionID; got != secondID {
		t.Fatalf("pendingInterjectionID after first event = %q, want %q", got, secondID)
	}
	if got := m.pendingInterruptUI; got != "deciding" {
		t.Fatalf("pendingInterruptUI after first event = %q, want deciding", got)
	}
	if got := m.activeInterruptSeq; got != requestID {
		t.Fatalf("activeInterruptSeq after first event = %d, want %d", got, requestID)
	}
	if len(m.pendingInterjections) != 1 || m.pendingInterjections[0].ID != secondID {
		t.Fatalf("pending stack after first event = %#v, want only second", m.pendingInterjections)
	}

	msg := cmd()
	classified, ok := msg.(interruptClassifiedMsg)
	if !ok {
		t.Fatalf("expected interruptClassifiedMsg, got %T", msg)
	}
	if classified.InterjectionID != secondID {
		t.Fatalf("classified interjection id = %q, want %q", classified.InterjectionID, secondID)
	}
	_, _ = m.handleInterruptClassified(classified)

	if got := m.pendingInterruptUI; got != "interject" {
		t.Fatalf("pendingInterruptUI after classification = %q, want interject", got)
	}
	if got := m.engine.DrainInterjection(); got != "first note\nsecond note" {
		t.Fatalf("expected both FIFO interjections to remain queued, got %q", got)
	}
}

func TestStreamEventInterjection_MatchesByIDNotText(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true

	firstID := m.nextPendingInterjectionID()
	m.applyInterruptAction(firstID, "same text", llm.InterruptInterject)
	secondID := m.nextPendingInterjectionID()
	m.applyInterruptAction(secondID, "same text", llm.InterruptInterject)

	_, _ = m.Update(streamEventMsg{event: ui.InterjectionEvent("same text", firstID)})

	if got := m.pendingInterjectionID; got != secondID {
		t.Fatalf("pendingInterjectionID after stale event = %q, want %q", got, secondID)
	}
	if got := m.pendingInterjection; got != "same text" {
		t.Fatalf("pendingInterjection after stale event = %q, want same text", got)
	}
	if got := m.pendingInterruptUI; got != "interject" {
		t.Fatalf("pendingInterruptUI after stale event = %q, want interject", got)
	}
	if len(m.pendingInterjections) != 1 || m.pendingInterjections[0].ID != secondID {
		t.Fatalf("pending stack after stale event = %#v, want only second", m.pendingInterjections)
	}
	if got := m.engine.DrainInterjection(); got != "same text\nsame text" {
		t.Fatalf("expected both same-text interjections to remain queued FIFO, got %q", got)
	}
}

func TestRestorePendingInterjectionDraft_RestoresImageParts(t *testing.T) {
	m := newTestChatModel(false)
	m.engine.QueueInterjection(llm.QueuedInterjection{
		ID:      "img-draft",
		Message: llm.UserImageMessage("image/png", base64.StdEncoding.EncodeToString([]byte("img")), "describe"),
	})

	m.restorePendingInterjectionDraft()

	if got := m.textarea.Value(); got != "describe" {
		t.Fatalf("restored text = %q, want describe", got)
	}
	if len(m.images) != 1 || m.images[0].MediaType != "image/png" || string(m.images[0].Data) != "img" {
		t.Fatalf("restored images = %#v, want png img", m.images)
	}
}

func TestStreamDone_PendingInterjectRestoresDraftWithoutEngineResidual(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.pendingInterjection = "keep sleeping"
	m.pendingInterruptUI = "interject"

	_, cmd := m.Update(streamEventMsg{event: ui.DoneEvent(0)})
	if cmd == nil {
		t.Fatal("expected command batch from stream completion")
	}
	if m.streaming {
		t.Fatal("expected streaming to stop after done event")
	}
	if got := m.textarea.Value(); got != "keep sleeping" {
		t.Fatalf("expected pending interjection restored to composer, got %q", got)
	}
	if got := m.pendingInterjection; got != "" {
		t.Fatalf("expected pending interjection cleared after restore, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "" {
		t.Fatalf("expected pending interrupt UI cleared after restore, got %q", got)
	}
}

func TestStreamError_PendingInterjectRestoresDraftWithoutEngineResidual(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.pendingInterjection = "keep sleeping"
	m.pendingInterruptUI = "interject"

	_, cmd := m.Update(streamEventMsg{event: ui.ErrorEvent(context.Canceled)})
	if cmd != nil {
		t.Fatal("expected no follow-up command on error")
	}
	if m.streaming {
		t.Fatal("expected streaming to stop after error")
	}
	if got := m.textarea.Value(); got != "keep sleeping" {
		t.Fatalf("expected pending interjection restored to composer, got %q", got)
	}
	if got := m.pendingInterjection; got != "" {
		t.Fatalf("expected pending interjection cleared after restore, got %q", got)
	}
	if got := m.pendingInterruptUI; got != "" {
		t.Fatalf("expected pending interrupt UI cleared after restore, got %q", got)
	}
}

func TestHandleKeyMsg_StreamingEscCancelsActiveStream(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.pendingAssistantMsgID = 42
	m.pendingAssistantSnapshot = llm.AssistantText("partial answer")
	m.pendingAssistantSnapshotSet = true
	m.completedAssistantTurns = 1

	cancelCalls := 0
	m.streamCancelFunc = func() {
		cancelCalls++
	}

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEsc})

	if cancelCalls != 1 {
		t.Fatalf("expected esc to call stream cancel once, got %d", cancelCalls)
	}
	if !m.streaming {
		t.Fatal("expected stream to remain active until the stream goroutine exits")
	}
	if !m.isStreamCancelRequested() {
		t.Fatal("expected esc to mark stream cancellation as pending")
	}
	if got := m.phase; got != "Stopping..." {
		t.Fatalf("phase after esc = %q, want %q", got, "Stopping...")
	}
	if got := m.pendingAssistantMsgID; got != 42 {
		t.Fatalf("pendingAssistantMsgID after esc = %d, want 42", got)
	}
	if !m.pendingAssistantSnapshotSet {
		t.Fatal("expected pending assistant snapshot to remain available until stream shutdown")
	}
	if got := m.pendingAssistantSnapshot.Parts[0].Text; got != "partial answer" {
		t.Fatalf("pending assistant snapshot text after esc = %q, want %q", got, "partial answer")
	}
	if got := m.completedAssistantTurns; got != 1 {
		t.Fatalf("completedAssistantTurns after esc = %d, want 1", got)
	}
}

func TestUpdate_StreamCancelTimeoutForcesCancelledState(t *testing.T) {
	m := newTestChatModel(false)
	done := make(chan struct{})
	m.streamGeneration = 1
	m.streaming = true
	m.streamDone = done
	m.setStreamCancelRequested(true)
	m.phase = "Stopping..."
	m.currentResponse.WriteString("partial answer")

	updated, _ := m.Update(streamCancelTimeoutMsg{done: done, generation: 1})
	m = updated.(*Model)

	if m.streaming {
		t.Fatal("expected timeout to clear streaming state")
	}
	if m.isStreamCancelRequested() {
		t.Fatal("expected timeout to clear cancellation request")
	}
}

func TestUpdate_StreamCancelTimeoutIgnoresStaleStream(t *testing.T) {
	m := newTestChatModel(false)
	oldDone := make(chan struct{})
	newDone := make(chan struct{})
	m.streamGeneration = 2
	m.streaming = true
	m.streamDone = newDone
	m.setStreamCancelRequested(true)
	m.phase = "Stopping..."

	updated, _ := m.Update(streamCancelTimeoutMsg{done: oldDone, generation: 1})
	m = updated.(*Model)

	if !m.streaming {
		t.Fatal("expected stale timeout to leave current stream active")
	}
	if !m.isStreamCancelRequested() {
		t.Fatal("expected stale timeout to preserve current cancellation request")
	}
}

func TestUpdate_StreamCancelTimeoutIgnoresLateDone(t *testing.T) {
	m := newTestChatModel(false)
	done := make(chan struct{})
	m.streamGeneration = 1
	m.streaming = true
	m.streamDone = done
	m.setStreamCancelRequested(true)
	m.autoSendQueue = []string{"next"}

	updated, _ := m.Update(streamCancelTimeoutMsg{done: done, generation: 1})
	m = updated.(*Model)

	updated, cmd := m.Update(streamEventMsg{event: ui.DoneEvent(0), generation: 1})
	m = updated.(*Model)

	if cmd != nil {
		t.Fatal("expected late done after forced cancel to be ignored without scheduling commands")
	}
	if m.streaming {
		t.Fatal("expected model to remain non-streaming after ignored late done")
	}
	if len(m.autoSendQueue) != 1 || m.autoSendQueue[0] != "next" {
		t.Fatalf("late done mutated auto-send queue: %#v", m.autoSendQueue)
	}
}

func TestUpdate_StreamEventIgnoresStaleGeneration(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.streamGeneration = 2
	m.autoSendQueue = []string{"next"}

	updated, cmd := m.Update(streamEventMsg{event: ui.DoneEvent(0), generation: 1})
	m = updated.(*Model)

	if cmd != nil {
		t.Fatal("expected stale stream event to be ignored without scheduling commands")
	}
	if !m.streaming {
		t.Fatal("expected stale done to leave current stream active")
	}
	if len(m.autoSendQueue) != 1 || m.autoSendQueue[0] != "next" {
		t.Fatalf("stale done mutated auto-send queue: %#v", m.autoSendQueue)
	}
}

func stubClipboard(t *testing.T) {
	orig := readClipboardImage
	readClipboardImage = func() ([]byte, error) { return nil, nil }
	t.Cleanup(func() { readClipboardImage = orig })
}

func TestUpdate_PasteMsg_RoutedToEmbeddedAskUserModel(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("draft")
	m.askUserModel = tools.NewEmbeddedAskUserModel([]tools.AskUserQuestion{{
		Header:   "Q1",
		Question: "Choose",
		Options: []tools.AskUserOption{
			{Label: "A"},
			{Label: "B"},
		},
	}}, 80)
	m.askUserDoneCh = make(chan []tools.AskUserAnswer, 1)
	m.askUserModel.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeyDown})
	m.askUserModel.UpdateEmbedded(tea.KeyPressMsg{Code: tea.KeyDown})

	result, _ := m.Update(tea.PasteMsg{Content: "custom answer"})
	rm := result.(*Model)

	if got := rm.textarea.Value(); got != "draft" {
		t.Fatalf("expected composer draft to remain unchanged, got %q", got)
	}
	view := ui.StripANSI(rm.askUserModel.View().Content)
	if !strings.Contains(view, "custom answer") {
		t.Fatalf("expected pasted text in ask_user view, got %q", view)
	}
}

func TestUpdate_PasteMsg_RoutedToHandoverInstructionsEditor(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("draft")
	m.handoverPreview = newHandoverPreviewModel("document", "developer", "", 80, m.styles)
	m.handoverPreview.editing = true

	result, _ := m.Update(tea.PasteMsg{Content: "extra context"})
	rm := result.(*Model)

	if got := rm.textarea.Value(); got != "draft" {
		t.Fatalf("expected composer draft to remain unchanged, got %q", got)
	}
	if got := rm.handoverPreview.Instructions(); got != "extra context" {
		t.Fatalf("expected pasted handover instructions, got %q", got)
	}
}

func TestUpdate_PasteMsg_RoutedToModelPickerFilter(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("draft")
	m.dialog.ShowModelPicker("mock:mock-model", []ProviderInfo{{
		Name:   "mock",
		Models: []string{"mock-model", "other-model"},
	}}, nil)

	result, _ := m.Update(tea.PasteMsg{Content: "other"})
	rm := result.(*Model)

	if got := rm.textarea.Value(); got != "draft" {
		t.Fatalf("expected composer draft to remain unchanged, got %q", got)
	}
	if got := rm.dialog.Query(); got != "other" {
		t.Fatalf("expected pasted model filter query, got %q", got)
	}
}

func TestUpdate_PasteMsg_RoutedToMCPPickerFilter(t *testing.T) {
	m := newTestChatModel(false)
	m.setTextareaValue("draft")
	m.dialog.dialogType = DialogMCPPicker
	m.dialog.items = []DialogItem{{ID: "server-one", Label: "server-one"}}
	m.dialog.filtered = m.dialog.items

	result, _ := m.Update(tea.PasteMsg{Content: "server"})
	rm := result.(*Model)

	if got := rm.textarea.Value(); got != "draft" {
		t.Fatalf("expected composer draft to remain unchanged, got %q", got)
	}
	if got := rm.dialog.Query(); got != "server" {
		t.Fatalf("expected pasted MCP filter query, got %q", got)
	}
}

func TestPasteCollapse_LargeSingleLinePasteGoesToTextarea(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	pasteText := strings.Repeat("dictated words ", 20)

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})

	if len(m.pasteChunks) != 0 {
		t.Fatalf("expected no collapsed paste for single-line text, got %d", len(m.pasteChunks))
	}
	if got := m.textarea.Value(); strings.Contains(got, "[Pasted text #") || !strings.Contains(got, "dictated words") {
		t.Fatalf("expected literal single-line paste to be inserted without placeholder, got %q", got)
	}
}

func TestPasteCollapse_LargeMultilinePasteBecomesInlinePlaceholder(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	pasteText := strings.Repeat("abcdefghij", 6) + "\n" + strings.Repeat("klmnopqrst", 6)

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})

	// Placeholder should be in the textarea, not the literal paste
	got := m.textarea.Value()
	if got == pasteText {
		t.Fatal("expected paste to be collapsed, but literal text appeared in textarea")
	}
	if !strings.Contains(got, "[Pasted text #1") {
		t.Fatalf("expected inline placeholder in textarea, got %q", got)
	}

	// Actual content stored in pasteChunks map
	if len(m.pasteChunks) != 1 {
		t.Fatalf("expected 1 paste chunk, got %d", len(m.pasteChunks))
	}
	if m.pasteChunks[1] != pasteText {
		t.Fatal("paste chunk content mismatch")
	}
}

func TestPasteCollapse_SmallPasteGoesToTextarea(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	// Under 100 chars — should pass through
	pasteText := "short paste that is under the hundred char threshold"

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})

	if len(m.pasteChunks) != 0 {
		t.Fatalf("expected no collapsed paste for short text, got %d", len(m.pasteChunks))
	}
	if got := m.textarea.Value(); got != pasteText {
		t.Fatalf("expected literal paste in textarea, got %q", got)
	}
}

func TestPasteCollapse_StreamingInterjectionExpandsPlaceholderOnSend(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)
	m.streaming = true
	pasteText := strings.Repeat("stream alpha ", 12) + "\n" + strings.Repeat("stream beta ", 12)

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})
	if !strings.Contains(m.textarea.Value(), "[Pasted text #1") {
		t.Fatalf("precondition: expected collapsed placeholder, got %q", m.textarea.Value())
	}

	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})

	if len(m.pasteChunks) != 0 {
		t.Fatalf("expected paste chunks cleared after streaming send, got %d", len(m.pasteChunks))
	}
	if got := m.engine.DrainInterjection(); got != pasteText {
		t.Fatalf("streaming interjection = %q, want expanded paste %q", got, pasteText)
	}
}

func TestPasteCollapse_StreamingInterruptClassificationUsesExpandedPlaceholder(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)
	m.streaming = true
	m.fastProvider = llm.NewMockProvider("fast").AddTextResponse("interject")
	pasteText := strings.Repeat("async alpha ", 12) + "\n" + strings.Repeat("async beta ", 12)

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})
	_, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected interrupt classification command")
	}
	if len(m.pasteChunks) != 0 {
		t.Fatalf("expected paste chunks cleared after queueing classification, got %d", len(m.pasteChunks))
	}

	gotMsg := cmd()
	msg, ok := gotMsg.(interruptClassifiedMsg)
	if !ok {
		t.Fatalf("expected interruptClassifiedMsg, got %T", gotMsg)
	}
	if msg.Content != pasteText {
		t.Fatalf("classification content = %q, want expanded paste %q", msg.Content, pasteText)
	}
}

func TestStreamingSlashThinkingExecutesLocally(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.reasoningConfig = config.DefaultReasoningConfig()
	m.setTextareaValue("/thinking expanded")

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if cmd == nil {
		// Footer timer may be nil in some test paths; command execution itself is what matters.
	}
	if m.reasoningModeOverride != config.ReasoningDisplayExpanded {
		t.Fatalf("reasoningModeOverride = %q, want expanded", m.reasoningModeOverride)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("expected command to clear composer, got %q", got)
	}
	if len(m.pendingInterjections) != 0 || m.pendingInterjection != "" {
		t.Fatalf("/thinking should not queue interjection, pending=%q stack=%d", m.pendingInterjection, len(m.pendingInterjections))
	}
}

func TestStreamingSideSlashInvalidatesCachedAltScreenBackgroundBeforeOverlay(t *testing.T) {
	m := newTestChatModel(true)
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	m.streaming = true
	m.streamRenderMinInterval = 0
	m.SetSideQuestionProviderFactory(func(_, _ string) (llm.Provider, error) {
		return llm.NewMockProvider("side").AddTextResponse("answer"), nil
	})
	m.images = []ImageAttachment{{MediaType: "image/png", Data: []byte("image")}}
	const submitted = "/side stale-composer-token"
	m.setTextareaValue(submitted)

	// Prime the actual cached viewport.View path with the stale command. This
	// models the append/viewport cache observed in a real alt-screen recording;
	// merely rendering the composer does not exercise that cache.
	_ = m.View()
	m.viewport.SetContent(submitted)
	m.viewCache.lastViewportView = ""
	m.viewCache.lastRenderedVersion = m.viewCache.contentVersion
	if view := ui.StripANSI(m.View().Content); !strings.Contains(view, submitted) {
		t.Fatalf("precondition: cached streaming view does not contain %q: %q", submitted, view)
	}
	if !strings.Contains(ui.StripANSI(m.viewCache.lastViewportView), submitted) {
		t.Fatalf("precondition: lastViewportView was not primed with %q: %q", submitted, m.viewCache.lastViewportView)
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("expected side question stream command")
	}
	if got := fmt.Sprintf("%T", cmd()); got != "tea.sequenceMsg" {
		t.Fatalf("side submission command = %s, want ordered clear-screen/listener sequence", got)
	}
	if m.viewCache.lastViewportView != "" || !m.viewCache.lastSetContentAt.IsZero() {
		t.Fatalf("streaming viewport cache was not invalidated: view=%q setAt=%v", m.viewCache.lastViewportView, m.viewCache.lastSetContentAt)
	}
	if m.viewCache.lastContentHistoryPlusStream || m.viewCache.lastStreamingContent != "" || m.viewCache.lastContentStr != "" || m.contentLines != nil {
		t.Fatalf("streaming append cache was not invalidated: historyPlusStream=%v stream=%q content=%q lines=%#v",
			m.viewCache.lastContentHistoryPlusStream, m.viewCache.lastStreamingContent, m.viewCache.lastContentStr, m.contentLines)
	}
	background := ui.StripANSI(m.viewAltScreen())
	if strings.Contains(background, submitted) {
		t.Fatalf("submitted composer remained in overlay background: %q", background)
	}
	if view := ui.StripANSI(m.renderSideQuestionOverlay(background)); strings.Contains(view, submitted) {
		t.Fatalf("submitted composer remained beneath side-question overlay: %q", view)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("textarea = %q, want cleared", got)
	}
	if m.completions.IsVisible() {
		t.Fatal("expected command completions to be hidden")
	}
	if len(m.images) != 1 {
		t.Fatalf("attachments = %d, want preserved for the main composer", len(m.images))
	}
}

func TestStreamingSlashCommandPrefixQueuesInterjection(t *testing.T) {
	for _, input := range []string{"/s", "/se", "/system-prompt-question", "/i", "/t"} {
		t.Run(input, func(t *testing.T) {
			m := newTestChatModel(false)
			m.streaming = true
			m.phase = "Thinking"
			m.fastProvider = nil
			m.setTextareaValue(input)

			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(*Model)
			if len(m.pendingInterjections) != 1 {
				t.Fatalf("expected slash prefix to queue as interjection, pending stack=%d", len(m.pendingInterjections))
			}
			if got := m.pendingInterjections[0].Text; got != input {
				t.Fatalf("pending interjection = %q, want %q", got, input)
			}
		})
	}
}

func TestStreamingSideEffectSlashCommandsQueueInterjection(t *testing.T) {
	for _, input := range []string{"/search", "/fast", "/export", "/system new prompt", "/inspect"} {
		t.Run(input, func(t *testing.T) {
			m := newTestChatModel(false)
			m.streaming = true
			m.phase = "Thinking"
			m.fastProvider = nil
			m.setTextareaValue(input)

			updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(*Model)
			if len(m.pendingInterjections) != 1 {
				t.Fatalf("expected side-effect command to queue as interjection, pending stack=%d", len(m.pendingInterjections))
			}
			if got := m.pendingInterjections[0].Text; got != input {
				t.Fatalf("pending interjection = %q, want %q", got, input)
			}
		})
	}
}

func TestStreamingUnknownSlashStillQueuesInterjection(t *testing.T) {
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Thinking"
	m.fastProvider = nil
	m.setTextareaValue("/tmp/foo")

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if len(m.pendingInterjections) != 1 {
		t.Fatalf("expected slash path to queue as interjection, pending stack=%d", len(m.pendingInterjections))
	}
	if got := m.pendingInterjections[0].Text; got != "/tmp/foo" {
		t.Fatalf("pending interjection = %q, want /tmp/foo", got)
	}
}

func TestPasteCollapse_CtrlEExpandsPlaceholderAtCursor(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)
	pasteText := strings.Repeat("alpha ", 20) + "\n" + strings.Repeat("beta ", 20)

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})
	placeholder := m.textarea.Value()
	if !strings.Contains(placeholder, "[Pasted text #1") {
		t.Fatalf("precondition: expected collapsed placeholder, got %q", placeholder)
	}

	_, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	if m.toolsExpanded {
		t.Fatal("ctrl+e on a paste placeholder should not toggle tool expansion")
	}
	if len(m.pasteChunks) != 0 {
		t.Fatalf("expected paste chunk to be consumed, got %d", len(m.pasteChunks))
	}
	got := m.textarea.Value()
	if strings.Contains(got, "[Pasted text #") {
		t.Fatalf("expected placeholder to expand, got %q", got)
	}
	if !strings.Contains(got, "alpha") || !strings.Contains(got, "beta") {
		t.Fatalf("expected expanded paste content in textarea, got %q", got)
	}
}

func TestPasteCollapse_CtrlEBubblesWhenCursorNotOnPlaceholder(t *testing.T) {
	m := newTestChatModel(false)
	content := "line one\nline two"
	placeholder := pastePlaceholder(1, content)
	m.pasteChunks = map[int]string{1: content}
	m.setTextareaValue("prefix " + placeholder)
	m.textarea.MoveToBegin()

	_, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	if !m.toolsExpanded {
		t.Fatal("expected ctrl+e away from placeholder to toggle tool expansion")
	}
	if len(m.pasteChunks) != 1 {
		t.Fatalf("expected paste chunk to remain, got %d", len(m.pasteChunks))
	}
	if got := m.textarea.Value(); !strings.Contains(got, placeholder) {
		t.Fatalf("expected placeholder to remain, got %q", got)
	}
}

func TestPasteCollapse_CtrlEAdjacentBoundaryExpandsLeftPlaceholder(t *testing.T) {
	m := newTestChatModel(false)
	first := "first line\nfirst line again"
	second := "second line\nsecond line again"
	firstPlaceholder := pastePlaceholder(1, first)
	secondPlaceholder := pastePlaceholder(2, second)
	m.pasteChunks = map[int]string{1: first, 2: second}
	m.setTextareaValue(firstPlaceholder + secondPlaceholder)
	m.moveTextareaCursorToByteOffset(len(firstPlaceholder))

	_, _ = m.Update(tea.KeyPressMsg{Code: 'e', Mod: tea.ModCtrl})

	if m.toolsExpanded {
		t.Fatal("ctrl+e on adjacent placeholder boundary should expand instead of toggling tools")
	}
	if _, ok := m.pasteChunks[1]; ok {
		t.Fatal("expected first placeholder chunk to be consumed")
	}
	if _, ok := m.pasteChunks[2]; !ok {
		t.Fatal("expected second placeholder chunk to remain")
	}
	if got := m.textarea.Value(); got != first+secondPlaceholder {
		t.Fatalf("textarea = %q, want %q", got, first+secondPlaceholder)
	}
}

func TestPasteCollapse_MultiplePastesGetUniquePlaceholders(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	longPaste := strings.Repeat("x", 60) + "\n" + strings.Repeat("y", 60)
	for i := 0; i < 3; i++ {
		_, _ = m.handlePasteMsg(tea.PasteMsg{Content: longPaste})
	}

	if len(m.pasteChunks) != 3 {
		t.Fatalf("expected 3 paste chunks, got %d", len(m.pasteChunks))
	}

	got := m.textarea.Value()
	for i := 1; i <= 3; i++ {
		placeholder := fmt.Sprintf("[Pasted text #%d", i)
		if !strings.Contains(got, placeholder) {
			t.Fatalf("expected %q in textarea, got %q", placeholder, got)
		}
	}
}

func TestPasteCollapse_ExpandPlaceholdersOnSend(t *testing.T) {
	m := newTestChatModel(false)
	content := strings.Repeat("y", 110)
	m.pasteChunks = map[int]string{
		1: content,
	}

	input := fmt.Sprintf("fix this: [Pasted text #1 +%d chars]", len(content))
	expanded := m.expandPastePlaceholders(input)

	expected := "fix this: " + content
	if expanded != expected {
		t.Fatalf("expected %q, got %q", expected, expanded)
	}
	if len(m.pasteChunks) != 0 {
		t.Fatal("expected pasteChunks cleared after expansion")
	}
}

func TestPasteCollapse_MultilinePlaceholderShowsLines(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	// Multi-line paste over 100 chars
	pasteText := "line one is here with some extra text to pad\nline two also has plenty of content in it\nline three as well with more words\nline four rounds it out nicely"

	_, _ = m.handlePasteMsg(tea.PasteMsg{Content: pasteText})

	got := m.textarea.Value()
	if !strings.Contains(got, "+4 lines]") {
		t.Fatalf("expected '+4 lines' in placeholder, got %q", got)
	}
}

func TestHandlePasteMsg_ShowsCommandCompletionsForPastedSlashCommand(t *testing.T) {
	stubClipboard(t)
	m := newTestChatModel(false)

	_, _ = m.Update(tea.PasteMsg{Content: "/"})

	if got := m.textarea.Value(); got != "/" {
		t.Fatalf("expected pasted slash in composer, got %q", got)
	}
	if !m.completions.IsVisible() {
		t.Fatal("expected completions to be visible after pasting a slash command")
	}
}

func TestHandleKeyMsg_ShiftTabSkipsAutoWhenGuardianUnavailable(t *testing.T) {
	m := newTestChatModel(false)
	approvalMgr := tools.NewApprovalManager(tools.NewToolPermissions())
	m.SetApprovalManager(approvalMgr)

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})

	if got := m.currentApprovalMode(); got != tools.ModeYolo {
		t.Fatalf("approval mode = %v, want yolo when auto reviewer is unavailable", got)
	}
	if !m.yolo || !approvalMgr.YoloEnabled() {
		t.Fatal("expected Shift+Tab to skip unavailable auto and enable yolo")
	}
}
