package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func TestPromptHistoryCrossSessionTraversal(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	sessA := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat, Agent: "jarvis"}
	if err := store.Create(ctx, sessA); err != nil {
		t.Fatalf("Create sessA: %v", err)
	}
	sessB := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat, Agent: "jarvis"}
	if err := store.Create(ctx, sessB); err != nil {
		t.Fatalf("Create sessB: %v", err)
	}
	sessBlankAgent := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sessBlankAgent); err != nil {
		t.Fatalf("Create sessBlankAgent: %v", err)
	}
	sessOtherAgent := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat, Agent: "other"}
	if err := store.Create(ctx, sessOtherAgent); err != nil {
		t.Fatalf("Create sessOtherAgent: %v", err)
	}

	addPrompt := func(sess *Session, text string) int64 {
		t.Helper()
		msg := NewMessage(sess.ID, llm.UserText(text), -1)
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%q): %v", text, err)
		}
		return msg.ID
	}

	firstID := addPrompt(sessA, "from terminal one")
	secondID := addPrompt(sessB, "from terminal two")
	blankAgentID := addPrompt(sessBlankAgent, "from default agent")
	_ = addPrompt(sessOtherAgent, "from other agent")

	history, ok := store.(PromptHistoryStore)
	if !ok {
		t.Fatal("store does not implement PromptHistoryStore")
	}

	latest, err := history.PreviousUserPrompt(ctx, "jarvis", 0)
	if err != nil {
		t.Fatalf("PreviousUserPrompt newest: %v", err)
	}
	if latest == nil || latest.ID != blankAgentID || latest.Text != "from default agent" {
		t.Fatalf("latest = %#v, want id=%d text=%q", latest, blankAgentID, "from default agent")
	}

	older, err := history.PreviousUserPrompt(ctx, "jarvis", latest.ID)
	if err != nil {
		t.Fatalf("PreviousUserPrompt older: %v", err)
	}
	if older == nil || older.ID != secondID || older.Text != "from terminal two" {
		t.Fatalf("older = %#v, want id=%d text=%q", older, secondID, "from terminal two")
	}

	oldest, err := history.PreviousUserPrompt(ctx, "jarvis", older.ID)
	if err != nil {
		t.Fatalf("PreviousUserPrompt oldest: %v", err)
	}
	if oldest == nil || oldest.ID != firstID || oldest.Text != "from terminal one" {
		t.Fatalf("oldest = %#v, want id=%d text=%q", oldest, firstID, "from terminal one")
	}

	newer, err := history.NextUserPrompt(ctx, "jarvis", older.ID)
	if err != nil {
		t.Fatalf("NextUserPrompt: %v", err)
	}
	if newer == nil || newer.ID != blankAgentID || newer.Text != "from default agent" {
		t.Fatalf("newer = %#v, want id=%d text=%q", newer, blankAgentID, "from default agent")
	}
}

func TestPromptHistoryOutsideSessionTraversalByDateAcrossAgents(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	current := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat, Agent: "jarvis"}
	if err := store.Create(ctx, current); err != nil {
		t.Fatalf("Create current: %v", err)
	}
	other := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat, Agent: "reviewer"}
	if err := store.Create(ctx, other); err != nil {
		t.Fatalf("Create other: %v", err)
	}
	defaultAgent := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, defaultAgent); err != nil {
		t.Fatalf("Create defaultAgent: %v", err)
	}

	base := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	addPrompt := func(sess *Session, text string, at time.Time) int64 {
		t.Helper()
		msg := NewMessage(sess.ID, llm.UserText(text), -1)
		msg.CreatedAt = at
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%q): %v", text, err)
		}
		return msg.ID
	}

	olderID := addPrompt(defaultAgent, "default older", base.Add(time.Minute))
	newerID := addPrompt(other, "reviewer newer", base.Add(2*time.Minute))
	_ = addPrompt(current, "current newest excluded", base.Add(3*time.Minute))

	history, ok := store.(PromptHistoryOutsideSessionStore)
	if !ok {
		t.Fatal("store does not implement PromptHistoryOutsideSessionStore")
	}

	latest, err := history.PreviousUserPromptOutsideSession(ctx, current.ID, 0, time.Time{})
	if err != nil {
		t.Fatalf("PreviousUserPromptOutsideSession newest: %v", err)
	}
	if latest == nil || latest.ID != newerID || latest.Text != "reviewer newer" {
		t.Fatalf("latest = %#v, want id=%d text=%q", latest, newerID, "reviewer newer")
	}

	older, err := history.PreviousUserPromptOutsideSession(ctx, current.ID, latest.ID, latest.CreatedAt)
	if err != nil {
		t.Fatalf("PreviousUserPromptOutsideSession older: %v", err)
	}
	if older == nil || older.ID != olderID || older.Text != "default older" {
		t.Fatalf("older = %#v, want id=%d text=%q", older, olderID, "default older")
	}

	newer, err := history.NextUserPromptOutsideSession(ctx, current.ID, older.ID, older.CreatedAt)
	if err != nil {
		t.Fatalf("NextUserPromptOutsideSession: %v", err)
	}
	if newer == nil || newer.ID != newerID || newer.Text != "reviewer newer" {
		t.Fatalf("newer = %#v, want id=%d text=%q", newer, newerID, "reviewer newer")
	}
}

