package cmd

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/session"
	"github.com/samsaffron/term-llm/internal/tools"
)

func newSessionDirectoryTestStore(t *testing.T) *session.SQLiteStore {
	t.Helper()
	store, err := session.NewSQLiteStore(session.Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createDirectorySession(t *testing.T, store session.Store, sess *session.Session) {
	t.Helper()
	if sess.Provider == "" {
		sess.Provider, sess.Model, sess.Mode = "test", "test-model", session.ModeChat
	}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("create %s: %v", sess.ID, err)
	}
}

func addDirectoryMessage(t *testing.T, store session.Store, sessionID, text string) {
	t.Helper()
	if err := store.AddMessage(context.Background(), sessionID, session.NewMessage(sessionID, llm.UserText(text), -1)); err != nil {
		t.Fatalf("add message to %s: %v", sessionID, err)
	}
}

func directoryEntryByID(entries []sessionDirectoryEntry, id string) (sessionDirectoryEntry, bool) {
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return sessionDirectoryEntry{}, false
}

// toolDirectoryEntryByID is the same lookup over the tool adapter's rows.
func toolDirectoryEntryByID(entries []tools.SessionDirectoryEntry, id string) (tools.SessionDirectoryEntry, bool) {
	for _, entry := range entries {
		if entry.ID == id {
			return entry, true
		}
	}
	return tools.SessionDirectoryEntry{}, false
}

func TestSessionDirectoryRecentListOrdersDecoratesAndExcludesSubagents(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	if err := store.CreateProject(ctx, &session.Project{
		ID: "project-1", Name: "Reflow", CanonicalDir: filepath.Join(t.TempDir(), "reflow"),
		CreatedAt: time.Now().Add(-time.Hour), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	createDirectorySession(t, store, &session.Session{
		ID: "dir-old", Summary: "older work", ProjectID: "project-1", ProjectName: "Reflow",
		CreatedAt: base, UpdatedAt: base,
	})
	createDirectorySession(t, store, &session.Session{
		ID: "dir-recent", GeneratedShortTitle: "Fix reflow crash", ProjectID: "project-1", ProjectName: "Reflow",
		CreatedAt: base.Add(time.Minute), UpdatedAt: base.Add(time.Minute),
	})
	createDirectorySession(t, store, &session.Session{
		ID: "dir-child", Summary: "subagent scratch", ParentID: "dir-recent", IsSubagent: true,
		CreatedAt: base.Add(2 * time.Minute), UpdatedAt: base.Add(2 * time.Minute),
	})
	createDirectorySession(t, store, &session.Session{
		ID: "dir-archived", Summary: "archived work", Archived: true,
		CreatedAt: base.Add(3 * time.Minute), UpdatedAt: base.Add(3 * time.Minute),
	})
	addDirectoryMessage(t, store, "dir-old", "older question")
	addDirectoryMessage(t, store, "dir-recent", "recent question")
	addDirectoryMessage(t, store, "dir-child", "child question")

	lifecycle, ok := session.AsServeResponseLifecycleStore(store)
	if !ok {
		t.Fatal("store does not support durable response runs")
	}
	if _, err := lifecycle.AdmitResponseRun(ctx, session.ResponseRunAdmission{
		ResponseID: "reply", SessionID: "dir-recent", RunEpoch: 1,
		OwnerInstanceID: "other-process", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	srv := &serveServer{store: store}

	entries, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[0].ID != "dir-recent" {
		t.Fatalf("activity order = %+v", entries)
	}
	recent, _ := directoryEntryByID(entries, "dir-recent")
	if recent.Title != "Fix reflow crash" || recent.Project != "Reflow" || recent.Number <= 0 ||
		recent.MessageCount != 1 || !recent.Running || recent.NeedsInput {
		t.Fatalf("recent entry = %+v", recent)
	}
	if recent.LastActivity.IsZero() || recent.LastActivity.Location() == nil {
		t.Fatalf("last activity = %v", recent.LastActivity)
	}
	if _, ok := directoryEntryByID(entries, "dir-child"); ok {
		t.Fatal("subagent session listed")
	}
	if _, ok := directoryEntryByID(entries, "dir-archived"); ok {
		t.Fatal("archived session listed without IncludeArchived")
	}

	runningOnly, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{RunningOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(runningOnly) != 1 || runningOnly[0].ID != "dir-recent" {
		t.Fatalf("running filter = %+v", runningOnly)
	}

	archived, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{IncludeArchived: true, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(archived) != 1 {
		t.Fatalf("limit ignored: %+v", archived)
	}

	scoped, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{ProjectID: "missing-project"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped) != 0 {
		t.Fatalf("project filter = %+v", scoped)
	}
}

func TestSessionDirectorySearchUsesTranscriptSnippets(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "dir-haystack", GeneratedShortTitle: "SQLite lookups"})
	createDirectorySession(t, store, &session.Session{ID: "dir-other", Summary: "unrelated"})
	addDirectoryMessage(t, store, "dir-haystack", "the transcript indexer keeps a needle-token marker")
	addDirectoryMessage(t, store, "dir-other", "nothing relevant here")
	srv := &serveServer{store: store}

	entries, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{Query: "needle-token"})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "dir-haystack" {
		t.Fatalf("search entries = %+v", entries)
	}
	if !strings.Contains(entries[0].Snippet, "needle-token") {
		t.Fatalf("snippet = %q", entries[0].Snippet)
	}
	if entries[0].Title != "SQLite lookups" || entries[0].Running {
		t.Fatalf("search entry = %+v", entries[0])
	}
	running, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{Query: "needle-token", RunningOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(running) != 0 {
		t.Fatalf("running-only search = %+v", running)
	}
	if empty, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{Query: "no-such-token-anywhere"}); err != nil || len(empty) != 0 {
		t.Fatalf("empty search = %+v, %v", empty, err)
	}
}

func TestSessionDirectoryReportsMissingStore(t *testing.T) {
	if _, err := (&serveServer{}).sessionDirectory(context.Background(), sessionDirectoryQuery{}); err == nil {
		t.Fatal("missing store accepted")
	}
	if _, err := (*serveServer)(nil).sessionDirectory(context.Background(), sessionDirectoryQuery{}); err == nil {
		t.Fatal("nil server accepted")
	}
}

func TestSessionDirectoryLimitsAreBounded(t *testing.T) {
	store := newSessionDirectoryTestStore(t)
	for i := range sessionDirectoryMaxLimit + 5 {
		createDirectorySession(t, store, &session.Session{ID: session.NewID(), Summary: "bulk " + string(rune('a'+i%26))})
	}
	srv := &serveServer{store: store}
	entries, err := srv.sessionDirectory(context.Background(), sessionDirectoryQuery{Limit: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != sessionDirectoryStoreDefaultLimit {
		t.Fatalf("default limit entries = %d", len(entries))
	}
	entries, err = srv.sessionDirectory(context.Background(), sessionDirectoryQuery{Limit: 10_000})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != sessionDirectoryMaxLimit {
		t.Fatalf("clamped entries = %d", len(entries))
	}
}

func TestSessionDirectoryMarksSessionsWaitingForInput(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "dir-waiting", Summary: "needs approval"})
	createDirectorySession(t, store, &session.Session{ID: "dir-idle", Summary: "nothing pending"})
	addDirectoryMessage(t, store, "dir-waiting", "run the migration")
	addDirectoryMessage(t, store, "dir-idle", "just chatting")
	srv := &serveServer{store: store, sessionMgr: &serveSessionManager{sessions: map[string]*serveRuntime{
		"dir-waiting": {pendingAskUsers: map[string]*servePendingAskUser{
			"ask-1": {CallID: "ask-1", CreatedAt: time.Now()},
		}},
	}}}

	entries, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	waiting, ok := directoryEntryByID(entries, "dir-waiting")
	if !ok || !waiting.NeedsInput {
		t.Fatalf("waiting entry = %+v", waiting)
	}
	idle, ok := directoryEntryByID(entries, "dir-idle")
	if !ok || idle.NeedsInput {
		t.Fatalf("idle entry = %+v", idle)
	}
}

// admitRunningSession publishes the same durable running truth another process or
// a restarted server leaves behind, which is also how a long-running task appears
// to a directory call that does not own its in-memory runtime.
func admitRunningSession(t *testing.T, store session.Store, sessionID string) {
	t.Helper()
	lifecycle, ok := session.AsServeResponseLifecycleStore(store)
	if !ok {
		t.Fatal("store does not support durable response runs")
	}
	if _, err := lifecycle.AdmitResponseRun(context.Background(), session.ResponseRunAdmission{
		ResponseID: "reply-" + sessionID, SessionID: sessionID, RunEpoch: 1,
		OwnerInstanceID: "other-process", StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// seedIdleDirectorySessions creates count idle sessions newer than at, so a
// recency page of any practical limit is filled with sessions that are not running.
func seedIdleDirectorySessions(t *testing.T, store session.Store, count int, at time.Time) {
	t.Helper()
	for i := range count {
		activity := at.Add(time.Duration(i+1) * time.Minute)
		createDirectorySession(t, store, &session.Session{
			ID: fmt.Sprintf("dir-idle-%02d", i), Summary: "idle work",
			CreatedAt: activity, UpdatedAt: activity,
		})
	}
}

// TestSessionDirectoryRunningOnlyIsDrivenByTheRunningSet guards the promise the
// voice prompt now makes: the agent answers "what else is running" with this tool,
// so a running session must never be hidden behind the listing limit. Filtering a
// recency page instead reported "nothing is running" while work was running, with
// no hint that the answer was truncated.
func TestSessionDirectoryRunningOnlyIsDrivenByTheRunningSet(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	started := time.Now().Add(-2 * time.Hour).Truncate(time.Millisecond)
	createDirectorySession(t, store, &session.Session{ID: "dir-old-running", Summary: "long build", CreatedAt: started, UpdatedAt: started})
	seedIdleDirectorySessions(t, store, sessionDirectoryStoreDefaultLimit+5, started)
	admitRunningSession(t, store, "dir-old-running")
	srv := &serveServer{store: store}

	// The tool's own default limit is the smaller of the two, and it is the one the
	// voice model gets without asking.
	for name, limit := range map[string]int{
		"tool default":  10,
		"store default": 0,
		"explicit":      sessionDirectoryStoreDefaultLimit,
	} {
		t.Run(name, func(t *testing.T) {
			entries, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{RunningOnly: true, Limit: limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].ID != "dir-old-running" || !entries[0].Running {
				t.Fatalf("running-only entries = %+v", entries)
			}
		})
	}
	// The listing is still bounded: the running set is not a licence to ignore limit.
	for _, id := range []string{"dir-second-running", "dir-third-running"} {
		createDirectorySession(t, store, &session.Session{ID: id, Summary: "busy", CreatedAt: started, UpdatedAt: started})
		admitRunningSession(t, store, id)
	}
	entries, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{RunningOnly: true, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("running-only limit ignored: %+v", entries)
	}
	for _, entry := range entries {
		if !entry.Running {
			t.Fatalf("non-running entry under running_only: %+v", entry)
		}
	}
}

// TestSessionDirectoryRunningOnlyWithNothingRunningIsEmpty pins the idle instance.
// "What is running?" is answered from the running set, and an empty set is not a
// filter: the store reads IDs only when the slice is non-empty, so passing it through
// lists the whole directory with every row flagged as running — an answer the voice
// model speaks with confidence and the user has no way to doubt.
func TestSessionDirectoryRunningOnlyWithNothingRunningIsEmpty(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	seedIdleDirectorySessions(t, store, 5, time.Now().Add(-time.Hour))
	srv := &serveServer{store: store}

	entries, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{RunningOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("an idle instance reported %d running sessions: %+v", len(entries), entries)
	}
	// The same through the tool the voice model calls, including its own default
	// limit, which is the smaller of the two.
	for name, limit := range map[string]int{"tool default": 10, "store default": 0} {
		t.Run(name, func(t *testing.T) {
			toolEntries, err := srv.sessionDirectoryForTool(ctx, tools.SessionDirectoryQuery{RunningOnly: true, Limit: limit})
			if err != nil {
				t.Fatal(err)
			}
			if len(toolEntries) != 0 {
				t.Fatalf("running-only tool lookup reported %+v", toolEntries)
			}
		})
	}
	// The sessions are still there: this is about the running filter, not about the
	// listing.
	listed, err := srv.sessionDirectory(ctx, sessionDirectoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 5 {
		t.Fatalf("idle sessions = %+v", listed)
	}
}

// TestSessionDirectoryForToolForwardsIncludeArchived guards the loop the voice
// model is told to close: it is instructed to find a session with
// session_directory and then switch to it, and an archived session is a legal
// switch target, so the directory has to be able to reach one.
func TestSessionDirectoryForToolForwardsIncludeArchived(t *testing.T) {
	ctx := context.Background()
	store := newSessionDirectoryTestStore(t)
	createDirectorySession(t, store, &session.Session{ID: "dir-live", Summary: "current work"})
	createDirectorySession(t, store, &session.Session{ID: "dir-archived", Summary: "old work", Archived: true})
	addDirectoryMessage(t, store, "dir-archived", "the archived decision was reverted")
	srv := &serveServer{store: store}

	visible, err := srv.sessionDirectoryForTool(ctx, tools.SessionDirectoryQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := toolDirectoryEntryByID(visible, "dir-archived"); ok {
		t.Fatalf("archived session listed by default: %+v", visible)
	}
	withArchived, err := srv.sessionDirectoryForTool(ctx, tools.SessionDirectoryQuery{IncludeArchived: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := toolDirectoryEntryByID(withArchived, "dir-archived"); !ok {
		t.Fatalf("include_archived not forwarded: %+v", withArchived)
	}
	// The same switch reaches an archived session through search as well.
	searched, err := srv.sessionDirectoryForTool(ctx, tools.SessionDirectoryQuery{IncludeArchived: true, Query: "reverted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := toolDirectoryEntryByID(searched, "dir-archived"); !ok {
		t.Fatalf("archived search returned nothing: %+v", searched)
	}
	hidden, err := srv.sessionDirectoryForTool(ctx, tools.SessionDirectoryQuery{Query: "reverted"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Fatalf("archived search without include_archived: %+v", hidden)
	}
}
