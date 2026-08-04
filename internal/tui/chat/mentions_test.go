package chat

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/mentions"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
	"github.com/samsaffron/term-llm/internal/ui"
)

func prepareMentionPopup(m *Model, root, value string, candidate mentions.Candidate) {
	m.mentionEnabled = true
	m.mentionRoot = root
	m.mentionIndexGeneration = 1
	m.mentionIndex = &mentions.Snapshot{Root: root, Candidates: []mentions.Candidate{candidate}}
	m.setTextareaValue(value)
	m.textarea.MoveToEnd()
	token, ok := mentions.ActiveTokenAt(value, len(value))
	if !ok {
		panic("test mention token did not parse")
	}
	m.mentionPopup = mentionPopupModel{
		visible: true, token: token, matches: []mentions.Match{{Candidate: 0}},
		matchesRoot: root, matchesToken: token, matchesCursor: len(value), matchesGen: 1,
	}
}

func TestMentionSelectionInsertsOnlyOrdinaryText(t *testing.T) {
	m := newTestChatModel(false)
	root := t.TempDir()
	prepareMentionPopup(m, root, "review @notes", mentions.Candidate{Path: "docs/design notes.md", Kind: mentions.KindFile})
	m.acceptMentionSelection()
	if got := m.textarea.Value(); got != `review @"docs/design notes.md" ` {
		t.Fatalf("selected text = %q", got)
	}
	if m.mentionPopup.IsVisible() {
		t.Fatal("popup remained visible after selection")
	}

	prepareMentionPopup(m, root, "list @pkg", mentions.Candidate{Path: "internal/pkg", Kind: mentions.KindDirectory})
	m.acceptMentionSelection()
	if got := m.textarea.Value(); got != "list @internal/pkg/ " {
		t.Fatalf("directory selected text = %q", got)
	}
}

