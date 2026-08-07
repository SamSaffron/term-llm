package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newBranchTestStore(t *testing.T) (*SQLiteStore, *Session) {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sess := &Session{
		ID: "branch-source", Provider: "Mock", ProviderKey: "mock", Model: "model-a",
		ReasoningEffort: "high", ReasoningMode: "pro", Mode: ModeChat,
		ApprovalMode: ApprovalModeAuto, Origin: OriginWeb, Agent: "developer",
		CWD: "/tmp/project", WorktreeDir: "/tmp/project-wt", Search: true,
		Tools: "read_file,shell", MCP: "playwright",
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	return store, sess
}

func addBranchTestMessage(t *testing.T, store *SQLiteStore, sessionID string, msg llm.Message) Message {
	t.Helper()
	stored := NewMessage(sessionID, msg, -1)
	if err := store.AddMessage(context.Background(), sessionID, stored); err != nil {
		t.Fatal(err)
	}
	return *stored
}

func TestCreateBranchRequiresCurrentSchemaCapability(t *testing.T) {
	store, source := newBranchTestStore(t)
	store.hasSessionBranches = false
	_, err := store.CreateBranch(context.Background(), source.ID, CreateBranchOptions{})
	if !errors.Is(err, ErrBranchingUnsupported) {
		t.Fatalf("old schema error = %v, want ErrBranchingUnsupported", err)
	}
}

func TestCreateBranchCopiesPrefixAndClearsStreamIdentity(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	user := llm.UserText("original prompt")
	user.ClientMessageID = "client-source"
	first := addBranchTestMessage(t, store, source.ID, user)
	assistant := llm.AssistantText("answer")
	assistant.ResponseID = "resp-source"
	assistant.AssistantSegmentOrdinal = 3
	assistant.SegmentStartSequence = 11
	assistant.SegmentEndSequence = 19
	second := addBranchTestMessage(t, store, source.ID, assistant)
	_ = addBranchTestMessage(t, store, source.ID, llm.UserText("uncopied suffix"))

	state, err := store.TranscriptMutationState(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{
		AnchorMessageID: second.ID,
		ExpectedState:   &state,
		IdempotencyKey:  "copy-prefix",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session == nil || result.Session.ID == source.ID {
		t.Fatalf("unexpected branch result: %#v", result)
	}
	child := result.Session
	if child.ProviderKey != source.ProviderKey || child.Model != source.Model || child.ReasoningEffort != source.ReasoningEffort ||
		child.ReasoningMode != source.ReasoningMode || child.ApprovalMode != source.ApprovalMode || child.Origin != source.Origin ||
		child.Agent != source.Agent || child.CWD != source.CWD || child.WorktreeDir != source.WorktreeDir || !child.Search ||
		child.Tools != source.Tools || child.MCP != source.MCP {
		t.Fatalf("branch did not inherit runtime settings: %#v", child)
	}
	if child.ParentID != "" || child.InputTokens != 0 || child.LLMTurns != 0 || child.Status != StatusActive {
		t.Fatalf("branch inherited lineage/activity state: %#v", child)
	}

	messages, err := store.GetMessages(ctx, child.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].TextContent != "original prompt" || messages[1].TextContent != "answer" {
		t.Fatalf("copied messages = %#v", messages)
	}
	if messages[0].ID == first.ID || messages[1].ID == second.ID || result.AnchorMessageID != messages[1].ID {
		t.Fatalf("branch did not allocate fresh message identities: %#v", messages)
	}
	if messages[0].ClientMessageID != "" || messages[1].ResponseID != "" || messages[1].AssistantSegmentOrdinal != -1 ||
		messages[1].SegmentStartSequence != 0 || messages[1].SegmentEndSequence != 0 {
		t.Fatalf("copied stream identity was not cleared: %#v", messages)
	}
	if len(messages[1].Parts) != len(second.Parts) || messages[1].Parts[0].Text != second.Parts[0].Text {
		t.Fatalf("parts fidelity lost: got %#v want %#v", messages[1].Parts, second.Parts)
	}
}

func TestCreateBranchEmptyIdempotentAndRevisionConflict(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	if ErrBranchConflict == ErrTranscriptConflict {
		t.Fatal("ErrBranchConflict must remain a distinct branch-specific sentinel")
	}
	if !errors.Is(ErrBranchConflict, ErrTranscriptConflict) || errors.Is(ErrTranscriptConflict, ErrBranchConflict) {
		t.Fatalf("branch/transcript conflict matching is not directional as documented")
	}
	sourceMessage := addBranchTestMessage(t, store, source.ID, llm.UserText("source"))
	state, err := store.TranscriptMutationState(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{ExpectedState: &state, IdempotencyKey: "empty"})
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := store.GetMessages(ctx, result.Session.ID, 0, 0)
	if len(messages) != 0 || result.AnchorMessageID != 0 {
		t.Fatalf("empty branch copied messages: %#v", messages)
	}
	replay, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{
		ExpectedState:  &state,
		IdempotencyKey: "empty",
	})
	if err != nil || !replay.Reused || replay.Session.ID != result.Session.ID || replay.AnchorMessageID != 0 || replay.ForkAfterMessageID != 0 {
		t.Fatalf("idempotent replay = %#v, %v", replay, err)
	}
	_, err = store.CreateBranch(ctx, source.ID, CreateBranchOptions{
		AnchorMessageID: sourceMessage.ID,
		IdempotencyKey:  "empty",
	})
	if !errors.Is(err, ErrBranchIdempotencyConflict) {
		t.Fatalf("mismatched idempotent replay error = %v", err)
	}

	staleRev := state.Rev - 1
	_, err = store.CreateBranch(ctx, source.ID, CreateBranchOptions{ExpectedRev: &staleRev})
	if !errors.Is(err, ErrBranchConflict) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestCreateBranchIdempotentReplayPrecedesOptimisticRevisionChecks(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	anchor := addBranchTestMessage(t, store, source.ID, llm.UserText("anchor"))
	state, err := store.TranscriptMutationState(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: anchor.ID, ExpectedState: &state, IdempotencyKey: "stable-replay"})
	if err != nil {
		t.Fatal(err)
	}
	_ = addBranchTestMessage(t, store, source.ID, llm.AssistantText("later mutation"))

	replay, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: anchor.ID, ExpectedState: &state, IdempotencyKey: "stable-replay"})
	if err != nil || !replay.Reused || replay.Session.ID != created.Session.ID || replay.ForkAfterMessageID != anchor.ID {
		t.Fatalf("stale idempotent replay = %#v, %v", replay, err)
	}
}