func TestPromptHistoryOutsideSessionNextSkipsCursorWithRealStoredTimestamps(t *testing.T) {
	store, err := NewStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	defer store.Close()
	ctx := context.Background()

	current := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, current); err != nil {
		t.Fatalf("Create current: %v", err)
	}
	other := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat, Agent: "reviewer"}
	if err := store.Create(ctx, other); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	addPrompt := func(text string) int64 {
		t.Helper()
		msg := NewMessage(other.ID, llm.UserText(text), -1)
		// Do not overwrite CreatedAt. This preserves time.Now's monotonic clock
		// suffix in the value sent to SQLite, matching real AddMessage callers and
		// guarding against the cursor row being returned by Next again.
		if err := store.AddMessage(ctx, other.ID, msg); err != nil {
			t.Fatalf("AddMessage(%q): %v", text, err)
		}
		time.Sleep(2 * time.Millisecond)
		return msg.ID
	}
	_ = addPrompt("external one")
	middleID := addPrompt("external two")
	latestID := addPrompt("external three")

	history, ok := store.(PromptHistoryOutsideSessionStore)
	if !ok {
		t.Fatal("store does not implement PromptHistoryOutsideSessionStore")
	}
	latest, err := history.PreviousUserPromptOutsideSession(ctx, current.ID, 0, time.Time{})
	if err != nil {
		t.Fatalf("PreviousUserPromptOutsideSession latest: %v", err)
	}
	if latest == nil || latest.ID != latestID {
		t.Fatalf("latest = %#v, want id=%d", latest, latestID)
	}
	middle, err := history.PreviousUserPromptOutsideSession(ctx, current.ID, latest.ID, latest.CreatedAt)
	if err != nil {
		t.Fatalf("PreviousUserPromptOutsideSession middle: %v", err)
	}
	if middle == nil || middle.ID != middleID {
		t.Fatalf("middle = %#v, want id=%d", middle, middleID)
	}

	newer, err := history.NextUserPromptOutsideSession(ctx, current.ID, middle.ID, middle.CreatedAt)
	if err != nil {
		t.Fatalf("NextUserPromptOutsideSession: %v", err)
	}
	if newer == nil || newer.ID != latestID {
		t.Fatalf("newer = %#v, want id=%d (not the cursor id=%d)", newer, latestID, middleID)
	}
}

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