func TestManualMentionAttachesAtSubmitAndPreservesUserText(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "manual.txt")
	if err := os.WriteFile(path, []byte("manual body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.mentionRoot = root
	content := "please review @manual.txt"
	_, _ = m.sendMessage(content)

	last := m.messages[len(m.messages)-1]
	if last.TextContent != content {
		t.Fatalf("visible TextContent = %q, want %q", last.TextContent, content)
	}
	providerText := llm.MessageText(last.ToLLMMessage())
	if !strings.HasPrefix(providerText, content) || !strings.Contains(providerText, "manual body") {
		t.Fatalf("provider text = %q", providerText)
	}
}

func TestSuccessfulMentionPreservesExplicitFileTextContentConsumers(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mention.txt"), []byte("mention-only-body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.mentionRoot = root
	m.files = []FileAttachment{{Name: "explicit.txt", Content: "explicitsearchbody\n"}}
	content := "review @mention.txt"
	_, _ = m.sendMessage(content)

	last := m.messages[len(m.messages)-1]
	wantText := content + "\n\n" + llm.EmbeddedFileIntro + "\n\n" +
		llm.FormatEmbeddedFileText("explicit.txt", "text/plain", "explicitsearchbody\n")
	if last.TextContent != wantText {
		t.Fatalf("TextContent lost explicit /file fallback:\n got: %q\nwant: %q", last.TextContent, wantText)
	}
	if strings.Contains(last.TextContent, "mention-only-body") {
		t.Fatalf("provider-only mention context leaked into TextContent: %q", last.TextContent)
	}
	if historyText, ok := memoryPromptText(last); !ok || historyText != wantText {
		t.Fatalf("prompt history text = %q, %v", historyText, ok)
	}
	exported := session.ExportToMarkdown(&session.Session{Name: "mentions"}, []session.Message{last}, session.ExportOptions{})
	if !strings.Contains(exported, "explicitsearchbody") || strings.Contains(exported, "mention-only-body") {
		t.Fatalf("export did not preserve established /file representation: %q", exported)
	}
	providerText := llm.MessageText(last.ToLLMMessage())
	if !strings.Contains(providerText, "explicitsearchbody") || !strings.Contains(providerText, "mention-only-body") {
		t.Fatalf("provider text lost attachment content: %q", providerText)
	}

	store, err := session.NewStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persistedSession := &session.Session{ID: session.NewID(), Provider: "test", Model: "test", Mode: session.ModeChat}
	if err := store.Create(context.Background(), persistedSession); err != nil {
		t.Fatal(err)
	}
	last.SessionID = persistedSession.ID
	if err := store.AddMessage(context.Background(), persistedSession.ID, &last); err != nil {
		t.Fatal(err)
	}
	results, err := store.Search(context.Background(), session.SearchOptions{Query: "explicitsearchbody", Limit: 5})
	if err != nil || len(results) != 1 {
		t.Fatalf("explicit /file text search results = %#v, err=%v", results, err)
	}
	historyStore, ok := store.(session.PromptHistoryStore)
	if !ok {
		t.Fatal("store does not support prompt history")
	}
	history, err := historyStore.PreviousUserPrompt(context.Background(), "", 0)
	if err != nil || history == nil || history.Text != wantText {
		t.Fatalf("persisted prompt history = %#v, err=%v", history, err)
	}
}

func TestDuplicateMentionsUseRawTextDeduplicationAndRemainVisible(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "same.txt"), []byte("unique attachment body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.mentionRoot = root
	content := "compare @same.txt with @./same.txt and @same.txt"
	_, _ = m.sendMessage(content)

	last := m.messages[len(m.messages)-1]
	if last.TextContent != content || strings.Count(last.TextContent, "@same.txt") != 2 {
		t.Fatalf("visible duplicate mentions changed: %q", last.TextContent)
	}
	if got := strings.Count(llm.MessageText(last.ToLLMMessage()), "unique attachment body"); got != 2 {
		t.Fatalf("raw-deduplicated attachment body count = %d", got)
	}
}

func TestMentionPopupWithNoSelectionDoesNotConsumeSubmission(t *testing.T) {
	for _, state := range []struct {
		name      string
		indexing  bool
		searching bool
	}{
		{name: "indexing", indexing: true},
		{name: "searching", searching: true},
		{name: "no matches"},
	} {
		t.Run(state.name, func(t *testing.T) {
			m := newTestChatModel(false)
			m.mentionEnabled = true
			m.mentionRoot = t.TempDir()
			content := "inspect @missing.txt"
			m.setTextareaValue(content)
			m.textarea.MoveToEnd()
			token, ok := mentions.ActiveTokenAt(content, len(content))
			if !ok {
				t.Fatal("manual mention token did not parse")
			}
			m.mentionPopup = mentionPopupModel{
				visible: true, token: token, indexing: state.indexing, searching: state.searching,
			}

			updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
			m = updated.(*Model)
			if !m.streaming || len(m.messages) == 0 {
				t.Fatal("first Enter was consumed instead of submitting")
			}
			if got := m.messages[len(m.messages)-1].TextContent; got != content {
				t.Fatalf("submitted text = %q, want %q", got, content)
			}
		})
	}
}

func TestMentionMatchesAreInvalidatedWhenQueryChanges(t *testing.T) {
	m := newTestChatModel(false)
	prepareMentionPopup(m, t.TempDir(), "review @typ", mentions.Candidate{Path: "types.go", Kind: mentions.KindFile})

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: 'z', Text: "z"})
	m = updated.(*Model)
	if len(m.mentionPopup.matches) != 0 {
		t.Fatalf("stale matches survived query mutation: %#v", m.mentionPopup.matches)
	}
	updated, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	m = updated.(*Model)
	if len(m.messages) == 0 || m.messages[len(m.messages)-1].TextContent != "review @typz" {
		t.Fatalf("stale result rewrote or blocked submission: %#v", m.messages)
	}
}

