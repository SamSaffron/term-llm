package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestSessionPreferredTitlePrecedence(t *testing.T) {
	sess := Session{Summary: "first message summary", GeneratedShortTitle: "Generated short title", GeneratedLongTitle: "Generated long title"}
	if got := sess.PreferredShortTitle(); got != "Generated short title" {
		t.Fatalf("PreferredShortTitle() = %q", got)
	}
	if got := sess.PreferredLongTitle(); got != "Generated long title" {
		t.Fatalf("PreferredLongTitle() = %q", got)
	}
	sess.Name = "Custom name"
	if got := sess.PreferredShortTitle(); got != "Custom name" {
		t.Fatalf("PreferredShortTitle() with name = %q", got)
	}
	if got := sess.PreferredLongTitle(); got != "Custom name" {
		t.Fatalf("PreferredLongTitle() with name = %q", got)
	}
}

func TestNewSQLiteStoreMemoryDBUsesSingleConnection(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 for :memory: databases", got)
	}
}

func TestSQLiteStoreGetMessagesFromHonorsLimit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	for i := 0; i < 5; i++ {
		msg := NewMessage(sess.ID, llm.UserText(fmt.Sprintf("msg-%d", i)), i)
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%d): %v", i, err)
		}
	}

	got, err := store.GetMessagesFrom(ctx, sess.ID, 2, 2)
	if err != nil {
		t.Fatalf("GetMessagesFrom limited: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("limited len = %d, want 2", len(got))
	}
	if got[0].Sequence != 2 || got[1].Sequence != 3 {
		t.Fatalf("limited sequences = [%d %d], want [2 3]", got[0].Sequence, got[1].Sequence)
	}

	got, err = store.GetMessagesFrom(ctx, sess.ID, 2, 0)
	if err != nil {
		t.Fatalf("GetMessagesFrom unlimited: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("unlimited len = %d, want 3", len(got))
	}
	if got[2].Sequence != 4 {
		t.Fatalf("last sequence = %d, want 4", got[2].Sequence)
	}
}

func TestSQLiteStoreGetLatestVisibleMessageIDSkipsInvisibleTail(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	user := NewMessage(sess.ID, llm.UserText("hello"), 0)
	if err := store.AddMessage(ctx, sess.ID, user); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	assistant := NewMessage(sess.ID, llm.AssistantText("hi"), 1)
	if err := store.AddMessage(ctx, sess.ID, assistant); err != nil {
		t.Fatalf("AddMessage(assistant): %v", err)
	}
	tool := NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartText, Text: "invisible tail"}}}, 2)
	if err := store.AddMessage(ctx, sess.ID, tool); err != nil {
		t.Fatalf("AddMessage(tool): %v", err)
	}

	got, err := store.GetLatestVisibleMessageID(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetLatestVisibleMessageID: %v", err)
	}
	if got != assistant.ID {
		t.Fatalf("latest visible message id = %d, want %d", got, assistant.ID)
	}
}

func TestSQLiteStoreReplaceMessagesPreservesUnchangedPrefix(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	messages := []Message{
		*NewMessage(sess.ID, llm.UserText("stable user"), 0),
		*NewMessage(sess.ID, llm.AssistantText("stable assistant"), 1),
		*NewMessage(sess.ID, llm.UserText("old suffix"), 2),
	}
	for i := range messages {
		messages[i].CreatedAt = base.Add(time.Duration(i) * time.Second)
		messages[i].TurnIndex = i
	}
	if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
		t.Fatalf("initial ReplaceMessages: %v", err)
	}
	before, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages before: %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("initial message count = %d, want 3", len(before))
	}

	replacement := []Message{
		*NewMessage(sess.ID, llm.UserText("stable user"), 0),
		*NewMessage(sess.ID, llm.AssistantText("stable assistant"), 1),
		*NewMessage(sess.ID, llm.UserText("new suffix"), 2),
		*NewMessage(sess.ID, llm.AssistantText("new tail"), 3),
	}
	for i := range replacement {
		// Simulate serve/web rebuilding a fresh snapshot for unchanged history: the
		// content is identical, but CreatedAt is newly allocated and should not force
		// a full rewrite of the prefix.
		replacement[i].CreatedAt = base.Add(time.Duration(i+100) * time.Second)
		replacement[i].TurnIndex = i
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET compaction_seq = 5, compaction_count = 1 WHERE id = ?", sess.ID); err != nil {
		t.Fatalf("seed compaction boundary: %v", err)
	}
	if err := store.ReplaceMessages(ctx, sess.ID, replacement); err != nil {
		t.Fatalf("replacement ReplaceMessages: %v", err)
	}

	after, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages after: %v", err)
	}
	if len(after) != 4 {
		t.Fatalf("final message count = %d, want 4", len(after))
	}
	if after[0].ID != before[0].ID || after[1].ID != before[1].ID {
		t.Fatalf("unchanged prefix row IDs = [%d %d], want [%d %d]", after[0].ID, after[1].ID, before[0].ID, before[1].ID)
	}
	if after[2].ID == before[2].ID {
		t.Fatalf("changed suffix row kept old ID %d; want rewritten suffix", after[2].ID)
	}
	if after[2].TextContent != "new suffix" || after[3].TextContent != "new tail" {
		t.Fatalf("suffix texts = %q, %q; want new suffix/new tail", after[2].TextContent, after[3].TextContent)
	}

	results, err := store.Search(ctx, "old suffix", 10)
	if err != nil {
		t.Fatalf("Search old suffix: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("old suffix still in FTS: %#v", results)
	}
	results, err = store.Search(ctx, "new suffix", 10)
	if err != nil {
		t.Fatalf("Search new suffix: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("new suffix FTS matches = %d, want 1", len(results))
	}
	meta, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get meta: %v", err)
	}
	if meta.CompactionSeq != -1 || meta.CompactionCount != 0 {
		t.Fatalf("compaction boundary = seq %d count %d, want cleared", meta.CompactionSeq, meta.CompactionCount)
	}
	summaries, err := store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].MessageCount != 4 {
		t.Fatalf("summary count = len %d message_count %d, want len 1 count 4", len(summaries), summaries[0].MessageCount)
	}
}

