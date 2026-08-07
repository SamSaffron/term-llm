package chat

import (
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/clipboard"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/ui"
)

func assistantCopyMessage(responseID, text string) session.Message {
	return session.Message{
		Role:        llm.RoleAssistant,
		ResponseID:  responseID,
		Parts:       []llm.Part{{Type: llm.PartText, Text: text}},
		TextContent: text,
	}
}

func TestResolveCompletedAssistantResponseNoCopyableResponses(t *testing.T) {
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: "question"},
		{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartToolResult, Text: "tool output"}}},
		{Role: llm.RoleSystem, TextContent: "system"},
		{Role: llm.RoleDeveloper, TextContent: "developer"},
		{Role: llm.RoleEvent, TextContent: "event"},
	}
	got := resolveCompletedAssistantResponse(messages, 1)
	if got.Found || got.Text != "" || got.LoadedCount != 0 {
		t.Fatalf("resolution = %+v, want structured not-found with zero loaded", got)
	}
}

func TestResolveCompletedAssistantResponseGroupsAndOrdersResponses(t *testing.T) {
	messages := []session.Message{
		{Role: llm.RoleUser, TextContent: "first question"},
		assistantCopyMessage("response-1", "I will inspect."),
		{Role: llm.RoleTool, ResponseID: "response-1", Parts: []llm.Part{{Type: llm.PartToolResult, Text: "secret tool output"}}, TextContent: "secret tool output"},
		{
			Role:       llm.RoleAssistant,
			ResponseID: "response-1",
			Parts: []llm.Part{
				{Type: llm.PartText, Text: "Inspection complete."},
				{Type: llm.PartToolCall, Text: "hidden tool call"},
			},
		},
		{Role: llm.RoleUser, TextContent: "second question"},
		assistantCopyMessage("response-2", "| A | B |\n|---|---|\n| 1 | 2 |\n\n```go\nfmt.Println(1)\n```"),
	}

	latest := resolveCompletedAssistantResponse(messages, 1)
	if !latest.Found || latest.LoadedCount != 2 || latest.Text != messages[5].Parts[0].Text {
		t.Fatalf("latest = %+v", latest)
	}
	previous := resolveCompletedAssistantResponse(messages, 2)
	if !previous.Found || previous.LoadedCount != 2 || previous.Text != "I will inspect.\nInspection complete." {
		t.Fatalf("previous = %+v", previous)
	}
	if strings.Contains(previous.Text, "secret") || strings.Contains(previous.Text, "hidden") {
		t.Fatalf("tool content leaked into response source: %q", previous.Text)
	}
	missing := resolveCompletedAssistantResponse(messages, 3)
	if missing.Found || missing.LoadedCount != 2 {
		t.Fatalf("missing = %+v", missing)
	}
}

func TestResolveCompletedAssistantResponseDistinctAndLegacyGroups(t *testing.T) {
	messages := []session.Message{
		assistantCopyMessage("a", "response a"),
		assistantCopyMessage("b", "response b"),
		{Role: llm.RoleUser, TextContent: "legacy turn"},
		assistantCopyMessage("", "legacy first"),
		{Role: llm.RoleTool, TextContent: "legacy tool", Parts: []llm.Part{{Type: llm.PartToolResult, Text: "legacy tool"}}},
		assistantCopyMessage("", "legacy second"),
		{Role: llm.RoleEvent, TextContent: "boundary"},
		assistantCopyMessage("", "   "),
	}

	want := []string{"legacy first\nlegacy second", "response b", "response a"}
	for ordinal, text := range want {
		got := resolveCompletedAssistantResponse(messages, ordinal+1)
		if !got.Found || got.LoadedCount != len(want) || got.Text != text {
			t.Fatalf("ordinal %d = %+v, want %q", ordinal+1, got, text)
		}
	}
}

