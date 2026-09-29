package session

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/sqliteutil"
)

func createOrderTestProject(t *testing.T, store *SQLiteStore, name string) *Project {
	t.Helper()
	project := &Project{Name: name, CanonicalDir: filepath.Join(t.TempDir(), strings.ToLower(name))}
	if err := store.CreateProject(context.Background(), project); err != nil {
		t.Fatalf("CreateProject(%s): %v", name, err)
	}
	return project
}

// addProjectOrderTestSession records a conversation in project with activity at at.
func addProjectOrderTestSession(t *testing.T, store *SQLiteStore, id string, project *Project, at time.Time) {
	t.Helper()
	createPinTestSession(t, store, id, at)
	if _, err := store.BindSessionWorkspace(context.Background(), id, SessionWorkspaceBinding{ProjectID: project.ID, CWD: project.CanonicalDir}); err != nil {
		t.Fatalf("BindSessionWorkspace(%s): %v", id, err)
	}
}

func listedProjectNames(t *testing.T, store *SQLiteStore, includeArchived bool) []string {
	t.Helper()
	projects, err := store.ListProjects(context.Background(), ProjectListOptions{IncludeArchived: includeArchived})
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	names := make([]string, 0, len(projects))
	for _, project := range projects {
		names = append(names, project.Name)
	}
	return names
}

func sidebarProjectNames(t *testing.T, store *SQLiteStore, opts SidebarOptions) []string {
	t.Helper()
	groups, err := store.Sidebar(context.Background(), opts)
	if err != nil {
		t.Fatalf("Sidebar: %v", err)
	}
	names := make([]string, 0, len(groups))
	for _, group := range groups {
		if group.NoProject {
			names = append(names, "<no project>")
			continue
		}
		names = append(names, group.Project.Name)
	}
	return names
}

func projectRanks(t *testing.T, db *sql.DB) map[string]int64 {
	t.Helper()
	rows, err := db.Query(`SELECT name, sort_order FROM projects`)
	if err != nil {
		t.Fatalf("query project ranks: %v", err)
	}
	defer rows.Close()
	ranks := map[string]int64{}
	for rows.Next() {
		var name string
		var rank sql.NullInt64
		if err := rows.Scan(&name, &rank); err != nil {
			t.Fatalf("scan project rank: %v", err)
		}
		ranks[name] = rank.Int64
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate project ranks: %v", err)
	}
	return ranks
}

func setOrderTestProjectArchived(t *testing.T, store *SQLiteStore, project *Project, archived bool) {
	t.Helper()
	if _, err := store.UpdateProject(context.Background(), project.ID, ProjectUpdate{Archived: &archived}); err != nil {
		t.Fatalf("archive %s=%t: %v", project.Name, archived, err)
	}
}

func TestProjectOrderMigrationRetryPreservesCustomOrder(t *testing.T) {
	store := newProjectTestStore(t)
	alpha := createOrderTestProject(t, store, "Alpha")
	beta := createOrderTestProject(t, store, "Beta")
	if _, err := store.ReorderProjects(context.Background(), []string{beta.ID, alpha.ID}); err != nil {
		t.Fatal(err)
	}
	if err := migrateProjectOrderV61(store.db); err != nil {
		t.Fatalf("retry project order migration: %v", err)
	}
	if got, want := listedProjectNames(t, store, false), []string{"Beta", "Alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("order after retry = %v, want %v", got, want)
	}
}