func TestSQLiteStoreReplaceMessagesFallsBackForDuplicateSequences(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	desired := []Message{*NewMessage(sess.ID, llm.UserText("kept message"), 0)}
	if err := store.ReplaceMessages(ctx, sess.ID, desired); err != nil {
		t.Fatalf("initial ReplaceMessages: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `DROP INDEX IF EXISTS idx_messages_session_sequence`); err != nil {
		t.Fatalf("drop sequence index for duplicate fixture: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence)
		VALUES (?, 'user', ?, 'stale duplicate', 0, 0, ?, 0)`,
		sess.ID, `[{"type":"text","text":"stale duplicate"}]`, time.Now().UTC()); err != nil {
		t.Fatalf("insert duplicate sequence row: %v", err)
	}

	if err := store.ReplaceMessages(ctx, sess.ID, desired); err != nil {
		t.Fatalf("ReplaceMessages with duplicate sequence: %v", err)
	}

	got, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 1 || got[0].TextContent != "kept message" {
		t.Fatalf("messages after duplicate cleanup = %#v, want one kept message", got)
	}
	results, err := store.Search(ctx, "stale duplicate", 10)
	if err != nil {
		t.Fatalf("Search stale duplicate: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("stale duplicate still in FTS: %#v", results)
	}
	summaries, err := store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].MessageCount != 1 {
		t.Fatalf("summary count = len %d message_count %d, want len 1 count 1", len(summaries), summaries[0].MessageCount)
	}
}

func TestSQLiteStoreReplaceMessagesPreservesTurnIndex(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	messages := []Message{
		*NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}}, 0),
		*NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "hi"}}}, 1),
	}
	messages[0].TurnIndex = 11
	messages[1].TurnIndex = 12

	if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	got, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	if got[0].TurnIndex != 11 || got[1].TurnIndex != 12 {
		t.Fatalf("turn indexes = [%d %d], want [11 12]", got[0].TurnIndex, got[1].TurnIndex)
	}
}

func TestSQLiteStoreReplaceMessagesClearsCompactionBoundary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET compaction_seq = 10, compaction_count = 2 WHERE id = ?", sess.ID); err != nil {
		t.Fatalf("seed compaction boundary: %v", err)
	}

	messages := []Message{*NewMessage(sess.ID, llm.UserText("replacement"), 0)}
	if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.CompactionSeq != -1 || got.CompactionCount != 0 {
		t.Fatalf("compaction boundary = seq %d count %d, want cleared", got.CompactionSeq, got.CompactionCount)
	}
}

func TestLoadActiveMessagesClearsStaleCompactionBoundary(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AddMessage(ctx, sess.ID, NewMessage(sess.ID, llm.UserText("hello"), 0)); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE sessions SET compaction_seq = 99, compaction_count = 1 WHERE id = ?", sess.ID); err != nil {
		t.Fatalf("seed stale compaction boundary: %v", err)
	}

	reloaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	msgs, err := LoadActiveMessages(ctx, store, reloaded)
	if err != nil {
		t.Fatalf("LoadActiveMessages: %v", err)
	}
	if len(msgs) != 1 || msgs[0].TextContent != "hello" {
		t.Fatalf("active messages = %#v, want full fallback history", msgs)
	}

	repaired, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get repaired: %v", err)
	}
	if repaired.CompactionSeq != -1 || repaired.CompactionCount != 0 {
		t.Fatalf("compaction boundary = seq %d count %d, want cleared", repaired.CompactionSeq, repaired.CompactionCount)
	}
}

func TestSQLiteStoreSearchEscapesUserQueryForFTS(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "term-llm memory search"}}}, 0)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	results, err := store.Search(ctx, "term-llm", 10)
	if err != nil {
		t.Fatalf("Search(term-llm) error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(term-llm) len = %d, want 1", len(results))
	}
}

func TestSQLiteStorePersistsDeveloperMessages(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "Be concise"}}}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage developer: %v", err)
	}

	msgs, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != llm.RoleDeveloper {
		t.Fatalf("message role = %q, want %q", msgs[0].Role, llm.RoleDeveloper)
	}
}

func TestSQLiteStorePersistsEventMessages(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	marker := llm.ModelSwapEventMessage(llm.ModelSwapMarker{FromProvider: "old", FromModel: "a", ToProvider: "new", ToModel: "b", Status: "succeeded", Strategy: "naive"})
	msg := NewMessage(sess.ID, marker, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage event: %v", err)
	}

	msgs, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("expected 1 message, got %d", len(msgs))
	}
	if msgs[0].Role != llm.RoleEvent {
		t.Fatalf("message role = %q, want %q", msgs[0].Role, llm.RoleEvent)
	}
	if parsed, ok := llm.ParseModelSwapMarker(msgs[0].ToLLMMessage()); !ok || parsed.Status != "succeeded" {
		t.Fatalf("failed to parse persisted model-swap marker: ok=%v parsed=%#v", ok, parsed)
	}
}

func TestSQLiteStoreAddMessageBumpsLastMessageAt(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	userTime := time.Now().UTC().Add(-5 * time.Minute).Truncate(time.Second)
	userMsg := NewMessage(sess.ID, llm.Message{Role: llm.RoleUser, Parts: []llm.Part{{Type: llm.PartText, Text: "hi"}}}, -1)
	userMsg.CreatedAt = userTime
	if err := store.AddMessage(ctx, sess.ID, userMsg); err != nil {
		t.Fatalf("AddMessage user: %v", err)
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List after user msg: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if !summaries[0].LastMessageAt.Equal(userTime) {
		t.Fatalf("LastMessageAt after user msg = %v, want %v", summaries[0].LastMessageAt, userTime)
	}
	if summaries[0].MessageCount != 1 {
		t.Fatalf("MessageCount after user msg = %d, want 1", summaries[0].MessageCount)
	}

	assistantTime := userTime.Add(10 * time.Second)
	assistantMsg := NewMessage(sess.ID, llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "hello"}}}, -1)
	assistantMsg.CreatedAt = assistantTime
	if err := store.AddMessage(ctx, sess.ID, assistantMsg); err != nil {
		t.Fatalf("AddMessage assistant: %v", err)
	}

	summaries, err = store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List after assistant msg: %v", err)
	}
	if !summaries[0].LastMessageAt.Equal(assistantTime) {
		t.Fatalf("LastMessageAt after assistant msg = %v, want %v", summaries[0].LastMessageAt, assistantTime)
	}
	if summaries[0].MessageCount != 2 {
		t.Fatalf("MessageCount after assistant msg = %d, want 2", summaries[0].MessageCount)
	}

	// Tool/developer/system messages must not bump last_message_at so it
	// stays aligned with message_count (which filters to user/assistant).
	nonVisibleTime := assistantTime.Add(1 * time.Hour)
	toolMsg := NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartText, Text: "ignored"}}}, -1)
	toolMsg.CreatedAt = nonVisibleTime
	if err := store.AddMessage(ctx, sess.ID, toolMsg); err != nil {
		t.Fatalf("AddMessage tool: %v", err)
	}
	devMsg := NewMessage(sess.ID, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "ignored"}}}, -1)
	devMsg.CreatedAt = nonVisibleTime.Add(1 * time.Second)
	if err := store.AddMessage(ctx, sess.ID, devMsg); err != nil {
		t.Fatalf("AddMessage developer: %v", err)
	}
	systemMsg := NewMessage(sess.ID, llm.Message{Role: llm.RoleSystem, Parts: []llm.Part{{Type: llm.PartText, Text: "ignored"}}}, -1)
	systemMsg.CreatedAt = nonVisibleTime.Add(2 * time.Second)
	if err := store.AddMessage(ctx, sess.ID, systemMsg); err != nil {
		t.Fatalf("AddMessage system: %v", err)
	}

	summaries, err = store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List after non-visible msgs: %v", err)
	}
	if !summaries[0].LastMessageAt.Equal(assistantTime) {
		t.Fatalf("LastMessageAt should not move for non-visible roles: got %v, want %v", summaries[0].LastMessageAt, assistantTime)
	}
	if summaries[0].MessageCount != 2 {
		t.Fatalf("MessageCount after non-visible msgs = %d, want 2", summaries[0].MessageCount)
	}
}

func TestSQLiteStoreMigration20BackfillsLastMessageAt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			number INTEGER,
			name TEXT,
			summary TEXT,
			generated_short_title TEXT,
			generated_long_title TEXT,
			title_source TEXT,
			title_generated_at TIMESTAMP,
			title_basis_msg_seq INTEGER DEFAULT 0,
			title_skipped_at TIMESTAMP,
			provider TEXT NOT NULL,
			provider_key TEXT,
			model TEXT NOT NULL,
			mode TEXT DEFAULT 'chat',
			origin TEXT DEFAULT 'tui',
			agent TEXT,
			cwd TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			archived BOOLEAN DEFAULT FALSE,
			pinned BOOLEAN DEFAULT FALSE,
			parent_id TEXT REFERENCES sessions(id),
			search BOOLEAN DEFAULT FALSE,
			tools TEXT,
			mcp TEXT,
			user_turns INTEGER DEFAULT 0,
			llm_turns INTEGER DEFAULT 0,
			tool_calls INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			cached_input_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			last_total_tokens INTEGER DEFAULT 0,
			last_message_count INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			tags TEXT,
			compaction_seq INTEGER DEFAULT -1,
			last_user_message_at TIMESTAMP,
			reasoning_effort TEXT
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool', 'developer')),
			parts TEXT NOT NULL,
			text_content TEXT,
			duration_ms INTEGER,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			sequence INTEGER NOT NULL
		);
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (19);
		INSERT INTO sessions (id, name, summary, provider, model, created_at, updated_at)
			VALUES ('sess1', '', '', 'test', 'test-model', '2024-01-01 00:00:00', '2024-01-01 00:00:00');
		INSERT INTO messages (session_id, role, parts, text_content, created_at, sequence)
			VALUES ('sess1', 'user', '[]', 'hi', '2024-01-01 00:00:00', 0);
		INSERT INTO messages (session_id, role, parts, text_content, created_at, sequence)
			VALUES ('sess1', 'assistant', '[]', 'hello', '2024-01-02 00:00:00', 1);
		INSERT INTO messages (session_id, role, parts, text_content, created_at, sequence)
			VALUES ('sess1', 'tool', '[]', 'tool result', '2024-01-03 00:00:00', 2);
		INSERT INTO messages (session_id, role, parts, text_content, created_at, sequence)
			VALUES ('sess1', 'developer', '[]', 'dev note', '2024-01-04 00:00:00', 3);
	`)
	if err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	summaries, err := store.List(context.Background(), ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	wantYear := 2024
	if summaries[0].LastMessageAt.Year() != wantYear {
		t.Fatalf("LastMessageAt = %v, want year %d", summaries[0].LastMessageAt, wantYear)
	}
	if summaries[0].LastMessageAt.Day() != 2 {
		t.Fatalf("LastMessageAt day = %d, want 2 (assistant message)", summaries[0].LastMessageAt.Day())
	}
	if summaries[0].MessageCount != 2 {
		t.Fatalf("MessageCount = %d, want 2 after migration backfill", summaries[0].MessageCount)
	}
}

func TestSQLiteStoreMigratesMessagesTableToAllowDeveloperRole(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			number INTEGER,
			name TEXT,
			summary TEXT,
			generated_short_title TEXT,
			generated_long_title TEXT,
			title_source TEXT,
			title_generated_at TIMESTAMP,
			title_basis_msg_seq INTEGER DEFAULT 0,
			title_skipped_at TIMESTAMP,
			provider TEXT NOT NULL,
			provider_key TEXT,
			model TEXT NOT NULL,
			mode TEXT DEFAULT 'chat',
			origin TEXT DEFAULT 'tui',
			agent TEXT,
			cwd TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			archived BOOLEAN DEFAULT FALSE,
			pinned BOOLEAN DEFAULT FALSE,
			parent_id TEXT REFERENCES sessions(id),
			search BOOLEAN DEFAULT FALSE,
			tools TEXT,
			mcp TEXT,
			user_turns INTEGER DEFAULT 0,
			llm_turns INTEGER DEFAULT 0,
			tool_calls INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			cached_input_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			tags TEXT,
			compaction_seq INTEGER DEFAULT -1,
			last_user_message_at TIMESTAMP,
			last_message_at TIMESTAMP
		);
		CREATE UNIQUE INDEX idx_sessions_number ON sessions(number);
		CREATE INDEX idx_sessions_updated_at ON sessions(updated_at DESC);
		CREATE INDEX idx_sessions_mode ON sessions(mode);
		CREATE INDEX idx_sessions_origin ON sessions(origin);
		CREATE INDEX idx_sessions_pinned ON sessions(pinned);
		CREATE INDEX idx_sessions_last_user_msg ON sessions(last_user_message_at DESC);
		CREATE INDEX idx_sessions_last_message ON sessions(last_message_at DESC);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
			role TEXT NOT NULL CHECK (role IN ('user', 'assistant', 'system', 'tool')),
			parts TEXT NOT NULL,
			text_content TEXT,
			duration_ms INTEGER,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			sequence INTEGER NOT NULL
		);
		CREATE INDEX idx_messages_session_id ON messages(session_id, sequence);
		CREATE UNIQUE INDEX idx_messages_session_sequence ON messages(session_id, sequence);
		CREATE VIRTUAL TABLE messages_fts USING fts5(
			text_content,
			content='messages',
			content_rowid='id'
		);
		CREATE TRIGGER messages_ai AFTER INSERT ON messages BEGIN
			INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
		END;
		CREATE TRIGGER messages_ad AFTER DELETE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
		END;
		CREATE TRIGGER messages_au AFTER UPDATE ON messages BEGIN
			INSERT INTO messages_fts(messages_fts, rowid, text_content) VALUES ('delete', old.id, old.text_content);
			INSERT INTO messages_fts(rowid, text_content) VALUES (new.id, new.text_content);
		END;
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (16);
	`)
	if err != nil {
		t.Fatalf("seed schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("failed to open migrated sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	msg := NewMessage(sess.ID, llm.Message{Role: llm.RoleDeveloper, Parts: []llm.Part{{Type: llm.PartText, Text: "Be concise"}}}, -1)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatalf("AddMessage developer after migration: %v", err)
	}
}

func TestSQLiteStoreGeneratedTitleRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	sess := &Session{
		ID:                  NewID(),
		Provider:            "test",
		Model:               "test-model",
		Mode:                ModeChat,
		Summary:             "very long first prompt",
		GeneratedShortTitle: "Fixing weird docs homepage",
		GeneratedLongTitle:  "Cleaning docs homepage and removing confusing front-page sections",
		TitleSource:         TitleSourceGenerated,
		TitleGeneratedAt:    now,
		TitleBasisMsgSeq:    7,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	loaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.GeneratedShortTitle != sess.GeneratedShortTitle {
		t.Fatalf("GeneratedShortTitle = %q", loaded.GeneratedShortTitle)
	}
	if loaded.GeneratedLongTitle != sess.GeneratedLongTitle {
		t.Fatalf("GeneratedLongTitle = %q", loaded.GeneratedLongTitle)
	}
	if loaded.TitleSource != TitleSourceGenerated {
		t.Fatalf("TitleSource = %q", loaded.TitleSource)
	}
	if loaded.TitleBasisMsgSeq != 7 {
		t.Fatalf("TitleBasisMsgSeq = %d", loaded.TitleBasisMsgSeq)
	}
	if loaded.TitleGeneratedAt.IsZero() {
		t.Fatal("TitleGeneratedAt should be set")
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(summaries))
	}
	if summaries[0].PreferredShortTitle() != sess.GeneratedShortTitle {
		t.Fatalf("PreferredShortTitle() = %q", summaries[0].PreferredShortTitle())
	}
}

func TestSQLiteStoreSessionOriginRoundTripAndFiltering(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	sessions := []*Session{
		{
			ID:        NewID(),
			Provider:  "test",
			Model:     "test-model",
			Mode:      ModeChat,
			Origin:    OriginTUI,
			Pinned:    true,
			Summary:   "tui chat",
			CreatedAt: now,
			UpdatedAt: now,
		},
		{
			ID:        NewID(),
			Provider:  "test",
			Model:     "test-model",
			Mode:      ModeChat,
			Origin:    OriginWeb,
			Summary:   "web chat",
			CreatedAt: now.Add(time.Second),
			UpdatedAt: now.Add(time.Second),
		},
		{
			ID:        NewID(),
			Provider:  "test",
			Model:     "test-model",
			Mode:      ModeAsk,
			Origin:    OriginTUI,
			Summary:   "ask session",
			CreatedAt: now.Add(2 * time.Second),
			UpdatedAt: now.Add(2 * time.Second),
		},
	}
	for _, sess := range sessions {
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s): %v", sess.Summary, err)
		}
	}

	loaded, err := store.Get(ctx, sessions[1].ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if loaded.Origin != OriginWeb {
		t.Fatalf("Origin = %q, want %q", loaded.Origin, OriginWeb)
	}
	if !sessions[0].Pinned {
		t.Fatal("expected first fixture to be pinned")
	}

	loadedPinned, err := store.Get(ctx, sessions[0].ID)
	if err != nil {
		t.Fatalf("Get pinned: %v", err)
	}
	if !loadedPinned.Pinned {
		t.Fatal("Pinned = false, want true")
	}

	summaries, err := store.List(ctx, ListOptions{
		Limit:      10,
		Categories: []string{"chat", "web"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summary count = %d, want 2", len(summaries))
	}
	if summaries[0].ID != sessions[0].ID {
		t.Fatalf("first summary = %q, want pinned session %q", summaries[0].ID, sessions[0].ID)
	}
	if !summaries[0].Pinned {
		t.Fatal("first summary Pinned = false, want true")
	}
	for _, sum := range summaries {
		if sum.Mode == ModeAsk {
			t.Fatalf("unexpected ask session in filtered results: %+v", sum)
		}
	}
}

func TestSQLiteStoreUpdateMetricsIncludesCachedTokens(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:        NewID(),
		Provider:  "ChatGPT (gpt-5.2-codex)",
		Model:     "gpt-5.2-codex",
		Mode:      ModeChat,
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	// Turn 1: 1000 input, 250 output, 700 cache read, 500 cache write
	if err := store.UpdateMetrics(ctx, sess.ID, 2, 3, 1000, 250, 700, 500); err != nil {
		t.Fatalf("failed to update session metrics: %v", err)
	}

	loaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to load session: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session to exist")
	}

	if loaded.LLMTurns != 2 {
		t.Errorf("expected llm_turns=2, got %d", loaded.LLMTurns)
	}
	if loaded.ToolCalls != 3 {
		t.Errorf("expected tool_calls=3, got %d", loaded.ToolCalls)
	}
	if loaded.InputTokens != 1000 {
		t.Errorf("expected input_tokens=1000, got %d", loaded.InputTokens)
	}
	if loaded.OutputTokens != 250 {
		t.Errorf("expected output_tokens=250, got %d", loaded.OutputTokens)
	}
	if loaded.CachedInputTokens != 700 {
		t.Errorf("expected cached_input_tokens=700, got %d", loaded.CachedInputTokens)
	}
	if loaded.CacheWriteTokens != 500 {
		t.Errorf("expected cache_write_tokens=500, got %d", loaded.CacheWriteTokens)
	}

	// Turn 2: verify accumulation
	if err := store.UpdateMetrics(ctx, sess.ID, 1, 2, 50, 100, 1200, 50); err != nil {
		t.Fatalf("failed to update session metrics (turn 2): %v", err)
	}
	if err := store.UpdateContextEstimate(ctx, sess.ID, 127_637, 42); err != nil {
		t.Fatalf("failed to update context estimate: %v", err)
	}
	loaded2, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to load session after turn 2: %v", err)
	}
	if loaded2.InputTokens != 1050 {
		t.Errorf("expected input_tokens=1050 after accumulation, got %d", loaded2.InputTokens)
	}
	if loaded2.CachedInputTokens != 1900 {
		t.Errorf("expected cached_input_tokens=1900 after accumulation, got %d", loaded2.CachedInputTokens)
	}
	if loaded2.CacheWriteTokens != 550 {
		t.Errorf("expected cache_write_tokens=550 after accumulation, got %d", loaded2.CacheWriteTokens)
	}
	if loaded2.OutputTokens != 350 {
		t.Errorf("expected output_tokens=350 after accumulation, got %d", loaded2.OutputTokens)
	}
	if loaded2.LastTotalTokens != 127_637 {
		t.Errorf("expected last_total_tokens=127637 after update, got %d", loaded2.LastTotalTokens)
	}
	if loaded2.LastMessageCount != 42 {
		t.Errorf("expected last_message_count=42 after update, got %d", loaded2.LastMessageCount)
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session summary, got %d", len(summaries))
	}
	if summaries[0].CachedInputTokens != 1900 {
		t.Errorf("expected summary cached_input_tokens=1900, got %d", summaries[0].CachedInputTokens)
	}
	if summaries[0].CacheWriteTokens != 550 {
		t.Errorf("expected summary cache_write_tokens=550, got %d", summaries[0].CacheWriteTokens)
	}
}

func TestUpdateDoesNotClobberTokenMetrics(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:       NewID(),
		Provider: "test",
		Model:    "test-model",
		Mode:     ModeChat,
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Accumulate metrics via UpdateMetrics
	if err := store.UpdateMetrics(ctx, sess.ID, 3, 5, 1000, 250, 700, 500); err != nil {
		t.Fatalf("UpdateMetrics: %v", err)
	}
	if err := store.UpdateContextEstimate(ctx, sess.ID, 127_637, 42); err != nil {
		t.Fatalf("UpdateContextEstimate: %v", err)
	}

	// Now call Update to change metadata (e.g. summary).
	// The in-memory sess still has zero token counts — this must NOT reset the DB.
	sess.Summary = "updated summary"
	if err := store.Update(ctx, sess); err != nil {
		t.Fatalf("Update: %v", err)
	}

	reloaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if reloaded.Summary != "updated summary" {
		t.Errorf("expected summary=%q, got %q", "updated summary", reloaded.Summary)
	}
	if reloaded.InputTokens != 1000 {
		t.Errorf("Update clobbered input_tokens: expected 1000, got %d", reloaded.InputTokens)
	}
	if reloaded.CachedInputTokens != 700 {
		t.Errorf("Update clobbered cached_input_tokens: expected 700, got %d", reloaded.CachedInputTokens)
	}
	if reloaded.CacheWriteTokens != 500 {
		t.Errorf("Update clobbered cache_write_tokens: expected 500, got %d", reloaded.CacheWriteTokens)
	}
	if reloaded.OutputTokens != 250 {
		t.Errorf("Update clobbered output_tokens: expected 250, got %d", reloaded.OutputTokens)
	}
	if reloaded.LastTotalTokens != 127_637 {
		t.Errorf("Update clobbered last_total_tokens: expected 127637, got %d", reloaded.LastTotalTokens)
	}
	if reloaded.LastMessageCount != 42 {
		t.Errorf("Update clobbered last_message_count: expected 42, got %d", reloaded.LastMessageCount)
	}
	if reloaded.LLMTurns != 3 {
		t.Errorf("Update clobbered llm_turns: expected 3, got %d", reloaded.LLMTurns)
	}
	if reloaded.ToolCalls != 5 {
		t.Errorf("Update clobbered tool_calls: expected 5, got %d", reloaded.ToolCalls)
	}
}

func TestSQLiteStoreCreatesMessagesSessionRoleIndex(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	rows, err := store.db.Query(`PRAGMA index_list(messages)`)
	if err != nil {
		t.Fatalf("PRAGMA index_list(messages): %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var seq int
		var name string
		var unique int
		var origin string
		var partial int
		if err := rows.Scan(&seq, &name, &unique, &origin, &partial); err != nil {
			t.Fatalf("scan index_list row: %v", err)
		}
		if name == "idx_messages_session_role" {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate index_list: %v", err)
	}
	t.Fatal("idx_messages_session_role index was not created")
}

func TestSQLiteStoreCustomPath(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "custom", "sessions.db")

	store, err := NewSQLiteStore(Config{
		Enabled: true,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("failed to create sqlite store with custom path: %v", err)
	}
	defer store.Close()

	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected database file at %q: %v", dbPath, err)
	}
}

func TestSQLiteStoreProviderKeyRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("failed to create sqlite store: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{
		ID:          NewID(),
		Provider:    "OpenAI (gpt-5)",
		ProviderKey: "openai",
		Model:       "gpt-5",
		Mode:        ModeChat,
		CreatedAt:   time.Now(),
		UpdatedAt:   time.Now(),
	}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	loaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to load session: %v", err)
	}
	if loaded == nil {
		t.Fatal("expected session to exist")
	}
	if loaded.ProviderKey != "openai" {
		t.Fatalf("expected provider_key %q, got %q", "openai", loaded.ProviderKey)
	}

	loaded.ProviderKey = "chatgpt"
	if err := store.Update(ctx, loaded); err != nil {
		t.Fatalf("failed to update session: %v", err)
	}

	reloaded, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("failed to reload session: %v", err)
	}
	if reloaded.ProviderKey != "chatgpt" {
		t.Fatalf("expected updated provider_key %q, got %q", "chatgpt", reloaded.ProviderKey)
	}
}

func TestSQLiteStoreMigratesProviderKeyColumn(t *testing.T) {
	tmp := t.TempDir()
	dbPath := filepath.Join(tmp, "sessions-v7.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("failed to open seed database: %v", err)
	}
	seedSQL := `
CREATE TABLE sessions (
    id TEXT PRIMARY KEY,
    number INTEGER,
    name TEXT,
    summary TEXT,
    provider TEXT NOT NULL,
    model TEXT NOT NULL,
    mode TEXT DEFAULT 'chat',
    agent TEXT,
    cwd TEXT,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    archived BOOLEAN DEFAULT FALSE,
    parent_id TEXT REFERENCES sessions(id),
    search BOOLEAN DEFAULT FALSE,
    tools TEXT,
    mcp TEXT,
    user_turns INTEGER DEFAULT 0,
    llm_turns INTEGER DEFAULT 0,
    tool_calls INTEGER DEFAULT 0,
    input_tokens INTEGER DEFAULT 0,
    cached_input_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0,
    status TEXT DEFAULT 'active',
    tags TEXT
);
CREATE TABLE schema_version (version INTEGER NOT NULL);
INSERT INTO schema_version(version) VALUES (7);
`
	if _, err := db.Exec(seedSQL); err != nil {
		db.Close()
		t.Fatalf("failed to seed v7 schema: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("failed to close seed database: %v", err)
	}

	store, err := NewSQLiteStore(Config{
		Enabled: true,
		Path:    dbPath,
	})
	if err != nil {
		t.Fatalf("failed to open migrated sqlite store: %v", err)
	}
	defer store.Close()

	// Verify migration added provider_key.
	rows, err := store.db.Query(`PRAGMA table_info(sessions)`)
	if err != nil {
		t.Fatalf("failed to inspect sessions table: %v", err)
	}
	defer rows.Close()

	var hasProviderKey bool
	for rows.Next() {
		var cid int
		var name, colType string
		var notNull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("failed to scan table info: %v", err)
		}
		if name == "provider_key" {
			hasProviderKey = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("failed iterating table info: %v", err)
	}
	if !hasProviderKey {
		t.Fatal("expected provider_key column after migration")
	}
}

func TestReadOnlyOldDBWithoutCompactionSeq(t *testing.T) {
	// Simulate an old database that doesn't have the compaction_seq column.
	// A read-only store should still be able to read sessions from it.
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")

	// Create DB with old schema (no compaction_seq)
	db, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			id TEXT PRIMARY KEY,
			number INTEGER,
			name TEXT,
			summary TEXT,
			provider TEXT NOT NULL,
			provider_key TEXT,
			model TEXT NOT NULL,
			mode TEXT DEFAULT 'chat',
			agent TEXT,
			cwd TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			archived BOOLEAN DEFAULT FALSE,
			parent_id TEXT REFERENCES sessions(id),
			search BOOLEAN DEFAULT FALSE,
			tools TEXT,
			mcp TEXT,
			user_turns INTEGER DEFAULT 0,
			llm_turns INTEGER DEFAULT 0,
			tool_calls INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			cached_input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			tags TEXT
		);
		CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT);
	`)
	if err != nil {
		t.Fatalf("create schema: %v", err)
	}

	// Insert a test session
	_, err = db.Exec(`
		INSERT INTO sessions (id, number, name, summary, provider, model, created_at, updated_at)
		VALUES ('test-id-123', 1, 'test session', 'summary', 'openai', 'gpt-5', datetime('now'), datetime('now'))
	`)
	if err != nil {
		t.Fatalf("insert session: %v", err)
	}
	db.Close()

	// Open in read-only mode (skips migrations)
	store, err := NewSQLiteStore(Config{
		Path:     dbPath,
		ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("open read-only store: %v", err)
	}
	defer store.Close()

	if store.hasCompactionSeq {
		t.Error("old DB should not have compaction_seq")
	}

	ctx := context.Background()

	// Get should work and default CompactionSeq to -1
	sess, err := store.Get(ctx, "test-id-123")
	if err != nil {
		t.Fatalf("Get failed on old DB: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session, got nil")
	}
	if sess.CompactionSeq != -1 {
		t.Errorf("CompactionSeq = %d, want -1 (default for missing column)", sess.CompactionSeq)
	}

	// GetByNumber should also work
	sess, err = store.GetByNumber(ctx, 1)
	if err != nil {
		t.Fatalf("GetByNumber failed on old DB: %v", err)
	}
	if sess == nil {
		t.Fatal("expected session from GetByNumber, got nil")
	}
	if sess.CompactionSeq != -1 {
		t.Errorf("CompactionSeq = %d, want -1", sess.CompactionSeq)
	}
}

func TestSQLiteStoreCompactMessagesIncrementsCompactionCount(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "sessions.db")
	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := store.AddMessage(ctx, sess.ID, &Message{Role: llm.RoleUser, TextContent: "hello"}); err != nil {
		t.Fatalf("AddMessage: %v", err)
	}

	if err := store.CompactMessages(ctx, sess.ID, []Message{{Role: llm.RoleAssistant, TextContent: "summary one"}}); err != nil {
		t.Fatalf("CompactMessages #1: %v", err)
	}
	got, err := store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after #1: %v", err)
	}
	if got.CompactionCount != 1 || got.CompactionSeq != 1 {
		t.Fatalf("after #1 count/seq = %d/%d, want 1/1", got.CompactionCount, got.CompactionSeq)
	}

	if err := store.CompactMessages(ctx, sess.ID, []Message{{Role: llm.RoleAssistant, TextContent: "summary two"}}); err != nil {
		t.Fatalf("CompactMessages #2: %v", err)
	}
	got, err = store.Get(ctx, sess.ID)
	if err != nil {
		t.Fatalf("Get after #2: %v", err)
	}
	if got.CompactionCount != 2 || got.CompactionSeq != 2 {
		t.Fatalf("after #2 count/seq = %d/%d, want 2/2", got.CompactionCount, got.CompactionSeq)
	}
}

func TestSQLiteStoreMigratesCompactionCountColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE sessions (
			id TEXT PRIMARY KEY,
			number INTEGER,
			name TEXT,
			summary TEXT,
			provider TEXT NOT NULL,
			provider_key TEXT,
			model TEXT NOT NULL,
			mode TEXT DEFAULT 'chat',
			agent TEXT,
			cwd TEXT,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			archived BOOLEAN DEFAULT FALSE,
			parent_id TEXT,
			search BOOLEAN DEFAULT FALSE,
			tools TEXT,
			mcp TEXT,
			user_turns INTEGER DEFAULT 0,
			llm_turns INTEGER DEFAULT 0,
			tool_calls INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			cached_input_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			last_total_tokens INTEGER DEFAULT 0,
			last_message_count INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			tags TEXT,
			compaction_seq INTEGER DEFAULT -1
		);
		CREATE TABLE messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL,
			role TEXT NOT NULL,
			parts TEXT NOT NULL,
			text_content TEXT,
			duration_ms INTEGER,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			sequence INTEGER NOT NULL
		);
		CREATE TABLE metadata (key TEXT PRIMARY KEY, value TEXT);
		PRAGMA user_version = 22;
	`)
	if err != nil {
		db.Close()
		t.Fatalf("create old schema: %v", err)
	}
	db.Close()

	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("NewSQLiteStore migrate: %v", err)
	}
	defer store.Close()
	if !store.hasCompactionCount {
		t.Fatal("expected migrated store to detect compaction_count")
	}
	var count int
	if err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('sessions') WHERE name = 'compaction_count'").Scan(&count); err != nil {
		t.Fatalf("query table info: %v", err)
	}
	if count != 1 {
		t.Fatalf("compaction_count column count = %d, want 1", count)
	}
	if err := store.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('messages') WHERE name = 'turn_index'").Scan(&count); err != nil {
		t.Fatalf("query message table info: %v", err)
	}
	if count != 1 {
		t.Fatalf("turn_index column count = %d, want 1", count)
	}
}