func TestAssistantResponseSourceTextFiltersPartsAndUsesLegacyFallback(t *testing.T) {
	summary := "[Context Compaction]\nprivate summary"
	ack := *session.NewMessage("session", llm.AssistantText("I've reviewed the context summary. I'll continue from where we left off."), -1)
	fileOnly := *session.NewMessage("session", llm.Message{
		Role:  llm.RoleAssistant,
		Parts: []llm.Part{{Type: llm.PartFile, Text: "uploaded file contents"}},
	}, -1)
	if fileOnly.TextContent != "uploaded file contents" {
		t.Fatalf("NewMessage PartFile precondition TextContent = %q", fileOnly.TextContent)
	}
	messages := []session.Message{
		{
			Role: llm.RoleAssistant,
			Parts: []llm.Part{
				{Type: llm.PartFile, Text: "file content"},
				{Type: llm.PartImage, Text: "image"},
				{Type: llm.PartToolCall, Text: "tool"},
				{Type: llm.PartToolResult, Text: "result"},
				{Type: llm.PartSkillActivation, Text: "skill"},
				{Type: llm.PartAgentMention, Text: "mention"},
				{Type: llm.PartText, Text: ""},
				{Type: llm.PartText, Text: "visible", ReasoningContent: "hidden reasoning"},
				{Type: llm.PartText, Text: "second"},
			},
			TextContent: "fallback must not be used",
		},
		{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText}}, TextContent: "empty text part suppresses fallback"},
		fileOnly,
		{Role: llm.RoleAssistant, TextContent: "legacy fallback"},
		{Role: llm.RoleAssistant, TextContent: summary},
		ack,
	}

	got := assistantResponseSourceText(messages)
	if got != "visible\nsecond\nlegacy fallback" {
		t.Fatalf("source text = %q", got)
	}
	for _, hidden := range []string{"file content", "uploaded file", "image", "tool", "result", "skill", "mention", "reasoning", "fallback must", "empty text", "private summary", "reviewed"} {
		if strings.Contains(got, hidden) {
			t.Fatalf("source text leaked %q: %q", hidden, got)
		}
	}
}

func installCopyBackend(t *testing.T, fn func(string) (clipboard.CopyMethod, error)) {
	t.Helper()
	old := copyTextBestEffort
	copyTextBestEffort = fn
	t.Cleanup(func() { copyTextBestEffort = old })
}

func runCopyCommandResult(t *testing.T, m *Model, cmd tea.Cmd) *Model {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected asynchronous copy command")
	}
	msg := cmd()
	if _, ok := msg.(copyResultMsg); !ok {
		t.Fatalf("copy command returned %T", msg)
	}
	updated, _ := m.Update(msg)
	return updated.(*Model)
}

func TestCmdCopyStreamingSemanticsAndSnapshots(t *testing.T) {
	var copied []string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = append(copied, text)
		return clipboard.CopyMethodNative, nil
	})

	m := newTestChatModel(true)
	m.messages = []session.Message{assistantCopyMessage("completed", "completed answer")}
	m.streaming = true
	m.currentResponse.WriteString("live partial 世界")

	_, cmd := m.cmdCopy(nil)
	m.currentResponse.Reset()
	m.currentResponse.WriteString("mutated live text")
	m = runCopyCommandResult(t, m, cmd)
	if copied[0] != "live partial 世界" {
		t.Fatalf("bare /copy payload = %q", copied[0])
	}
	if m.copyStatus != "Copied latest response · 15 chars" {
		t.Fatalf("copy status = %q", m.copyStatus)
	}

	_, cmd = m.cmdCopy([]string{"1"})
	m.messages[0] = assistantCopyMessage("completed", "mutated completed answer")
	m = runCopyCommandResult(t, m, cmd)
	if copied[1] != "completed answer" {
		t.Fatalf("explicit /copy payload = %q", copied[1])
	}
	if m.copyStatus != "Copied response 1 · 16 chars" {
		t.Fatalf("copy status = %q", m.copyStatus)
	}
}

func TestCmdCopyBareFallsBackBeforeLiveVisibleText(t *testing.T) {
	var copied string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = text
		return clipboard.CopyMethodOSC52, nil
	})
	m := newTestChatModel(true)
	m.messages = []session.Message{assistantCopyMessage("completed", "done")}
	m.streaming = true
	m.currentResponse.WriteString(" \n\t")

	_, cmd := m.cmdCopy(nil)
	m = runCopyCommandResult(t, m, cmd)
	if copied != "done" || m.copyStatus != "Copied latest response · 4 chars · OSC 52" {
		t.Fatalf("payload=%q status=%q", copied, m.copyStatus)
	}
}

func TestCmdCopyCompletedStateIgnoresStaleCurrentResponse(t *testing.T) {
	var copied string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = text
		return clipboard.CopyMethodNative, nil
	})
	m := newTestChatModel(true)
	m.streaming = false
	m.currentResponse.WriteString("stale completed buffer")
	m.messages = []session.Message{assistantCopyMessage("completed", "persisted completed answer")}

	_, cmd := m.cmdCopy(nil)
	m = runCopyCommandResult(t, m, cmd)
	if copied != "persisted completed answer" {
		t.Fatalf("completed bare /copy payload = %q", copied)
	}
}