func TestProjectsAppendAndKeepRanksThroughArchiveAndRestore(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	alpha := createOrderTestProject(t, store, "Alpha")
	beta := createOrderTestProject(t, store, "Beta")
	gamma := createOrderTestProject(t, store, "Gamma")
	for i, project := range []*Project{alpha, beta, gamma} {
		if project.SortOrder != int64(i+1) {
			t.Fatalf("%s rank = %d, want appended rank %d", project.Name, project.SortOrder, i+1)
		}
	}

	// Archived projects list after active ones but keep their ranks.
	setOrderTestProjectArchived(t, store, alpha, true)
	if got, want := listedProjectNames(t, store, true), []string{"Beta", "Gamma", "Alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("with archived = %v, want %v", got, want)
	}
	if got, want := listedProjectNames(t, store, false), []string{"Beta", "Gamma"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("without archived = %v, want %v", got, want)
	}
	// A new project appends after every rank, archived ones included.
	delta := createOrderTestProject(t, store, "Delta")
	if delta.SortOrder != 4 {
		t.Fatalf("Delta rank = %d, want 4", delta.SortOrder)
	}
	loaded, err := store.GetProject(ctx, alpha.ID)
	if err != nil || loaded.SortOrder != 1 {
		t.Fatalf("archived Alpha = %+v, %v; want rank 1", loaded, err)
	}

	// Restoring returns a project to its place.
	setOrderTestProjectArchived(t, store, alpha, false)
	if got, want := listedProjectNames(t, store, true), []string{"Alpha", "Beta", "Gamma", "Delta"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after restore = %v, want %v", got, want)
	}
	// So does re-adding an archived project's directory.
	setOrderTestProjectArchived(t, store, beta, true)
	restored := &Project{Name: "Beta again", CanonicalDir: beta.CanonicalDir}
	if err := store.CreateProject(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if restored.ID != beta.ID || restored.SortOrder != 2 || restored.Archived() {
		t.Fatalf("restored duplicate = %+v, want Beta's identity and rank 2", restored)
	}
	duplicate := &Project{Name: "Gamma", CanonicalDir: gamma.CanonicalDir}
	if err := store.CreateProject(ctx, duplicate); !errors.Is(err, ErrProjectDuplicate) || duplicate.SortOrder != 3 {
		t.Fatalf("duplicate create = %+v, %v; want ErrProjectDuplicate with rank 3", duplicate, err)
	}
}

func TestBootstrapProjectTakesFirstRank(t *testing.T) {
	store := newProjectTestStore(t)
	project := &Project{Name: "Home", CanonicalDir: filepath.Join(t.TempDir(), "home")}
	if err := store.BootstrapProject(context.Background(), project, nil); err != nil {
		t.Fatal(err)
	}
	if project.SortOrder != 1 {
		t.Fatalf("bootstrap rank = %d, want 1", project.SortOrder)
	}
	if next := createOrderTestProject(t, store, "Next"); next.SortOrder != 2 {
		t.Fatalf("next rank = %d, want 2", next.SortOrder)
	}
}

func TestSidebarKeepsProjectOrderWhenActivityChanges(t *testing.T) {
	store := newProjectTestStore(t)
	base := time.Now().Add(-time.Hour)
	alpha := createOrderTestProject(t, store, "Alpha")
	beta := createOrderTestProject(t, store, "Beta")
	gamma := createOrderTestProject(t, store, "Gamma")
	addProjectOrderTestSession(t, store, "alpha-chat", alpha, base)
	addProjectOrderTestSession(t, store, "beta-chat", beta, base.Add(time.Minute))
	addProjectOrderTestSession(t, store, "gamma-chat", gamma, base.Add(2*time.Minute))
	createPinTestSession(t, store, "loose-chat", base.Add(3*time.Minute))
	opts := SidebarOptions{PerProject: 12, IncludeArchivedProjects: true}
	want := []string{"Alpha", "Beta", "Gamma", "<no project>"}
	if got := sidebarProjectNames(t, store, opts); !reflect.DeepEqual(got, want) {
		t.Fatalf("initial sidebar = %v, want %v", got, want)
	}

	// New replies, including in an archived conversation that only an
	// archive-inclusive sidebar counts, never move a project.
	touchPinTestSession(t, store, "gamma-chat", base.Add(30*time.Minute))
	touchPinTestSession(t, store, "beta-chat", base.Add(40*time.Minute))
	archived, err := store.Get(context.Background(), "alpha-chat")
	if err != nil {
		t.Fatal(err)
	}
	archived.Archived = true
	if err := store.Update(context.Background(), archived); err != nil {
		t.Fatal(err)
	}
	addProjectOrderTestSession(t, store, "gamma-new", gamma, base.Add(50*time.Minute))
	for _, includeArchivedSessions := range []bool{false, true} {
		opts.IncludeArchivedSessions = includeArchivedSessions
		if got := sidebarProjectNames(t, store, opts); !reflect.DeepEqual(got, want) {
			t.Fatalf("sidebar after activity (archived sessions=%t) = %v, want %v", includeArchivedSessions, got, want)
		}
	}
	if got := listedProjectNames(t, store, true); !reflect.DeepEqual(got, want[:3]) {
		t.Fatalf("project list after activity = %v, want %v", got, want[:3])
	}

	if _, err := store.ReorderProjects(context.Background(), []string{gamma.ID, alpha.ID}); err != nil {
		t.Fatal(err)
	}
	want = []string{"Gamma", "Beta", "Alpha", "<no project>"}
	touchPinTestSession(t, store, "beta-chat", time.Now())
	if got := sidebarProjectNames(t, store, opts); !reflect.DeepEqual(got, want) {
		t.Fatalf("sidebar after reorder and activity = %v, want %v", got, want)
	}
}

func TestReorderProjectsPermutesListedSlotsAndIsIdempotent(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	alpha := createOrderTestProject(t, store, "Alpha")
	beta := createOrderTestProject(t, store, "Beta")
	hidden := createOrderTestProject(t, store, "Hidden")
	gamma := createOrderTestProject(t, store, "Gamma")
	setOrderTestProjectArchived(t, store, hidden, true)
	before, err := store.GetProject(ctx, alpha.ID)
	if err != nil {
		t.Fatal(err)
	}

	// The caller lists only its active projects; the archived one keeps its slot.
	order, err := store.ReorderProjects(ctx, []string{gamma.ID, beta.ID, alpha.ID})
	if err != nil {
		t.Fatal(err)
	}
	wantPositions := []ProjectPosition{{ID: gamma.ID, SortOrder: 1}, {ID: beta.ID, SortOrder: 2}, {ID: hidden.ID, SortOrder: 3}, {ID: alpha.ID, SortOrder: 4}}
	if !reflect.DeepEqual(order.Positions, wantPositions) || !reflect.DeepEqual(order.Changed, []string{gamma.ID, alpha.ID}) {
		t.Fatalf("reorder = %+v, want positions %+v and changed [gamma alpha]", order, wantPositions)
	}
	if got, want := listedProjectNames(t, store, true), []string{"Gamma", "Beta", "Alpha", "Hidden"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("listed = %v, want %v", got, want)
	}

	again, err := store.ReorderProjects(ctx, []string{gamma.ID, beta.ID, alpha.ID})
	if err != nil || len(again.Changed) != 0 || !reflect.DeepEqual(again.Positions, wantPositions) {
		t.Fatalf("repeated reorder = %+v, %v; want idempotent", again, err)
	}
	after, err := store.GetProject(ctx, alpha.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) || !after.LastUsedAt.Equal(before.LastUsedAt) {
		t.Fatalf("reorder changed project metadata times: %+v -> %+v", before, after)
	}

	// Restoring the archived project returns it between Beta and Alpha.
	setOrderTestProjectArchived(t, store, hidden, false)
	if got, want := listedProjectNames(t, store, true), []string{"Gamma", "Beta", "Hidden", "Alpha"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("after restore = %v, want %v", got, want)
	}
}

func TestReorderProjectsRejectsInvalidRequestsWithoutWriting(t *testing.T) {
	store := newProjectTestStore(t)
	alpha := createOrderTestProject(t, store, "Alpha")
	beta := createOrderTestProject(t, store, "Beta")
	before := projectRanks(t, store.db)
	for _, tc := range []struct {
		name string
		ids  []string
		want error
	}{
		{name: "unknown", ids: []string{beta.ID, "prj_missing", alpha.ID}, want: ErrNotFound},
		{name: "duplicate", ids: []string{beta.ID, alpha.ID, beta.ID}, want: ErrProjectOrderInvalid},
		{name: "blank", ids: []string{beta.ID, " "}, want: ErrProjectOrderInvalid},
		{name: "empty", ids: nil, want: ErrProjectOrderInvalid},
		{name: "oversized", ids: make([]string, MaxProjectOrderIDs+1), want: ErrProjectOrderInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := store.ReorderProjects(context.Background(), tc.ids); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if got := projectRanks(t, store.db); !reflect.DeepEqual(got, before) {
				t.Fatalf("ranks changed to %v, want %v", got, before)
			}
		})
	}
}

