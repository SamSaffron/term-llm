package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

func createPinTestSession(t *testing.T, store *SQLiteStore, id string, activity time.Time) {
	t.Helper()
	sess := &Session{ID: id, Provider: "mock", Model: "mock-model", Mode: ModeChat, Origin: OriginWeb, CreatedAt: activity, UpdatedAt: activity, Status: StatusActive}
	if err := store.Create(context.Background(), sess); err != nil {
		t.Fatalf("Create(%s): %v", id, err)
	}
	touchPinTestSession(t, store, id, activity)
}

// touchPinTestSession records conversation activity at the given time.
func touchPinTestSession(t *testing.T, store *SQLiteStore, id string, at time.Time) {
	t.Helper()
	message := NewMessage(id, llm.UserText("message for "+id), -1)
	message.CreatedAt = at
	if err := store.AddMessage(context.Background(), id, message); err != nil {
		t.Fatalf("AddMessage(%s): %v", id, err)
	}
}

func pinTestSessions(t *testing.T, store *SQLiteStore, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := store.SetSessionPinned(context.Background(), id, true); err != nil {
			t.Fatalf("SetSessionPinned(%s): %v", id, err)
		}
	}
}

func summaryIDs(summaries []SessionSummary) []string {
	ids := make([]string, 0, len(summaries))
	for _, summary := range summaries {
		ids = append(ids, summary.ID)
	}
	return ids
}

func listPinTestIDs(t *testing.T, store *SQLiteStore, opts ListOptions) []string {
	t.Helper()
	opts.Limit = -1
	summaries, err := store.List(context.Background(), opts)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return summaryIDs(summaries)
}