func TestCmdCopyValidationAndLoadedCountFeedback(t *testing.T) {
	installCopyBackend(t, func(string) (clipboard.CopyMethod, error) {
		t.Fatal("clipboard backend called for invalid command")
		return "", nil
	})

	for _, input := range []string{"/copy 0", "/copy -1", "/copy nope", "/copy 1 extra", "/copy +1"} {
		t.Run(input, func(t *testing.T) {
			m := newTestChatModel(true)
			updated, cmd := m.ExecuteCommand(input)
			m = runCopyCommandResult(t, updated.(*Model), cmd)
			if m.copyStatus != copyUsage {
				t.Fatalf("status = %q", m.copyStatus)
			}
		})
	}

	m := newTestChatModel(true)
	m.messages = []session.Message{assistantCopyMessage("one", "one")}
	before := len(m.messages)
	updated, cmd := m.ExecuteCommand("/copy 2")
	m = runCopyCommandResult(t, updated.(*Model), cmd)
	if m.copyStatus != "Only 1 assistant response is loaded" {
		t.Fatalf("status = %q", m.copyStatus)
	}
	if len(m.messages) != before {
		t.Fatal("copy feedback polluted the transcript")
	}

	m = newTestChatModel(true)
	updated, cmd = m.ExecuteCommand("/copy")
	m = runCopyCommandResult(t, updated.(*Model), cmd)
	if m.copyStatus != "Nothing to copy yet" {
		t.Fatalf("status = %q", m.copyStatus)
	}
}

func TestCopyMessagesRemainOwnedByParentChat(t *testing.T) {
	if !isParentChatMessage(copyResultMsg{}) || !isParentChatMessage(copyStatusClearMsg{}) {
		t.Fatal("copy lifecycle message could be swallowed by an embedded view")
	}
}

func TestHandleCopyResultFormatsRuneCount(t *testing.T) {
	m := newTestChatModel(true)
	updated, _ := m.handleCopyResult(copyResultMsg{
		kind:        copyResultSuccess,
		method:      clipboard.CopyMethodNative,
		runeCount:   1234,
		targetLabel: "latest response",
	})
	if got := updated.(*Model).copyStatus; got != "Copied latest response · 1,234 chars" {
		t.Fatalf("status = %q", got)
	}
}

func TestCopyStatusResultIsExplicit(t *testing.T) {
	msg, ok := copyStatusCmd(copyUsage)().(copyResultMsg)
	if !ok {
		t.Fatalf("copyStatusCmd returned %T", msg)
	}
	if msg.kind != copyResultStatus || msg.status != copyUsage || msg.err != nil || msg.targetLabel != "" {
		t.Fatalf("status result = %+v", msg)
	}

	m := newTestChatModel(true)
	updated, _ := m.handleCopyResult(copyResultMsg{})
	if got := updated.(*Model).copyStatus; got != "Copy failed: invalid copy result" {
		t.Fatalf("zero-value result status = %q", got)
	}
}

func TestCopyResultStatusRendersInView(t *testing.T) {
	installCopyBackend(t, func(string) (clipboard.CopyMethod, error) {
		return clipboard.CopyMethodNative, nil
	})
	m := newTestChatModel(true)
	m.width, m.height = 120, 32
	_, _ = m.Update(tea.WindowSizeMsg{Width: m.width, Height: m.height})
	m.messages = []session.Message{assistantCopyMessage("one", "source response")}

	_, cmd := m.cmdCopy(nil)
	m = runCopyCommandResult(t, m, cmd)
	view := ui.StripANSI(m.View().Content)
	if !strings.Contains(view, "Copied latest response · 15 chars") {
		t.Fatalf("rendered view missing copy status: %q", view)
	}
}

func TestHandleCopyResultUsesSingularCharacter(t *testing.T) {
	m := newTestChatModel(true)
	updated, _ := m.handleCopyResult(copyResultMsg{
		kind:        copyResultSuccess,
		method:      clipboard.CopyMethodNative,
		runeCount:   1,
		targetLabel: "selection",
	})
	if got := updated.(*Model).copyStatus; got != "Copied selection · 1 char" {
		t.Fatalf("status = %q", got)
	}
}

func TestCopyResultFailureIsContentFreeAndSurvivesKeypress(t *testing.T) {
	secret := "do not leak this response"
	installCopyBackend(t, func(string) (clipboard.CopyMethod, error) {
		return "", errors.New("native unavailable; OSC 52 unavailable")
	})
	m := newTestChatModel(true)
	m.messages = []session.Message{assistantCopyMessage("one", secret)}
	_, cmd := m.cmdCopy(nil)
	m = runCopyCommandResult(t, m, cmd)
	if m.copyStatus != "Copy failed: native unavailable; OSC 52 unavailable" || strings.Contains(m.copyStatus, secret) {
		t.Fatalf("status = %q", m.copyStatus)
	}

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'x', Text: "x"})
	m = updated.(*Model)
	if m.copyStatus == "" {
		t.Fatal("asynchronous copy result was eagerly cleared before rendering")
	}
	seq := m.copyStatusSeq
	updated, _ = m.Update(copyStatusClearMsg{seq: seq})
	if updated.(*Model).copyStatus != "" {
		t.Fatal("copy status did not clear after its timer message")
	}
}