func TestMentionAcceptanceRevalidatesCursorAndTokenRange(t *testing.T) {
	m := newTestChatModel(false)
	value := "review @typ"
	prepareMentionPopup(m, t.TempDir(), value, mentions.Candidate{Path: "types.go", Kind: mentions.KindFile})
	m.textarea.MoveToBegin()

	consumed, _ := m.handleMentionPopupKey(tea.KeyPressMsg{Code: tea.KeyTab})
	if !consumed {
		t.Fatal("Tab with a stale selection should be contained by the picker")
	}
	if got := m.textarea.Value(); got != value {
		t.Fatalf("stale token range rewrote draft to %q", got)
	}
	if m.mentionPopup.IsVisible() {
		t.Fatal("stale popup remained visible after rejected acceptance")
	}
}

func TestMentionPopupHidesAfterMouseCursorMove(t *testing.T) {
	m := newTestChatModel(false)
	prepareMentionPopup(m, t.TempDir(), "review @typ", mentions.Candidate{Path: "types.go", Kind: mentions.KindFile})
	_ = m.View()

	_, _ = m.Update(tea.MouseClickMsg{
		X:      m.textareaLeftX + m.textareaPromptWidth,
		Y:      m.textareaTopY,
		Button: tea.MouseLeft,
	})
	if m.mentionPopup.IsVisible() {
		t.Fatal("mouse cursor movement left stale mention popup visible")
	}
}

func TestMentionAcceptanceRejectsStaleIndexGeneration(t *testing.T) {
	m := newTestChatModel(false)
	value := "review @typ"
	prepareMentionPopup(m, t.TempDir(), value, mentions.Candidate{Path: "types.go", Kind: mentions.KindFile})
	m.mentionIndexGeneration++

	consumed, _ := m.handleMentionPopupKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if consumed {
		t.Fatal("Enter consumed submission for a stale index selection")
	}
	if got := m.textarea.Value(); got != value {
		t.Fatalf("stale index selection rewrote draft to %q", got)
	}
}

func TestMentionPopupRefreshesAfterNewline(t *testing.T) {
	m := newTestChatModel(false)
	prepareMentionPopup(m, t.TempDir(), "review @typ", mentions.Candidate{Path: "types.go", Kind: mentions.KindFile})

	updated, _ := m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModShift})
	m = updated.(*Model)
	if got := m.textarea.Value(); got != "review @typ\n" {
		t.Fatalf("newline value = %q", got)
	}
	if m.mentionPopup.IsVisible() {
		t.Fatal("newline left a stale mention popup visible")
	}
}

func TestFailedMentionAttachmentStillSends(t *testing.T) {
	m := newTestChatModel(false)
	m.mentionRoot = t.TempDir()
	content := "inspect @missing.txt anyway"
	_, _ = m.sendMessage(content)

	if !m.streaming || len(m.messages) == 0 {
		t.Fatal("failed mention blocked submission")
	}
	last := m.messages[len(m.messages)-1]
	if last.TextContent != content || llm.MessageText(last.ToLLMMessage()) != content {
		t.Fatalf("failed mention message = %#v", last)
	}
}

func TestDeniedMentionDoesNotPromptOrBlockSending(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "denied.txt"), []byte("do not attach"), 0o644); err != nil {
		t.Fatal(err)
	}
	approval := tools.NewApprovalManager(tools.NewToolPermissions())
	prompted := false
	approval.PromptUIFunc = func(string, bool, bool, string) (tools.ApprovalResult, error) {
		prompted = true
		return tools.ApprovalResult{}, nil
	}
	m := newTestChatModel(false)
	m.mentionRoot = root
	m.toolMgr = &tools.ToolManager{ApprovalMgr: approval}
	content := "inspect @denied.txt"
	_, _ = m.sendMessage(content)

	last := m.messages[len(m.messages)-1]
	if prompted || last.TextContent != content || strings.Contains(llm.MessageText(last.ToLLMMessage()), "do not attach") {
		t.Fatalf("denied mention prompted=%v message=%#v", prompted, last)
	}
}