func pinRanks(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT id, pin_order FROM sessions WHERE pin_order IS NOT NULL`)
	if err != nil {
		t.Fatalf("query ranks: %v", err)
	}
	defer rows.Close()
	ranks := map[string]int64{}
	for rows.Next() {
		var id string
		var rank int64
		if err := rows.Scan(&id, &rank); err != nil {
			t.Fatalf("scan rank: %v", err)
		}
		ranks[id] = rank
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate ranks: %v", err)
	}
	return ranks
}

func TestUnrankedPinKeySQLMatchesGoValue(t *testing.T) {
	if got := strconv.FormatInt(unrankedPinKey, 10); got != unrankedPinKeySQL {
		t.Fatalf("unrankedPinKeySQL = %s, want %s", unrankedPinKeySQL, got)
	}
}

func TestSetSessionPinnedAppendsKeepsAndClearsRank(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
	}
	for i, id := range []string{"a", "b", "c"} {
		state, err := store.SetSessionPinned(ctx, id, true)
		if err != nil {
			t.Fatalf("pin %s: %v", id, err)
		}
		if !state.Pinned || !state.Changed || state.PinOrder != int64(i+1) {
			t.Fatalf("pin %s state = %+v, want appended rank %d", id, state, i+1)
		}
	}

	again, err := store.SetSessionPinned(ctx, "a", true)
	if err != nil || again.Changed || again.PinOrder != 1 {
		t.Fatalf("re-pin a = %+v, %v; want unchanged rank 1", again, err)
	}

	unpinned, err := store.SetSessionPinned(ctx, "b", false)
	if err != nil || !unpinned.Changed || unpinned.Pinned || unpinned.PinOrder != 0 {
		t.Fatalf("unpin b = %+v, %v", unpinned, err)
	}
	loaded, err := store.Get(ctx, "b")
	if err != nil || loaded.Pinned || loaded.PinOrder != 0 {
		t.Fatalf("unpinned b = %+v, %v", loaded, err)
	}
	if _, ok := pinRanks(t, store.db)["b"]; ok {
		t.Fatal("unpin left a stored rank")
	}

	repinned, err := store.SetSessionPinned(ctx, "b", true)
	if err != nil || repinned.PinOrder != 4 {
		t.Fatalf("re-pin b = %+v, %v; want appended rank 4", repinned, err)
	}
	if got, want := listPinTestIDs(t, store, ListOptions{SortByActivity: true}), []string{"a", "c", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("pinned order = %v, want %v", got, want)
	}

	if _, err := store.SetSessionPinned(ctx, "missing", true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pin missing session error = %v, want ErrNotFound", err)
	}
}

func TestCreatePinnedSessionAppendsAfterExistingPins(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	createPinTestSession(t, store, "existing", time.Now().Add(-time.Hour))
	pinTestSessions(t, store, "existing")

	created := &Session{ID: "created-pinned", Provider: "mock", Model: "mock-model", Pinned: true}
	if err := store.Create(ctx, created); err != nil {
		t.Fatal(err)
	}
	if created.PinOrder != 2 {
		t.Fatalf("created pin rank = %d, want 2", created.PinOrder)
	}
	loaded, err := store.Get(ctx, created.ID)
	if err != nil || !loaded.Pinned || loaded.PinOrder != 2 {
		t.Fatalf("loaded created pin = %+v, %v", loaded, err)
	}
	unpinned := &Session{ID: "created-unpinned", Provider: "mock", Model: "mock-model"}
	if err := store.Create(ctx, unpinned); err != nil || unpinned.PinOrder != 0 {
		t.Fatalf("unpinned create rank = %d, %v", unpinned.PinOrder, err)
	}
}

func TestListKeepsPinnedOrderWhenActivityChanges(t *testing.T) {
	store := newProjectTestStore(t)
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c", "d", "e"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
	}
	// Pin the older conversation second: explicit order, not activity, wins.
	pinTestSessions(t, store, "c", "a")

	for _, opts := range []ListOptions{{SortByActivity: true}, {}} {
		if got, want := listPinTestIDs(t, store, opts), []string{"c", "a", "e", "d", "b"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("initial order (activity=%t) = %v, want %v", opts.SortByActivity, got, want)
		}
	}

	touchPinTestSession(t, store, "a", base.Add(time.Hour))
	touchPinTestSession(t, store, "b", base.Add(2*time.Hour))
	if got, want := listPinTestIDs(t, store, ListOptions{SortByActivity: true}), []string{"c", "a", "b", "e", "d"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order after new messages = %v, want pins unchanged and b first among the rest: %v", got, want)
	}
	if got := listPinTestIDs(t, store, ListOptions{}); !reflect.DeepEqual(got[:2], []string{"c", "a"}) {
		t.Fatalf("last-user order after new messages = %v, want pins c, a first", got)
	}
}

func TestUpdateSnapshotDoesNotChangePinState(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	createPinTestSession(t, store, "a", base)
	createPinTestSession(t, store, "b", base.Add(time.Minute))

	staleUnpinned, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	pinTestSessions(t, store, "a", "b")
	if _, err := store.ReorderPinnedSessions(ctx, []string{"b", "a"}); err != nil {
		t.Fatal(err)
	}

	// A long-running runtime persists its older snapshot.
	staleUnpinned.Name = "renamed by runtime"
	if err := store.Update(ctx, staleUnpinned); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Name != "renamed by runtime" || !loaded.Pinned || loaded.PinOrder != 2 {
		t.Fatalf("after stale update a = name %q pinned %t rank %d; want rename, pinned, rank 2", loaded.Name, loaded.Pinned, loaded.PinOrder)
	}

	stalePinned, err := store.Get(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetSessionPinned(ctx, "b", false); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(ctx, stalePinned); err != nil {
		t.Fatal(err)
	}
	loaded, err = store.Get(ctx, "b")
	if err != nil || loaded.Pinned || loaded.PinOrder != 0 {
		t.Fatalf("stale pinned snapshot re-pinned b: %+v, %v", loaded, err)
	}
}

func TestReorderPinnedSessionsPermutesOnlyListedSlots(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c", "hidden"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
	}
	pinTestSessions(t, store, "a", "b", "c", "hidden")
	archived, err := store.Get(ctx, "hidden")
	if err != nil {
		t.Fatal(err)
	}
	archived.Archived = true
	if err := store.Update(ctx, archived); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}

	// The caller only sees a, b, and c; the archived pin keeps its slot.
	order, err := store.ReorderPinnedSessions(ctx, []string{"c", "a"})
	if err != nil {
		t.Fatal(err)
	}
	wantPositions := []PinnedPosition{{ID: "c", PinOrder: 1}, {ID: "b", PinOrder: 2}, {ID: "a", PinOrder: 3}, {ID: "hidden", PinOrder: 4}}
	if !reflect.DeepEqual(order.Positions, wantPositions) || !reflect.DeepEqual(order.Changed, []string{"c", "a"}) {
		t.Fatalf("reorder = %+v, want positions %+v changed [c a]", order, wantPositions)
	}
	if got, want := listPinTestIDs(t, store, ListOptions{SortByActivity: true, Archived: true}), []string{"c", "b", "a", "hidden"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listed order = %v, want %v", got, want)
	}

	again, err := store.ReorderPinnedSessions(ctx, []string{"c", "a"})
	if err != nil || len(again.Changed) != 0 || !reflect.DeepEqual(again.Positions, wantPositions) {
		t.Fatalf("repeated reorder = %+v, %v; want idempotent", again, err)
	}
	after, err := store.Get(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Fatalf("reorder changed activity time: %v -> %v", before.UpdatedAt, after.UpdatedAt)
	}

	if _, err := store.ReorderPinnedSessions(ctx, []string{"hidden", "c", "b", "a"}); err != nil {
		t.Fatal(err)
	}
	if got, want := pinRanks(t, store.db), map[string]int64{"hidden": 1, "c": 2, "b": 3, "a": 4}; !reflect.DeepEqual(got, want) {
		t.Fatalf("full reorder ranks = %v, want %v", got, want)
	}
}

func TestReorderPinnedSessionsRejectsInvalidRequestsWithoutWriting(t *testing.T) {
	store := newProjectTestStore(t)
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "unpinned"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
	}
	pinTestSessions(t, store, "a", "b")
	before := pinRanks(t, store.db)

	for _, tc := range []struct {
		name string
		ids  []string
		want error
	}{
		{name: "unknown", ids: []string{"b", "missing", "a"}, want: ErrNotFound},
		{name: "unpinned", ids: []string{"b", "unpinned", "a"}, want: ErrPinnedOrderConflict},
		{name: "duplicate", ids: []string{"b", "a", "b"}, want: ErrPinnedOrderInvalid},
		{name: "blank", ids: []string{"b", " "}, want: ErrPinnedOrderInvalid},
		{name: "empty", ids: nil, want: ErrPinnedOrderInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ReorderPinnedSessions(context.Background(), tc.ids); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := pinRanks(t, store.db); !reflect.DeepEqual(got, before) {
				t.Fatalf("ranks changed to %v, want %v", got, before)
			}
		})
	}
}

func TestReorderPinnedSessionsPublishesMetadataChanges(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"a", "b", "c"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
	}
	pinTestSessions(t, store, "a", "b", "c")
	cursor, err := store.StoreChangeCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReorderPinnedSessions(ctx, []string{"b", "a"}); err != nil {
		t.Fatal(err)
	}
	changes, err := store.ListStoreChanges(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	changed := map[string]bool{}
	for _, change := range changes {
		if change.Kind == StoreChangeSessionMetadataChanged {
			changed[change.SessionID] = true
		}
	}
	if !reflect.DeepEqual(changed, map[string]bool{"a": true, "b": true}) {
		t.Fatalf("metadata changes = %v, want a and b for other processes", changed)
	}
}

func TestListCursorPagesThroughPinnedRanks(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"p0", "u0", "p1", "u1", "p2", "u2", "p3", "u3", "p4"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
	}
	// Rank order deliberately disagrees with activity order.
	pinTestSessions(t, store, "p3", "p0", "p4", "p1", "p2")
	want := []string{"p3", "p0", "p4", "p1", "p2", "u3", "u2", "u1", "u0"}
	if got := listPinTestIDs(t, store, ListOptions{SortByActivity: true}); !reflect.DeepEqual(got, want) {
		t.Fatalf("full order = %v, want %v", got, want)
	}

	var cursor *ProjectSessionCursor
	var got []string
	for page := 0; page < len(want); page++ {
		summaries, err := store.List(ctx, ListOptions{Limit: 2, SortByActivity: true, ProjectCursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, summaryIDs(summaries)...)
		if len(summaries) < 2 {
			break
		}
		decoded, err := DecodeProjectSessionCursor(EncodeRecentSessionCursor(summaries[len(summaries)-1]))
		if err != nil {
			t.Fatal(err)
		}
		cursor = &decoded
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("paged order = %v, want %v", got, want)
	}

	// A cursor minted before ranks existed restarts the pinned block instead
	// of skipping rows; clients merge pages by ID.
	legacy, err := DecodeProjectSessionCursor(EncodeRecentSessionCursor(SessionSummary{Pinned: true, PinOrder: 2, Number: 99, CreatedAt: base}))
	if err != nil {
		t.Fatal(err)
	}
	legacy.PinOrder = 0
	summaries, err := store.List(ctx, ListOptions{Limit: -1, SortByActivity: true, ProjectCursor: &legacy})
	if err != nil {
		t.Fatal(err)
	}
	if gotLegacy := summaryIDs(summaries); !reflect.DeepEqual(gotLegacy, want) {
		t.Fatalf("legacy cursor page = %v, want every row %v", gotLegacy, want)
	}
}

func TestSidebarOrdersProjectPinsByRankAndContinuesCursor(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	alpha := &Project{Name: "Alpha", CanonicalDir: filepath.Join(t.TempDir(), "alpha")}
	if err := store.CreateProject(ctx, alpha); err != nil {
		t.Fatal(err)
	}
	base := time.Now().Add(-time.Hour)
	for i, id := range []string{"s0", "s1", "s2", "s3"} {
		createPinTestSession(t, store, id, base.Add(time.Duration(i)*time.Minute))
		if _, err := store.BindSessionWorkspace(ctx, id, SessionWorkspaceBinding{ProjectID: alpha.ID, CWD: alpha.CanonicalDir}); err != nil {
			t.Fatal(err)
		}
	}
	pinTestSessions(t, store, "s0", "s2")
	if _, err := store.ReorderPinnedSessions(ctx, []string{"s2", "s0"}); err != nil {
		t.Fatal(err)
	}
	touchPinTestSession(t, store, "s0", base.Add(time.Hour))

	groups, err := store.Sidebar(ctx, SidebarOptions{PerProject: 1, IncludeArchivedProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	var group *SidebarGroup
	for i := range groups {
		if groups[i].Project != nil && groups[i].Project.ID == alpha.ID {
			group = &groups[i]
		}
	}
	if group == nil || !reflect.DeepEqual(summaryIDs(group.Sessions), []string{"s2"}) || group.NextCursor == "" {
		t.Fatalf("alpha group = %+v, want first pin s2 with a cursor", group)
	}
	if group.Sessions[0].PinOrder != 1 {
		t.Fatalf("sidebar pin rank = %d, want 1", group.Sessions[0].PinOrder)
	}
	cursor, err := DecodeProjectSessionCursor(group.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	rest, err := store.List(ctx, ListOptions{Limit: -1, SortByActivity: true, ProjectID: alpha.ID, ProjectCursor: &cursor})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := summaryIDs(rest), []string{"s0", "s3", "s1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remaining project page = %v, want %v", got, want)
	}
}

// failMigration makes the given migration do its real work and then fail,
// leaving the database genuinely at the previous version. The returned func
// restores migrations.
func failMigration(t *testing.T, version int) func() {
	t.Helper()
	original := migrations
	migrations = append([]migration(nil), original...)
	for i := range migrations {
		if migrations[i].version != version {
			continue
		}
		realUp := migrations[i].up
		migrations[i].up = func(db schemaExecutor) error {
			if err := realUp(db); err != nil {
				return err
			}
			return fmt.Errorf("injected migration %d failure", version)
		}
		return func() { migrations = original }
	}
	migrations = original
	t.Fatalf("migration %d not found", version)
	return nil
}

func failMigration60(t *testing.T) func() {
	t.Helper()
	return failMigration(t, 60)
}

func TestSessionMigration60BackfillsPinnedOrderFromActivityAndRetries(t *testing.T) {
	db := openProjectMigration46DB(t)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := db.Exec(`
		INSERT INTO sessions (id, number, provider, model, created_at, last_message_at, pinned, archived) VALUES
			('old-pin', 10, 'mock', 'mock', ?, ?, TRUE, FALSE),
			('new-pin', 11, 'mock', 'mock', ?, ?, TRUE, FALSE),
			('archived-pin', 12, 'mock', 'mock', ?, ?, TRUE, TRUE),
			('tie-low', 13, 'mock', 'mock', ?, ?, TRUE, FALSE),
			('tie-high', 14, 'mock', 'mock', ?, ?, TRUE, FALSE),
			('unpinned', 15, 'mock', 'mock', ?, ?, FALSE, FALSE)`,
		base, base,
		base, base.Add(3*time.Hour),
		base, base.Add(2*time.Hour),
		base, base.Add(time.Hour),
		base, base.Add(time.Hour),
		base, base.Add(4*time.Hour),
	); err != nil {
		t.Fatalf("seed historical pins: %v", err)
	}

	restore := failMigration60(t)
	err := initSchema(db)
	restore()
	if err == nil || !strings.Contains(err.Error(), "injected migration 60 failure") {
		t.Fatalf("failing migration error = %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 59 {
		t.Fatalf("marker after failed migration 60 = %d, %v; want 59", version, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "sessions", "pin_order"); err != nil || exists {
		t.Fatalf("pin_order after rollback exists=%t err=%v", exists, err)
	}
	if exists, err := sqliteutil.IndexExists(db, "idx_sessions_sidebar_activity"); err != nil || !exists {
		t.Fatalf("v59 sidebar index after rollback exists=%t err=%v", exists, err)
	}

	if err := initSchema(db); err != nil {
		t.Fatalf("retry from version 59: %v", err)
	}
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != schemaVersion {
		t.Fatalf("marker after retry = %d, %v", version, err)
	}
	// Existing pins keep the order they were last shown in: newest activity
	// first, higher session number on ties. Unpinned rows get no rank.
	want := map[string]int64{"new-pin": 1, "archived-pin": 2, "tie-high": 3, "tie-low": 4, "old-pin": 5}
	if got := pinRanks(t, db); !reflect.DeepEqual(got, want) {
		t.Fatalf("backfilled ranks = %v, want %v", got, want)
	}
	var backfillEvents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM session_change_log WHERE kind = ?`, StoreChangeSessionMetadataChanged).Scan(&backfillEvents); err != nil || backfillEvents != 0 {
		t.Fatalf("backfill metadata change rows = %d, %v; want none", backfillEvents, err)
	}
	for _, index := range []string{"idx_sessions_sidebar_activity", "idx_sessions_sidebar_last_user_activity"} {
		if exists, err := sqliteutil.IndexExists(db, index); err != nil || exists {
			t.Fatalf("replaced index %s exists=%t err=%v", index, exists, err)
		}
	}

	fresh, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	fresh.SetMaxOpenConns(1)
	defer fresh.Close()
	if err := initSchema(fresh); err != nil {
		t.Fatal(err)
	}
	freshSignature, err := sqliteutil.SchemaSignature(context.Background(), fresh)
	if err != nil {
		t.Fatal(err)
	}
	migratedSignature, err := sqliteutil.SchemaSignature(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(freshSignature, migratedSignature) {
		t.Fatalf("fresh and v60-migrated schemas differ\n--- fresh ---\n%s\n--- migrated ---\n%s", strings.Join(freshSignature, "\n"), strings.Join(migratedSignature, "\n"))
	}
}