func TestReorderProjectsRecordsProjectUpdatesForOtherProcesses(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	alpha := createOrderTestProject(t, store, "Alpha")
	beta := createOrderTestProject(t, store, "Beta")
	createOrderTestProject(t, store, "Gamma")
	cursor, err := store.StoreChangeCursor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReorderProjects(ctx, []string{beta.ID, alpha.ID}); err != nil {
		t.Fatal(err)
	}
	changes, err := store.ListStoreChanges(ctx, cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	updated := map[string]bool{}
	for _, change := range changes {
		if change.Kind == StoreChangeProjectUpdated {
			updated[change.ProjectID] = true
		}
	}
	if !reflect.DeepEqual(updated, map[string]bool{alpha.ID: true, beta.ID: true}) {
		t.Fatalf("project updates = %v, want Alpha and Beta only", updated)
	}
}

// TestSessionMigration61BackfillsLegacySidebarOrder seeds a version 60
// database, records the project order the pre-rank sidebar showed, and checks
// that migration 61 freezes exactly that order.
func TestSessionMigration61BackfillsLegacySidebarOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sessions.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	seedProjectMigration46Schema(t, db)
	restore := failMigration(t, 61)
	err = initSchema(db)
	restore()
	if err == nil || !strings.Contains(err.Error(), "injected migration 61 failure") {
		t.Fatalf("failing migration error = %v", err)
	}
	var version int
	if err := db.QueryRow(`SELECT version FROM schema_version`).Scan(&version); err != nil || version != 60 {
		t.Fatalf("marker after failed migration 61 = %d, %v; want 60", version, err)
	}
	if exists, err := sqliteutil.ColumnExists(db, "projects", "sort_order"); err != nil || exists {
		t.Fatalf("sort_order after rollback exists=%t err=%v", exists, err)
	}

	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	at := func(hours int) time.Time { return base.Add(time.Duration(hours) * time.Hour) }
	if _, err := db.Exec(`
		INSERT INTO projects (id, name, canonical_dir, created_at, updated_at, last_used_at, archived_at) VALUES
			('prj_alpha', 'alpha', '/p/alpha', ?, ?, ?, NULL),
			('prj_able', 'Able', '/p/able', ?, ?, ?, NULL),
			('prj_beta', 'Beta', '/p/beta', ?, ?, ?, NULL),
			('prj_gamma', 'gamma', '/p/gamma', ?, ?, ?, NULL),
			('prj_delta', 'Delta', '/p/delta', ?, ?, ?, NULL),
			('prj_echo', 'Echo', '/p/echo', ?, ?, ?, ?),
			('prj_zulu', 'Zulu', '/p/zulu', ?, ?, ?, ?)`,
		at(0), at(0), at(20),
		at(0), at(0), at(0),
		at(0), at(0), at(0),
		at(0), at(0), at(30),
		at(0), at(0), at(0),
		at(0), at(0), at(0), at(1),
		at(0), at(0), at(40), at(1),
	); err != nil {
		t.Fatalf("seed projects: %v", err)
	}
	if _, err := db.Exec(`
		UPDATE sessions SET name = '', summary = '';
		INSERT INTO sessions (id, number, name, summary, provider, model, project_id, parent_id, archived, created_at, last_message_at) VALUES
			('alpha-chat', 10, '', '', 'mock', 'mock', 'prj_alpha', NULL, FALSE, ?, ?),
			('able-chat', 11, '', '', 'mock', 'mock', 'prj_able', NULL, FALSE, ?, ?),
			('beta-old', 12, '', '', 'mock', 'mock', 'prj_beta', NULL, FALSE, ?, ?),
			('beta-new', 13, '', '', 'mock', 'mock', 'prj_beta', NULL, FALSE, ?, NULL),
			('gamma-archived', 14, '', '', 'mock', 'mock', 'prj_gamma', NULL, TRUE, ?, ?),
			('gamma-parent', 15, '', '', 'mock', 'mock', NULL, NULL, FALSE, ?, ?),
			('gamma-child', 16, '', '', 'mock', 'mock', 'prj_gamma', 'gamma-parent', FALSE, ?, ?),
			('echo-chat', 17, '', '', 'mock', 'mock', 'prj_echo', NULL, FALSE, ?, ?)`,
		at(0), at(1),
		at(0), at(1),
		at(0), at(2),
		at(3), // No message yet: its creation is its activity.
		at(0), at(5),
		at(0), at(0),
		at(0), at(6),
		at(0), at(10),
	); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// The pre-rank sidebar, from a read-only store that cannot migrate: most
	// recent activity first (ties by name), projects without top-level
	// unarchived activity next, then archived projects.
	readOnly, err := NewSQLiteStore(Config{Enabled: true, Path: path, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if readOnly.hasProjectSortOrder {
		t.Fatal("read-only v60 store reported sort_order")
	}
	legacyOpts := SidebarOptions{PerProject: 12, IncludeArchivedProjects: true}
	legacy := sidebarProjectNames(t, readOnly, legacyOpts)
	want := []string{"Beta", "Able", "alpha", "Delta", "gamma", "Echo", "Zulu", "<no project>"}
	if !reflect.DeepEqual(legacy, want) {
		t.Fatalf("legacy sidebar = %v, want %v", legacy, want)
	}
	if listed, err := readOnly.ListProjects(ctx, ProjectListOptions{IncludeArchived: true}); err != nil || len(listed) != 7 || listed[0].SortOrder != 0 {
		t.Fatalf("read-only project list = %+v, %v", listed, err)
	}
	if _, err := readOnly.ReorderProjects(ctx, []string{"prj_beta"}); !errors.Is(err, ErrProjectsUnsupported) {
		t.Fatalf("read-only reorder error = %v, want ErrProjectsUnsupported", err)
	}
	if _, ok := AsProjectStore(readOnly); ok {
		t.Fatal("read-only store advertised project storage")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := NewSQLiteStore(Config{Enabled: true, Path: path})
	if err != nil {
		t.Fatalf("migrate from version 60: %v", err)
	}
	defer store.Close()
	if got := sidebarProjectNames(t, store, legacyOpts); !reflect.DeepEqual(got, legacy) {
		t.Fatalf("migrated sidebar = %v, want the legacy order %v", got, legacy)
	}
	wantRanks := map[string]int64{"Beta": 1, "Able": 2, "alpha": 3, "Delta": 4, "gamma": 5, "Echo": 6, "Zulu": 7}
	if got := projectRanks(t, store.db); !reflect.DeepEqual(got, wantRanks) {
		t.Fatalf("backfilled ranks = %v, want %v", got, wantRanks)
	}
	var backfillEvents int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM session_change_log WHERE kind = ?`, StoreChangeProjectUpdated).Scan(&backfillEvents); err != nil || backfillEvents != 0 {
		t.Fatalf("backfill project update rows = %d, %v; want none", backfillEvents, err)
	}
	// Once ranked, activity no longer reorders projects.
	if _, err := store.db.Exec(`UPDATE sessions SET last_message_at = ? WHERE id = 'alpha-chat'`, at(100)); err != nil {
		t.Fatal(err)
	}
	if got := sidebarProjectNames(t, store, legacyOpts); !reflect.DeepEqual(got, legacy) {
		t.Fatalf("sidebar after new activity = %v, want %v", got, legacy)
	}
}