func TestStreamingInterjectionGetsSubmitTimeMentionContext(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("interjection attachment"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := newTestChatModel(false)
	m.mentionRoot = root
	m.streaming = true
	m.fastProvider = nil
	m.setTextareaValue("also inspect @note.txt")

	_, _ = m.handleKeyMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	queued := m.engine.ListPendingInterjections()
	if len(queued) != 1 {
		t.Fatalf("queued interjections = %#v", queued)
	}
	parts := queued[0].Message.Parts
	if len(parts) != 2 || parts[0].Type != llm.PartText || parts[1].Type != llm.PartFile {
		t.Fatalf("queued part order = %#v, want original text before mention context", parts)
	}
	if parts[0].Text != "also inspect @note.txt" || !strings.Contains(parts[1].Text, "interjection attachment") {
		t.Fatalf("queued mention parts = %#v", parts)
	}
	text := llm.MessageText(queued[0].Message)
	if !strings.Contains(text, "also inspect @note.txt") || !strings.Contains(text, "interjection attachment") {
		t.Fatalf("queued mention context = %q", text)
	}
}

func TestMentionPopupRendersInlineAndAltScreen(t *testing.T) {
	for _, alt := range []bool{false, true} {
		m := newTestChatModel(alt)
		m.width, m.height = 80, 24
		prepareMentionPopup(m, t.TempDir(), "review @typ", mentions.Candidate{Path: "internal/llm/types.go", Kind: mentions.KindFile})
		content := ui.StripANSI(m.View().Content)
		if !strings.Contains(content, "internal/llm/types.go") || !strings.Contains(content, "enter/tab select") {
			t.Fatalf("alt=%v popup missing from frame: %q", alt, content)
		}
	}
}

func TestMentionQueryCancellationAndRootReset(t *testing.T) {
	m := newTestChatModel(false)
	root := t.TempDir()
	prepareMentionPopup(m, root, "review @typ", mentions.Candidate{Path: "types.go", Kind: mentions.KindFile})
	cmd := m.scheduleMentionQuery(m.mentionPopup.token)
	msg := cmd()
	m.mentionQueryRequest++
	if handled, next := m.handleMentionMessage(msg); !handled || next != nil {
		t.Fatalf("stale debounce handled=%v next=%v", handled, next)
	}

	newRoot := t.TempDir()
	m.resetMentionsForRoot(newRoot)
	m.setTextareaValue("review @a")
	m.textarea.MoveToEnd()
	if cmd := m.updateMentionQuery(); cmd == nil || !m.mentionPopup.indexing || m.mentionIndex != nil {
		t.Fatalf("root reset did not schedule fresh index: popup=%#v index=%#v", m.mentionPopup, m.mentionIndex)
	}
}

func TestBareMentionEnterDoesNotRewriteDraft(t *testing.T) {
	m := newTestChatModel(false)
	m.mentionEnabled = true
	m.mentionIndex = &mentions.Snapshot{Candidates: []mentions.Candidate{{Path: "a.go", Kind: mentions.KindFile}}}
	m.setTextareaValue("ping me @")
	m.textarea.MoveToEnd()
	token, ok := mentions.ActiveTokenAt(m.textarea.Value(), len(m.textarea.Value()))
	if !ok {
		t.Fatal("bare token did not parse")
	}
	m.mentionPopup = mentionPopupModel{visible: true, token: token, matches: []mentions.Match{{Candidate: 0}}}
	before := m.textarea.Value()
	consumed, cmd := m.handleMentionPopupKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	if consumed || cmd != nil || m.textarea.Value() != before {
		t.Fatalf("bare @ Enter consumed=%v value=%q", consumed, m.textarea.Value())
	}
}

func TestTruncateMentionPathKeepsMatchedBasename(t *testing.T) {
	path := "very/deep/project/path/internal/session/types.go"
	position := strings.LastIndex(path, "types.go")
	display, positions := truncateMentionPath(path, []int{position}, 20)
	if !strings.Contains(display, "types.go") || len(positions) != 1 || positions[0] < 0 || positions[0] >= len(display) {
		t.Fatalf("truncation display=%q positions=%v", display, positions)
	}
}