func TestInternalCompactionSummarySQLPrefixMatchesLLM(t *testing.T) {
	if !llm.IsInternalCompactionSummaryText(internalCompactionSummarySQLPrefix + "\nsummary") {
		t.Fatalf("internalCompactionSummarySQLPrefix %q no longer matches llm compaction summaries", internalCompactionSummarySQLPrefix)
	}
	if strings.Contains(internalCompactionSummarySQLPrefix, "'") {
		t.Fatalf("internalCompactionSummarySQLPrefix %q must remain safe for SQL literal interpolation", internalCompactionSummarySQLPrefix)
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

func TestSQLiteStoreListByNumberCursorReturnsCompleteSessions(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	seed := []struct {
		status   SessionStatus
		archived bool
	}{
		{status: StatusComplete},
		{status: StatusComplete},
		{status: StatusActive},
		{status: StatusComplete},
		{status: StatusComplete},
		{status: StatusComplete, archived: true},
	}
	for i, tc := range seed {
		sess := &Session{
			ID:       NewID(),
			Provider: "test",
			Model:    "test-model",
			Mode:     ModeChat,
			Status:   tc.status,
			Archived: tc.archived,
			Name:     fmt.Sprintf("session-%d", i+1),
		}
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%d): %v", i, err)
		}
	}

	page1, err := store.List(ctx, ListOptions{Status: StatusComplete, Limit: 2, SortByNumberDesc: true})
	if err != nil {
		t.Fatalf("List page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("len(page1) = %d, want 2", len(page1))
	}
	if page1[0].Number != 5 || page1[1].Number != 4 {
		t.Fatalf("page1 numbers = [%d %d], want [5 4]", page1[0].Number, page1[1].Number)
	}

	page2, err := store.List(ctx, ListOptions{Status: StatusComplete, Limit: 2, BeforeNumber: page1[len(page1)-1].Number, SortByNumberDesc: true})
	if err != nil {
		t.Fatalf("List page2: %v", err)
	}
	if len(page2) != 2 {
		t.Fatalf("len(page2) = %d, want 2", len(page2))
	}
	if page2[0].Number != 2 || page2[1].Number != 1 {
		t.Fatalf("page2 numbers = [%d %d], want [2 1]", page2[0].Number, page2[1].Number)
	}

	page3, err := store.List(ctx, ListOptions{Status: StatusComplete, Limit: 2, BeforeNumber: page2[len(page2)-1].Number, SortByNumberDesc: true})
	if err != nil {
		t.Fatalf("List page3: %v", err)
	}
	if len(page3) != 0 {
		t.Fatalf("len(page3) = %d, want 0", len(page3))
	}
}

func TestSQLiteStoreListByNumberCursorUsesSessionNumberIndex(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: ":memory:"})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	plan := sqliteExplainPlan(t, store.db, `EXPLAIN QUERY PLAN
		SELECT s.id, s.number
		FROM sessions s INDEXED BY idx_sessions_number
		WHERE s.status = ? AND s.archived = FALSE AND s.number < ?
		ORDER BY s.number DESC
		LIMIT ?`, string(StatusComplete), 1000, 200)
	if !strings.Contains(plan, "idx_sessions_number") {
		t.Fatalf("query plan = %q, want session number index", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("query plan = %q, want no temp sort", plan)
	}
}

func sqliteExplainPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("explain query: %v", err)
	}
	defer rows.Close()

	var details []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan explain row: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	return strings.Join(details, "\n")
}