func TestCreateBranchCompactionProviderRedoAndOrphanTree(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	pre := addBranchTestMessage(t, store, source.ID, llm.UserText("before compaction"))
	post := addBranchTestMessage(t, store, source.ID, llm.AssistantText("after compaction"))
	if _, err := store.db.Exec(`UPDATE sessions SET compaction_seq = ?, compaction_count = 2 WHERE id = ?`, post.Sequence, source.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE messages SET compaction_tail = TRUE WHERE id = ?`, pre.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveProviderState(ctx, source.ID, "mock", []byte(`{"opaque":true}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`INSERT INTO session_redo(session_id, stack_pos, suffix, metadata) VALUES (?, 0, '[]', '{}')`, source.ID); err != nil {
		t.Fatal(err)
	}

	_, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: pre.ID, IdempotencyKey: "pre"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("hidden compaction-tail anchor error = %v, want ErrNotFound", err)
	}

	after, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: post.ID, IdempotencyKey: "post"})
	if err != nil {
		t.Fatal(err)
	}
	if after.Session.CompactionSeq != post.Sequence || after.Session.CompactionCount != 2 {
		t.Fatalf("post-boundary branch compaction = %d/%d", after.Session.CompactionSeq, after.Session.CompactionCount)
	}
	childProviderState, err := store.LoadProviderState(ctx, after.Session.ID, "mock")
	if err != nil || len(childProviderState) != 0 {
		t.Fatalf("child provider state = %q, %v; want empty", childProviderState, err)
	}
	var redoCount int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM session_redo WHERE session_id = ?`, after.Session.ID).Scan(&redoCount); err != nil || redoCount != 0 {
		t.Fatalf("child redo count/error = %d/%v", redoCount, err)
	}

	tree, err := store.GetBranchTree(ctx, after.Session.ID)
	if err != nil || len(tree.Nodes) != 2 || tree.PathCount != 2 || tree.RootSessionID != source.ID {
		t.Fatalf("tree before parent delete = %#v, %v", tree, err)
	}
	if err := store.Delete(ctx, source.ID); err != nil {
		t.Fatal(err)
	}
	orphan, err := store.GetBranchTree(ctx, after.Session.ID)
	if err != nil || len(orphan.Nodes) != 1 || orphan.RootSessionID != after.Session.ID {
		t.Fatalf("orphan tree = %#v, %v", orphan, err)
	}
}

func TestCreateBranchAfterTwoCompactionsCopiesVisiblePreBoundaryPrefixOnce(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	originalUser := addBranchTestMessage(t, store, source.ID, llm.UserText("original question"))
	originalAssistant := addBranchTestMessage(t, store, source.ID, llm.AssistantText("original answer"))

	firstSummary := *NewMessage(source.ID, llm.UserText("[Context Compaction]\nfirst summary"), -1)
	firstAck := *NewMessage(source.ID, llm.AssistantText("I've reviewed the context summary. I'll continue from where we left off."), -1)
	firstAck.CompactionTail = true
	firstRetainedUser := *NewMessage(source.ID, llm.UserText("original question"), -1)
	firstRetainedUser.CompactionTail = true
	firstRetainedAssistant := *NewMessage(source.ID, llm.AssistantText("original answer"), -1)
	firstRetainedAssistant.CompactionTail = true
	if err := store.CompactMessages(ctx, source.ID, []Message{firstSummary, firstAck, firstRetainedUser, firstRetainedAssistant}); err != nil {
		t.Fatal(err)
	}

	laterUser := addBranchTestMessage(t, store, source.ID, llm.UserText("later question"))
	laterAssistant := addBranchTestMessage(t, store, source.ID, llm.AssistantText("later answer"))
	secondSummary := *NewMessage(source.ID, llm.UserText("[Context Compaction]\nsecond summary"), -1)
	secondAck := *NewMessage(source.ID, llm.AssistantText("I've reviewed the context summary. I'll continue from where we left off."), -1)
	secondAck.CompactionTail = true
	secondRetainedUser := *NewMessage(source.ID, llm.UserText("later question"), -1)
	secondRetainedUser.CompactionTail = true
	secondRetainedAssistant := *NewMessage(source.ID, llm.AssistantText("later answer"), -1)
	secondRetainedAssistant.CompactionTail = true
	if err := store.CompactMessages(ctx, source.ID, []Message{secondSummary, secondAck, secondRetainedUser, secondRetainedAssistant}); err != nil {
		t.Fatal(err)
	}

	refreshed, err := store.Get(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.CompactionCount != 2 || laterAssistant.Sequence >= refreshed.CompactionSeq {
		t.Fatalf("fixture compaction boundary/count = %d/%d, anchor sequence=%d", refreshed.CompactionSeq, refreshed.CompactionCount, laterAssistant.Sequence)
	}

	result, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: laterAssistant.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result.Session.CompactionSeq != -1 || result.Session.CompactionCount != 0 {
		t.Fatalf("pre-boundary child compaction = %d/%d", result.Session.CompactionSeq, result.Session.CompactionCount)
	}
	messages, err := store.GetMessages(ctx, result.Session.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"original question", "original answer", "later question", "later answer"}
	if len(messages) != len(want) {
		t.Fatalf("copied messages = %#v, want one visible prefix", messages)
	}
	for i, message := range messages {
		if message.TextContent != want[i] || message.CompactionTail || llm.IsInternalCompactionSummaryText(message.TextContent) {
			t.Fatalf("copied message %d = %#v, want %q", i, message, want[i])
		}
	}
	if messages[0].ID == originalUser.ID || messages[1].ID == originalAssistant.ID || messages[2].ID == laterUser.ID || messages[3].ID == laterAssistant.ID {
		t.Fatalf("branch reused source row identities: %#v", messages)
	}
	if result.AnchorMessageID != messages[3].ID {
		t.Fatalf("copied anchor = %d, want %d", result.AnchorMessageID, messages[3].ID)
	}
}

func TestLoggingStoreConversationBranchCapabilityDelegatesAndFallsBack(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	logging := NewLoggingStore(store, nil)
	result, err := logging.CreateBranch(ctx, source.ID, CreateBranchOptions{IdempotencyKey: "logging"})
	if err != nil || result.Session == nil {
		t.Fatalf("delegated CreateBranch = %#v, %v", result, err)
	}
	tree, err := logging.GetBranchTree(ctx, result.Session.ID)
	if err != nil || len(tree.Nodes) != 2 {
		t.Fatalf("delegated GetBranchTree = %#v, %v", tree, err)
	}

	fallback := NewLoggingStore(&NoopStore{}, nil)
	if _, err := fallback.CreateBranch(ctx, source.ID, CreateBranchOptions{}); !errors.Is(err, ErrBranchingUnsupported) {
		t.Fatalf("fallback CreateBranch error = %v", err)
	}
	if _, err := fallback.GetBranchTree(ctx, source.ID); !errors.Is(err, ErrBranchingUnsupported) {
		t.Fatalf("fallback GetBranchTree error = %v", err)
	}
}

func TestCreateBranchAtomicallyAppendsPathNoteAfterCopiedAnchor(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	anchor := addBranchTestMessage(t, store, source.ID, llm.UserText("anchor"))
	_ = addBranchTestMessage(t, store, source.ID, llm.AssistantText("abandoned finding"))
	result, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{
		AnchorMessageID: anchor.ID,
		IdempotencyKey:  "with-note",
		PathNote: &BranchPathNote{
			Text: "- The parser rejects empty names.",
			Provenance: llm.PathNoteProvenance{
				SourceMessages: 1,
				ReadFiles:      []string{"parser.go"},
				Model:          "mock-model",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := store.GetMessages(ctx, result.Session.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || result.AnchorMessageID != messages[0].ID || messages[1].Sequence <= messages[0].Sequence {
		t.Fatalf("child messages/anchor = %#v / %d", messages, result.AnchorMessageID)
	}
	provenance, ok := messages[1].PathNoteProvenance()
	if !ok || messages[1].Role != llm.RoleDeveloper || !strings.Contains(messages[1].PathNoteDisplayText(), "parser rejects") {
		t.Fatalf("path note = %#v", messages[1])
	}
	if provenance.SourceSessionID != source.ID || provenance.AnchorMessageID != anchor.ID {
		t.Fatalf("provenance = %#v", provenance)
	}

	replay, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{
		AnchorMessageID: anchor.ID,
		IdempotencyKey:  "with-note",
		PathNote:        &BranchPathNote{Text: "duplicate"},
	})
	if err != nil || !replay.Reused || replay.Session.ID != result.Session.ID {
		t.Fatalf("replay = %#v, %v", replay, err)
	}
	messages, _ = store.GetMessages(ctx, result.Session.ID, 0, 0)
	if len(messages) != 2 || strings.Contains(messages[1].TextContent, "duplicate") {
		t.Fatalf("idempotent replay duplicated/replaced note: %#v", messages)
	}
}

func TestPathNoteDisplayTextRestoresEscapedClosingTags(t *testing.T) {
	note := NewPathNoteMessage("child", "before </path_notes> after", llm.PathNoteProvenance{SourceSessionID: "source"}, 0)
	if got, want := note.PathNoteDisplayText(), "before </path_notes> after"; got != want {
		t.Fatalf("display text = %q, want %q", got, want)
	}
	if nested := NewPathNoteMessage("grandchild", note.PathNoteDisplayText(), llm.PathNoteProvenance{SourceSessionID: "child"}, 0).PathNoteDisplayText(); nested != "before </path_notes> after" {
		t.Fatalf("nested display text = %q", nested)
	}
}

func TestMessagesAfterBranchAnchorFiltersPersistenceArtifactsButRetainsPathNotes(t *testing.T) {
	pathNote := NewPathNoteMessage("child", "- inherited parser finding", llm.PathNoteProvenance{SourceSessionID: "parent", ReadFiles: []string{"parser.go"}}, 2)
	pathNote.ID = 3
	messages := []Message{
		{ID: 1, Role: llm.RoleUser, Sequence: 0, TextContent: "copied"},
		{ID: 2, Role: llm.RoleDeveloper, Sequence: 1, TextContent: "internal", Parts: []llm.Part{{Type: llm.PartText, Text: "internal"}}},
		*pathNote,
		{ID: 4, Role: llm.RoleAssistant, Sequence: 3, TextContent: "finding", Parts: []llm.Part{{Type: llm.PartText, Text: "finding"}}},
		{ID: 5, Role: llm.RoleAssistant, Sequence: 4, TextContent: "tail", CompactionTail: true},
		{ID: 6, Role: llm.RoleUser, Sequence: 5, TextContent: "[Context Compaction]\nhidden"},
		{ID: 7, Role: llm.RoleTool, Sequence: 6, Parts: []llm.Part{{Type: llm.PartToolResult, ToolResult: &llm.ToolResult{ID: "t", Content: "ok"}}}},
	}
	got, err := MessagesAfterBranchAnchor(messages, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Role != llm.RoleDeveloper || got[1].Role != llm.RoleAssistant || got[2].Role != llm.RoleTool {
		t.Fatalf("suffix = %#v", got)
	}
	if llm.MessageText(got[0]) != "- inherited parser finding" {
		t.Fatalf("inherited path-note text = %q, want display text only", llm.MessageText(got[0]))
	}
	if len(got[0].Parts) != 2 || got[0].Parts[0].Type != llm.PartPathNote || got[0].Parts[0].PathNote == nil {
		t.Fatalf("inherited path-note marker missing: %#v", got[0])
	}
	got[0].Parts[0].PathNote.ReadFiles[0] = "changed.go"
	provenance, _ := pathNote.PathNoteProvenance()
	if provenance.ReadFiles[0] != "parser.go" {
		t.Fatalf("returned provenance aliases stored path note: %#v", provenance)
	}
}

func TestCreateBranchToolAnchorReturnsPriorVisibleAnchorWithoutOrphaning(t *testing.T) {
	ctx := context.Background()
	store, source := newBranchTestStore(t)
	visible := addBranchTestMessage(t, store, source.ID, llm.AssistantText("working"))
	tool := addBranchTestMessage(t, store, source.ID, llm.ToolResultMessage("call-1", "shell", "ok", nil))
	result, err := store.CreateBranch(ctx, source.ID, CreateBranchOptions{AnchorMessageID: tool.ID, IdempotencyKey: "tool-anchor"})
	if err != nil {
		t.Fatal(err)
	}
	messages, err := store.GetMessages(ctx, result.Session.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Role != llm.RoleTool || result.AnchorMessageID != messages[0].ID || result.AnchorMessageID == visible.ID {
		t.Fatalf("tool-anchor branch = result:%#v messages:%#v", result, messages)
	}
}