func TestCtrlYSelectionPrecedenceAndNoSelectionResponseCopy(t *testing.T) {
	var copied []string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = append(copied, text)
		return clipboard.CopyMethodNative, nil
	})
	m := newTestChatModel(true)
	m.messages = []session.Message{assistantCopyMessage("one", "response source")}
	m.contentLines = []string{"rendered selection"}
	m.selection = Selection{Active: true, Anchor: ContentPos{Line: 0, Col: 0}, Cursor: ContentPos{Line: 0, Col: 8}}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	m = updated.(*Model)
	if m.selection.Active {
		t.Fatal("selection was not cleared synchronously")
	}
	m = runCopyCommandResult(t, m, cmd)
	if copied[0] != "rendered" {
		t.Fatalf("selected Ctrl+Y copied %q", copied[0])
	}

	updated, cmd = m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	m = runCopyCommandResult(t, updated.(*Model), cmd)
	if copied[1] != "response source" {
		t.Fatalf("unselected Ctrl+Y copied %q", copied[1])
	}
}

func TestSideQuestionSelectionUsesSharedCopyResult(t *testing.T) {
	var copied string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = text
		return clipboard.CopyMethodNative, nil
	})
	m := newTestChatModel(true)
	m.sideQuestion.Visible = true
	m.sideQuestion.selectionLines = []string{"side rendered text"}
	m.selection = Selection{
		Active:       true,
		SideQuestion: true,
		Anchor:       ContentPos{Line: 0, Col: 0},
		Cursor:       ContentPos{Line: 0, Col: 4},
	}

	updated, cmd := m.Update(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	m = runCopyCommandResult(t, updated.(*Model), cmd)
	if copied != "side" || m.copyStatus != "Copied selection · 4 chars" {
		t.Fatalf("payload=%q status=%q", copied, m.copyStatus)
	}
}

func TestStreamingCanonicalCopyExecutesLocally(t *testing.T) {
	var copied string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = text
		return clipboard.CopyMethodNative, nil
	})
	m := newTestChatModel(false)
	m.streaming = true
	m.phase = "Responding"
	m.currentResponse.WriteString("live response")
	m.setTextareaValue("/copy")

	updated, cmd := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if cmd == nil {
		t.Fatal("streaming /copy did not return the local command sequence")
	}
	if len(m.pendingInterjections) != 0 {
		t.Fatalf("/copy queued as an interjection: %+v", m.pendingInterjections)
	}
	if got := m.textarea.Value(); got != "" {
		t.Fatalf("streaming /copy left composer text %q", got)
	}
	_, copyCmd := m.ExecuteCommand("/copy")
	m = runCopyCommandResult(t, m, copyCmd)
	if copied != "live response" {
		t.Fatalf("streaming /copy copied %q", copied)
	}
}

func TestCopyCommandRegistryPrefixAndStreamingGate(t *testing.T) {
	var copied string
	installCopyBackend(t, func(text string) (clipboard.CopyMethod, error) {
		copied = text
		return clipboard.CopyMethodNative, nil
	})
	var found bool
	for _, command := range AllCommands() {
		if command.Name == "copy" {
			found = true
			if command.Usage != "/copy [N]" || command.ArgumentHint != "[N]" {
				t.Fatalf("copy command = %+v", command)
			}
		}
	}
	if !found {
		t.Fatal("copy command not registered")
	}

	matches := FilterCommands("co")
	var copyMatch, compactMatch bool
	for _, match := range matches {
		copyMatch = copyMatch || match.Name == "copy"
		compactMatch = compactMatch || match.Name == "compact"
	}
	if !copyMatch || !compactMatch {
		t.Fatalf("/co matches = %+v", matches)
	}

	m := newTestChatModel(true)
	m.messages = []session.Message{assistantCopyMessage("one", "prefix response")}
	updated, cmd := m.ExecuteCommand("/cop")
	m = runCopyCommandResult(t, updated.(*Model), cmd)
	if copied != "prefix response" || !strings.Contains(m.copyStatus, "Copied latest response") {
		t.Fatalf("/cop payload=%q status=%q", copied, m.copyStatus)
	}
	_, cmd = m.ExecuteCommand("/co")
	if cmd == nil {
		t.Fatal("ambiguous /co did not report candidate commands")
	}
	if _, ok := cmd().(copyResultMsg); ok {
		t.Fatal("ambiguous /co unexpectedly executed /copy")
	}

	if !isStreamingLocalSlashCommand("/copy") || isStreamingLocalSlashCommand("/cop") {
		t.Fatal("streaming gate must accept only canonical /copy")
	}
}