func TestReadOnlyPreV60StoreListsWithoutPinnedOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	seedProjectMigration46Schema(t, db)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`
		UPDATE sessions SET name = '', summary = '', created_at = ?;
		INSERT INTO sessions (id, number, name, summary, provider, model, created_at, last_message_at, pinned) VALUES
			('older-pin', 1, '', '', 'mock', 'mock', ?, ?, TRUE),
			('newer-pin', 2, '', '', 'mock', 'mock', ?, ?, TRUE),
			('regular', 3, '', '', 'mock', 'mock', ?, ?, FALSE)`,
		base.Add(96*time.Hour),
		base, base,
		base, base.Add(24*time.Hour),
		base, base.Add(48*time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	restore := failMigration60(t)
	err = initSchema(db)
	restore()
	if err == nil {
		t.Fatal("migration 60 unexpectedly succeeded")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := NewSQLiteStore(Config{Enabled: true, Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if readOnly.hasPinOrder {
		t.Fatal("read-only v59 store reported pin_order")
	}
	summaries, err := readOnly.List(ctx, ListOptions{Limit: -1, SortByActivity: true})
	if err != nil {
		t.Fatal(err)
	}
	// The fixture's legacy row has no messages, so its creation time is its
	// activity.
	if got, want := summaryIDs(summaries), []string{"newer-pin", "older-pin", "legacy-project-migration", "regular"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("read-only order = %v, want activity-ordered pins first %v", got, want)
	}
	decoded, err := DecodeProjectSessionCursor(EncodeRecentSessionCursor(summaries[0]))
	if err != nil {
		t.Fatal(err)
	}
	if next, err := readOnly.List(ctx, ListOptions{Limit: -1, SortByActivity: true, ProjectCursor: &decoded}); err != nil || len(next) != 3 {
		t.Fatalf("read-only cursor page = %v, %v", summaryIDs(next), err)
	}
	if loaded, err := readOnly.Get(ctx, "newer-pin"); err != nil || !loaded.Pinned || loaded.PinOrder != 0 {
		t.Fatalf("read-only Get = %+v, %v", loaded, err)
	}
	if _, ok := AsPinnedSessionStore(readOnly); ok {
		t.Fatal("read-only store advertised pinned ordering")
	}
	if _, err := SetSessionPinned(ctx, readOnly, "regular", true); !errors.Is(err, ErrPinnedOrderUnsupported) {
		t.Fatalf("read-only pin error = %v, want ErrPinnedOrderUnsupported", err)
	}
}