func TestInitSchemaFreshDBDoesNotRunHistoricalMigrations(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	originalMigrations := migrations
	migrations = []migration{{
		version:     1,
		description: "sentinel migration",
		up: func(db *sql.DB) error {
			_, err := db.Exec(`CREATE TABLE migration_sentinel (id INTEGER PRIMARY KEY)`)
			return err
		},
	}}
	defer func() {
		migrations = originalMigrations
	}()

	if err := initSchema(db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil {
		t.Fatalf("read schema version: %v", err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}

	var sentinelCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='migration_sentinel'
	`).Scan(&sentinelCount); err != nil {
		t.Fatalf("check sentinel table: %v", err)
	}
	if sentinelCount != 0 {
		t.Fatal("fresh DB should not replay historical migrations")
	}

	for _, tc := range []struct {
		objectType string
		name       string
	}{
		{objectType: "table", name: "push_subscriptions"},
		{objectType: "table", name: "session_provider_state"},
		{objectType: "index", name: "idx_messages_session_sequence"},
		{objectType: "index", name: "idx_sessions_status"},
		{objectType: "index", name: "idx_sessions_title_skipped"},
		{objectType: "index", name: "idx_sessions_last_user_msg"},
		{objectType: "index", name: "idx_sessions_last_message"},
	} {
		var count int
		if err := db.QueryRow(`
			SELECT COUNT(*) FROM sqlite_master
			WHERE type = ? AND name = ?
		`, tc.objectType, tc.name).Scan(&count); err != nil {
			t.Fatalf("check %s %s: %v", tc.objectType, tc.name, err)
		}
		if count != 1 {
			t.Fatalf("fresh DB missing %s %s", tc.objectType, tc.name)
		}
	}
}

func TestInitSchemaExistingDBWithoutSchemaVersionRunsMigrations(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()

	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("seed schema without version table: %v", err)
	}

	originalMigrations := migrations
	migrations = []migration{{
		version:     1,
		description: "sentinel migration",
		up: func(db *sql.DB) error {
			_, err := db.Exec(`CREATE TABLE migration_sentinel (id INTEGER PRIMARY KEY)`)
			return err
		},
	}}
	defer func() {
		migrations = originalMigrations
	}()

	if err := initSchema(db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	var sentinelCount int
	if err := db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type='table' AND name='migration_sentinel'
	`).Scan(&sentinelCount); err != nil {
		t.Fatalf("check sentinel table: %v", err)
	}
	if sentinelCount != 1 {
		t.Fatal("existing DB without schema_version should still run migrations")
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

func TestSQLiteStoreGetMessagesPageDescendingHonorsBeforeSeqAndLimit(t *testing.T) {
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

	seed := []Message{
		*NewMessage(sess.ID, llm.UserText("msg-0"), 0),
		*NewMessage(sess.ID, llm.AssistantText("msg-1"), 1),
		*NewMessage(sess.ID, llm.Message{Role: llm.RoleTool, Parts: []llm.Part{{Type: llm.PartText, Text: "tool-2"}}}, 2),
		*NewMessage(sess.ID, llm.AssistantText("msg-3"), 3),
	}
	for i := range seed {
		if err := store.AddMessage(ctx, sess.ID, &seed[i]); err != nil {
			t.Fatalf("AddMessage(%d): %v", i, err)
		}
	}

	got, err := store.GetMessagesPageDescending(ctx, sess.ID, 0, 2)
	if err != nil {
		t.Fatalf("GetMessagesPageDescending latest: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("latest len = %d, want 2", len(got))
	}
	if got[0].Sequence != 3 || got[1].Sequence != 2 {
		t.Fatalf("latest sequences = [%d %d], want [3 2]", got[0].Sequence, got[1].Sequence)
	}

	got, err = store.GetMessagesPageDescending(ctx, sess.ID, 2, 2)
	if err != nil {
		t.Fatalf("GetMessagesPageDescending before seq: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("before seq len = %d, want 2", len(got))
	}
	if got[0].Sequence != 1 || got[1].Sequence != 0 {
		t.Fatalf("before seq sequences = [%d %d], want [1 0]", got[0].Sequence, got[1].Sequence)
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

	compactionTail := NewMessage(sess.ID, llm.AssistantText("hidden retained tail"), 3)
	compactionTail.CompactionTail = true
	if err := store.AddMessage(ctx, sess.ID, compactionTail); err != nil {
		t.Fatalf("AddMessage(compaction tail): %v", err)
	}

	got, err := store.GetLatestVisibleMessageID(ctx, sess.ID)
	if err != nil {
		t.Fatalf("GetLatestVisibleMessageID: %v", err)
	}
	if got != assistant.ID {
		t.Fatalf("latest visible message id = %d, want %d", got, assistant.ID)
	}
}

func TestSQLiteStorePersistsCompactionTailFlag(t *testing.T) {
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

	visible := NewMessage(sess.ID, llm.UserText("visible prompt"), -1)
	if err := store.AddMessage(ctx, sess.ID, visible); err != nil {
		t.Fatalf("AddMessage(visible): %v", err)
	}
	hidden := NewMessage(sess.ID, llm.AssistantText("hidden retained tail"), -1)
	hidden.CompactionTail = true
	if err := store.AddMessage(ctx, sess.ID, hidden); err != nil {
		t.Fatalf("AddMessage(hidden): %v", err)
	}

	all, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(all) != 2 || all[0].CompactionTail || !all[1].CompactionTail {
		t.Fatalf("compaction tail flags from GetMessages = %#v", all)
	}
	from, err := store.GetMessagesFrom(ctx, sess.ID, all[1].Sequence, 0)
	if err != nil {
		t.Fatalf("GetMessagesFrom: %v", err)
	}
	if len(from) != 1 || !from[0].CompactionTail {
		t.Fatalf("compaction tail flag from GetMessagesFrom = %#v", from)
	}
	byID, err := store.GetMessageByID(ctx, hidden.ID)
	if err != nil {
		t.Fatalf("GetMessageByID: %v", err)
	}
	if byID == nil || !byID.CompactionTail {
		t.Fatalf("compaction tail flag from GetMessageByID = %#v", byID)
	}

	summaries, err := store.List(ctx, ListOptions{Limit: 1})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 || summaries[0].MessageCount != 1 {
		t.Fatalf("visible message count = %#v, want only non-tail user counted", summaries)
	}
	results, err := store.Search(ctx, SearchOptions{Query: "hidden", Limit: 10})
	if err != nil {
		t.Fatalf("Search hidden: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("hidden compaction tail should not appear in search results: %#v", results)
	}
}

func TestSQLiteStoreMessageCountExcludesNonChatBubbleRowsOnAdd(t *testing.T) {
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

	messages := []*Message{
		NewMessage(sess.ID, llm.UserText("\n\t[Context Compaction]\nInternal context only; not a user command."), -1),
		NewMessage(sess.ID, llm.UserText("please inspect the repo"), -1),
		NewMessage(sess.ID, llm.AssistantText("   \n\t"), -1),
		assistantToolCallOnlyMessage(sess.ID, -1),
		toolResultMessage(sess.ID, -1),
		NewMessage(sess.ID, llm.AssistantText("I found the answer."), -1),
	}
	for _, msg := range messages {
		if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
			t.Fatalf("AddMessage(%s/%q): %v", msg.Role, msg.TextContent, err)
		}
	}

	assertListedMessageCount(t, store, 2)
}

func TestSQLiteStoreMessageCountExcludesNonChatBubbleRowsOnReplaceMessages(t *testing.T) {
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
		*NewMessage(sess.ID, llm.UserText("\n\t[Context Compaction]\nInternal context only; not a user command."), 0),
		*NewMessage(sess.ID, llm.UserText("please inspect the repo"), 1),
		*NewMessage(sess.ID, llm.AssistantText("   \n\t"), 2),
		*assistantToolCallOnlyMessage(sess.ID, 3),
		*toolResultMessage(sess.ID, 4),
		*NewMessage(sess.ID, llm.AssistantText("I found the answer."), 5),
	}
	if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
		t.Fatalf("ReplaceMessages: %v", err)
	}

	assertListedMessageCount(t, store, 2)
}

func TestSQLiteStoreMigration27BackfillsChatBubbleMessageCount(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "sessions.db")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatalf("create seed schema: %v", err)
	}
	_, err = db.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (26);
		INSERT INTO sessions (id, name, summary, provider, model, message_count, created_at, updated_at)
			VALUES ('sess1', '', '', 'test', 'test-model', 6, '2024-01-01 00:00:00', '2024-01-01 00:00:00');
	`)
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
	seedMessages := []*Message{
		NewMessage("sess1", llm.UserText("\n\t[Context Compaction]\nInternal context only; not a user command."), 0),
		NewMessage("sess1", llm.UserText("please inspect the repo"), 1),
		NewMessage("sess1", llm.AssistantText("   \n\t"), 2),
		assistantToolCallOnlyMessage("sess1", 3),
		toolResultMessage("sess1", 4),
		NewMessage("sess1", llm.AssistantText("I found the answer."), 5),
	}
	for _, msg := range seedMessages {
		partsJSON, err := msg.PartsJSON()
		if err != nil {
			t.Fatalf("PartsJSON: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO messages (session_id, role, parts, text_content, duration_ms, turn_index, created_at, sequence, compaction_tail)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			msg.SessionID, string(msg.Role), partsJSON, msg.TextContent, msg.DurationMs, msg.TurnIndex, time.Now().UTC(), msg.Sequence, msg.CompactionTail); err != nil {
			t.Fatalf("insert seed message: %v", err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	store, err := NewSQLiteStore(Config{Enabled: true, Path: dbPath})
	if err != nil {
		t.Fatalf("open migrated store: %v", err)
	}
	defer store.Close()

	assertListedMessageCount(t, store, 2)
}

func TestSQLiteStoreMessageCountTriggersHandleDeleteAndCompactionTailUpdates(t *testing.T) {
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

	user := NewMessage(sess.ID, llm.UserText("visible user"), -1)
	if err := store.AddMessage(ctx, sess.ID, user); err != nil {
		t.Fatalf("AddMessage(user): %v", err)
	}
	toolOnly := assistantToolCallOnlyMessage(sess.ID, -1)
	if err := store.AddMessage(ctx, sess.ID, toolOnly); err != nil {
		t.Fatalf("AddMessage(tool-only assistant): %v", err)
	}
	assistant := NewMessage(sess.ID, llm.AssistantText("visible assistant"), -1)
	if err := store.AddMessage(ctx, sess.ID, assistant); err != nil {
		t.Fatalf("AddMessage(assistant): %v", err)
	}
	assertListedMessageCount(t, store, 2)

	if _, err := store.db.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", toolOnly.ID); err != nil {
		t.Fatalf("delete non-countable assistant: %v", err)
	}
	assertListedMessageCount(t, store, 2)

	if _, err := store.db.ExecContext(ctx, "DELETE FROM messages WHERE id = ?", assistant.ID); err != nil {
		t.Fatalf("delete countable assistant: %v", err)
	}
	assertListedMessageCount(t, store, 1)

	if err := store.PersistCompactionTailHints(ctx, sess.ID, []int64{user.ID}); err != nil {
		t.Fatalf("PersistCompactionTailHints(user): %v", err)
	}
	assertListedMessageCount(t, store, 0)
}

func assistantToolCallOnlyMessage(sessionID string, sequence int) *Message {
	return NewMessage(sessionID, llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type: llm.PartToolCall,
			ToolCall: &llm.ToolCall{
				ID:        "call-1",
				Name:      "read_file",
				Arguments: []byte(`{"path":"README.md"}`),
			},
		}},
	}, sequence)
}

func toolResultMessage(sessionID string, sequence int) *Message {
	return NewMessage(sessionID, llm.Message{
		Role: llm.RoleTool,
		Parts: []llm.Part{{
			Type: llm.PartToolResult,
			ToolResult: &llm.ToolResult{
				ID:      "call-1",
				Name:    "read_file",
				Content: "README contents",
			},
		}},
	}, sequence)
}

func assertListedMessageCount(t *testing.T, store *SQLiteStore, want int) {
	t.Helper()

	summaries, err := store.List(context.Background(), ListOptions{Limit: 5})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("List len = %d, want 1", len(summaries))
	}
	if summaries[0].MessageCount != want {
		t.Fatalf("MessageCount = %d, want %d", summaries[0].MessageCount, want)
	}
}

func TestSQLiteStoreReplaceMessagesRecomputesActivityMetadata(t *testing.T) {
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

	base := time.Date(2024, time.January, 2, 3, 4, 5, 0, time.UTC)
	messages := []Message{
		*NewMessage(sess.ID, llm.UserText("hello"), 0),
		*NewMessage(sess.ID, llm.AssistantText("hi"), 1),
	}
	messages[0].CreatedAt = base
	messages[1].CreatedAt = base.Add(time.Minute)
	if err := store.ReplaceMessages(ctx, sess.ID, messages); err != nil {
		t.Fatalf("initial ReplaceMessages: %v", err)
	}

	assertSessionActivity := func(wantTurns int, wantLastUser, wantLastMessage time.Time) {
		t.Helper()
		var gotTurns int
		var gotLastUser, gotLastMessage sql.NullTime
		if err := store.db.QueryRowContext(ctx,
			`SELECT user_turns, last_user_message_at, last_message_at FROM sessions WHERE id = ?`,
			sess.ID,
		).Scan(&gotTurns, &gotLastUser, &gotLastMessage); err != nil {
			t.Fatalf("scan session metadata: %v", err)
		}
		if gotTurns != wantTurns {
			t.Fatalf("user_turns = %d, want %d", gotTurns, wantTurns)
		}
		if !gotLastUser.Valid || !gotLastUser.Time.Equal(wantLastUser) {
			t.Fatalf("last_user_message_at = %v (valid=%v), want %v", gotLastUser.Time, gotLastUser.Valid, wantLastUser)
		}
		if !gotLastMessage.Valid || !gotLastMessage.Time.Equal(wantLastMessage) {
			t.Fatalf("last_message_at = %v (valid=%v), want %v", gotLastMessage.Time, gotLastMessage.Valid, wantLastMessage)
		}
	}
	assertSessionActivity(1, base, base.Add(time.Minute))

	replacement := []Message{
		*NewMessage(sess.ID, llm.UserText("hello"), 0),
		*NewMessage(sess.ID, llm.AssistantText("hi"), 1),
	}
	replacement[0].CreatedAt = base.Add(24 * time.Hour)
	replacement[1].CreatedAt = base.Add(25 * time.Hour)
	if err := store.ReplaceMessages(ctx, sess.ID, replacement); err != nil {
		t.Fatalf("replacement ReplaceMessages: %v", err)
	}

	assertSessionActivity(1, base, base.Add(time.Minute))
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

	results, err := store.Search(ctx, SearchOptions{Query: "old suffix", Limit: 10})
	if err != nil {
		t.Fatalf("Search old suffix: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("old suffix still in FTS: %#v", results)
	}
	results, err = store.Search(ctx, SearchOptions{Query: "new suffix", Limit: 10})
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
	results, err := store.Search(ctx, SearchOptions{Query: "stale duplicate", Limit: 10})
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

	results, err := store.Search(ctx, SearchOptions{Query: "term-llm", Limit: 10})
	if err != nil {
		t.Fatalf("Search(term-llm) error = %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("Search(term-llm) len = %d, want 1", len(results))
	}
	if results[0].Mode != ModeChat {
		t.Fatalf("Search(term-llm) mode = %q, want %q", results[0].Mode, ModeChat)
	}
	if results[0].Status != StatusActive {
		t.Fatalf("Search(term-llm) status = %q, want %q", results[0].Status, StatusActive)
	}
	if results[0].MessageCount != 1 {
		t.Fatalf("Search(term-llm) message_count = %d, want 1", results[0].MessageCount)
	}
	if results[0].SessionCreatedAt.IsZero() {
		t.Fatal("Search(term-llm) session_created_at = zero, want populated timestamp")
	}
	if results[0].UpdatedAt.IsZero() {
		t.Fatal("Search(term-llm) updated_at = zero, want populated timestamp")
	}
}

func TestSQLiteStoreSearchReturnsDistinctFilteredSessionMatches(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	sessions := []*Session{
		{
			ID:                  "chat-session",
			Provider:            "test",
			ProviderKey:         "test",
			Model:               "test-model",
			Mode:                ModeChat,
			Origin:              OriginTUI,
			GeneratedShortTitle: "Chat result",
			CreatedAt:           now,
			UpdatedAt:           now,
			Status:              StatusActive,
		},
		{
			ID:                  "web-session",
			Provider:            "test",
			ProviderKey:         "test",
			Model:               "test-model",
			Mode:                ModeChat,
			Origin:              OriginWeb,
			GeneratedShortTitle: "Web result",
			CreatedAt:           now.Add(time.Second),
			UpdatedAt:           now.Add(time.Second),
			Status:              StatusActive,
		},
		{
			ID:                  "archived-web-session",
			Provider:            "test",
			ProviderKey:         "test",
			Model:               "test-model",
			Mode:                ModeChat,
			Origin:              OriginWeb,
			GeneratedShortTitle: "Archived web result",
			CreatedAt:           now.Add(2 * time.Second),
			UpdatedAt:           now.Add(2 * time.Second),
			Status:              StatusActive,
			Archived:            true,
		},
	}
	for _, sess := range sessions {
		if err := store.Create(ctx, sess); err != nil {
			t.Fatalf("Create(%s): %v", sess.ID, err)
		}
	}
	for i := 0; i < 3; i++ {
		if err := store.AddMessage(ctx, "chat-session", NewMessage("chat-session", llm.UserText(fmt.Sprintf("shared needle duplicate %d", i)), i)); err != nil {
			t.Fatalf("AddMessage(chat %d): %v", i, err)
		}
	}
	if err := store.AddMessage(ctx, "web-session", NewMessage("web-session", llm.UserText("shared needle web match"), 0)); err != nil {
		t.Fatalf("AddMessage(web): %v", err)
	}
	if err := store.AddMessage(ctx, "archived-web-session", NewMessage("archived-web-session", llm.UserText("shared needle archived match"), 0)); err != nil {
		t.Fatalf("AddMessage(archived): %v", err)
	}

	results, err := store.Search(ctx, SearchOptions{Query: "shared needle", Limit: 2})
	if err != nil {
		t.Fatalf("Search distinct: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("Search distinct len = %d, want 2", len(results))
	}
	seen := map[string]bool{}
	for _, result := range results {
		if seen[result.SessionID] {
			t.Fatalf("duplicate session in results: %#v", results)
		}
		seen[result.SessionID] = true
		if result.Archived {
			t.Fatalf("unexpected archived result in default search: %#v", result)
		}
	}
	if !seen["chat-session"] || !seen["web-session"] {
		t.Fatalf("default search ids = %#v, want chat-session and web-session", results)
	}

	results, err = store.Search(ctx, SearchOptions{Query: "shared needle", Limit: 5, Categories: []string{"web"}})
	if err != nil {
		t.Fatalf("Search web category: %v", err)
	}
	if len(results) != 1 || results[0].SessionID != "web-session" {
		t.Fatalf("web category results = %#v, want only web-session", results)
	}

	results, err = store.Search(ctx, SearchOptions{Query: "shared needle", Limit: 5, Categories: []string{"web"}, Archived: true})
	if err != nil {
		t.Fatalf("Search web include archived: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("web include archived len = %d, want 2", len(results))
	}
	ids := []string{results[0].SessionID, results[1].SessionID}
	sort.Strings(ids)
	if ids[0] != "archived-web-session" || ids[1] != "web-session" {
		t.Fatalf("web include archived ids = %v, want [archived-web-session web-session]", ids)
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

	// Tool/developer/system messages must not bump last_message_at; user and
	// assistant role rows retain the historical activity-sort behavior.
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

func TestUpdateDoesNotClobberUserTurns(t *testing.T) {
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

	if err := store.IncrementUserTurns(ctx, sess.ID); err != nil {
		t.Fatalf("IncrementUserTurns: %v", err)
	}

	// The in-memory sess still has zero user turns — Update must not write that stale value back.
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
	if reloaded.UserTurns != 1 {
		t.Errorf("Update clobbered user_turns: expected 1, got %d", reloaded.UserTurns)
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

func TestSQLiteStoreProviderStateRoundTrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	store, err := NewSQLiteStore(DefaultConfig())
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	ctx := context.Background()
	sess := &Session{ID: NewID(), Provider: "Claude CLI", ProviderKey: "claude-bin", Model: "sonnet", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	initial := []byte(`{"session_id":"claude-1","messages_sent":3}`)
	if err := store.SaveProviderState(ctx, sess.ID, "claude-bin", initial); err != nil {
		t.Fatalf("SaveProviderState initial: %v", err)
	}
	loaded, err := store.LoadProviderState(ctx, sess.ID, "claude-bin")
	if err != nil {
		t.Fatalf("LoadProviderState initial: %v", err)
	}
	if string(loaded) != string(initial) {
		t.Fatalf("provider state = %s, want %s", loaded, initial)
	}

	updated := []byte(`{"session_id":"claude-2","messages_sent":8}`)
	if err := store.SaveProviderState(ctx, sess.ID, "claude-bin", updated); err != nil {
		t.Fatalf("SaveProviderState update: %v", err)
	}
	loaded, err = store.LoadProviderState(ctx, sess.ID, "claude-bin")
	if err != nil {
		t.Fatalf("LoadProviderState update: %v", err)
	}
	if string(loaded) != string(updated) {
		t.Fatalf("updated provider state = %s, want %s", loaded, updated)
	}

	if other, err := store.LoadProviderState(ctx, sess.ID, "openai"); err != nil {
		t.Fatalf("LoadProviderState other provider: %v", err)
	} else if len(other) != 0 {
		t.Fatalf("other provider state = %s, want empty", other)
	}

	if err := store.DeleteProviderState(ctx, sess.ID, "claude-bin"); err != nil {
		t.Fatalf("DeleteProviderState: %v", err)
	}
	loaded, err = store.LoadProviderState(ctx, sess.ID, "claude-bin")
	if err != nil {
		t.Fatalf("LoadProviderState after delete: %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("provider state after delete = %s, want empty", loaded)
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

	var providerStateTables int
	if err := store.db.QueryRow(`
		SELECT COUNT(*) FROM sqlite_master
		WHERE type = 'table' AND name = 'session_provider_state'
	`).Scan(&providerStateTables); err != nil {
		t.Fatalf("failed to inspect session_provider_state table: %v", err)
	}
	if providerStateTables != 1 {
		t.Fatalf("session_provider_state table count = %d, want 1", providerStateTables)
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

func TestSQLiteStoreAddMessageConcurrentAutoSequence(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	sess := &Session{ID: NewID(), Provider: "test", Model: "test-model", Mode: ModeChat}
	if err := store.Create(ctx, sess); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 64
	errCh := make(chan error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		i := i
		go func() {
			<-start
			msg := NewMessage(sess.ID, llm.UserText(fmt.Sprintf("msg-%02d", i)), -1)
			errCh <- store.AddMessage(ctx, sess.ID, msg)
		}()
	}
	close(start)
	for i := 0; i < n; i++ {
		if err := <-errCh; err != nil {
			t.Fatalf("AddMessage concurrent: %v", err)
		}
	}

	msgs, err := store.GetMessages(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("GetMessages: %v", err)
	}
	if len(msgs) != n {
		t.Fatalf("got %d messages, want %d", len(msgs), n)
	}
	for i, msg := range msgs {
		if msg.Sequence != i {
			t.Fatalf("message %d sequence = %d, want %d", i, msg.Sequence, i)
		}
	}
}

func TestSQLiteStoreSidebarListUsesActivityIndexes(t *testing.T) {
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	queries := []struct {
		name      string
		query     string
		wantIndex string
	}{
		{
			name:      "activity",
			query:     `SELECT id FROM sessions WHERE archived = FALSE ORDER BY COALESCE(pinned, FALSE) DESC, COALESCE(last_message_at, last_user_message_at, created_at) DESC LIMIT 100`,
			wantIndex: "idx_sessions_sidebar_activity",
		},
		{
			name:      "last user activity",
			query:     `SELECT id FROM sessions WHERE archived = FALSE ORDER BY COALESCE(pinned, FALSE) DESC, COALESCE(last_user_message_at, created_at) DESC LIMIT 100`,
			wantIndex: "idx_sessions_sidebar_last_user_activity",
		},
	}
	for _, tc := range queries {
		t.Run(tc.name, func(t *testing.T) {
			plan := explainQueryPlan(t, store.db, tc.query)
			if !strings.Contains(plan, tc.wantIndex) {
				t.Fatalf("plan does not use %s:\n%s", tc.wantIndex, plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE") {
				t.Fatalf("plan uses temp sort:\n%s", plan)
			}
		})
	}
}

func explainQueryPlan(t *testing.T, db *sql.DB, query string) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN " + query)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN: %v", err)
	}
	defer rows.Close()

	var parts []string
	for rows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := rows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		parts = append(parts, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate plan: %v", err)
	}
	return strings.Join(parts, "\n")
}
