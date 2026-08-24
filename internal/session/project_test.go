package session

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/samsaffron/term-llm/internal/llm"
)

func newProjectTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := NewSQLiteStore(Config{Enabled: true, Path: filepath.Join(t.TempDir(), "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestProjectMigrationsAndCRUDStableIdentity(t *testing.T) {
	found47, found48 := false, false
	for _, migration := range migrations {
		found47 = found47 || migration.version == projectSchemaVersion
		found48 = found48 || migration.version == 48
	}
	if !found47 || !found48 {
		t.Fatalf("project migrations present = 47:%t 48:%t", found47, found48)
	}
	store := newProjectTestStore(t)
	ctx := context.Background()
	p := &Project{Name: "Alpha", CanonicalDir: filepath.Join(t.TempDir(), "alpha")}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	if len(p.ID) <= len("prj_") || p.ID[:4] != "prj_" {
		t.Fatalf("project ID = %q", p.ID)
	}
	duplicate := &Project{Name: "Duplicate", CanonicalDir: p.CanonicalDir}
	if err := store.CreateProject(ctx, duplicate); !errors.Is(err, ErrProjectDuplicate) {
		t.Fatalf("duplicate error = %v", err)
	}
	if duplicate.ID != p.ID {
		t.Fatalf("duplicate ID = %q, want %q", duplicate.ID, p.ID)
	}
	archive := true
	if _, err := store.UpdateProject(ctx, p.ID, ProjectUpdate{Archived: &archive}); err != nil {
		t.Fatal(err)
	}
	restored := &Project{Name: "Restored", CanonicalDir: p.CanonicalDir}
	if err := store.CreateProject(ctx, restored); err != nil {
		t.Fatal(err)
	}
	if restored.ID != p.ID || restored.Archived() {
		t.Fatalf("restored project = %#v", restored)
	}
	list, err := store.ListProjects(ctx, ProjectListOptions{})
	if err != nil || len(list) != 1 || list[0].Name != "Restored" {
		t.Fatalf("projects = %#v, %v", list, err)
	}
}

func openProjectMigration46DB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })

	preProjectSchema := strings.Replace(schema, "    project_id TEXT,\n", "", 1)
	if preProjectSchema == schema {
		t.Fatal("project_id column not found in current schema")
	}
	if _, err := db.Exec(preProjectSchema); err != nil {
		t.Fatalf("seed pre-project schema: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE schema_version (version INTEGER NOT NULL);
		INSERT INTO schema_version(version) VALUES (46);
		INSERT INTO sessions (id, provider, model) VALUES ('legacy-project-migration', 'mock', 'mock');
	`); err != nil {
		t.Fatalf("seed schema version: %v", err)
	}
	return db
}

func TestProjectMigration47UpgradesExistingDatabase(t *testing.T) {
	db := openProjectMigration46DB(t)
	if err := initSchema(db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	var version int
	if err := db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	var projectID sql.NullString
	if err := db.QueryRow("SELECT project_id FROM sessions WHERE id = 'legacy-project-migration'").Scan(&projectID); err != nil {
		t.Fatalf("read migrated session: %v", err)
	}
	if projectID.Valid {
		t.Fatalf("legacy session project_id = %q, want NULL", projectID.String)
	}
	var projectsTable int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'projects'").Scan(&projectsTable); err != nil {
		t.Fatal(err)
	}
	if projectsTable != 1 {
		t.Fatalf("projects table count = %d, want 1", projectsTable)
	}
}

func TestProjectMigration48RepairsVersion47WithoutProjectColumn(t *testing.T) {
	db := openProjectMigration46DB(t)
	if _, err := db.Exec("UPDATE schema_version SET version = ?", projectSchemaVersion); err != nil {
		t.Fatal(err)
	}

	if err := initSchema(db); err != nil {
		t.Fatalf("initSchema: %v", err)
	}

	var version int
	if err := db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version = %d, want %d", version, schemaVersion)
	}
	var projectID sql.NullString
	if err := db.QueryRow("SELECT project_id FROM sessions WHERE id = 'legacy-project-migration'").Scan(&projectID); err != nil {
		t.Fatalf("read repaired session: %v", err)
	}
	if projectID.Valid {
		t.Fatalf("legacy session project_id = %q, want NULL", projectID.String)
	}
	var projectsTable int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'projects'").Scan(&projectsTable); err != nil {
		t.Fatal(err)
	}
	if projectsTable != 1 {
		t.Fatalf("projects table count = %d, want 1", projectsTable)
	}
}

func TestProjectMigration47FailureLeavesNoPartialSchema(t *testing.T) {
	db := openProjectMigration46DB(t)
	originalMigrations := migrations
	migrations = append([]migration(nil), originalMigrations...)
	for i := range migrations {
		if migrations[i].version != projectSchemaVersion {
			continue
		}
		migrations[i].description = "injected project migration failure"
		migrations[i].up = func(db schemaExecutor) error {
			if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN project_id TEXT"); err != nil {
				return err
			}
			if _, err := db.Exec(projectsSchemaV47); err != nil {
				return err
			}
			return errors.New("injected failure")
		}
		break
	}
	t.Cleanup(func() { migrations = originalMigrations })

	if err := initSchema(db); err == nil {
		t.Fatal("initSchema succeeded; want injected migration failure")
	}
	var version int
	if err := db.QueryRow("SELECT version FROM schema_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 46 {
		t.Fatalf("schema version = %d, want 46", version)
	}
	var projectsTable int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'projects'").Scan(&projectsTable); err != nil {
		t.Fatal(err)
	}
	if projectsTable != 0 {
		t.Fatal("failed migration left projects table behind")
	}
	rows, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		if name == "project_id" {
			t.Fatal("failed migration left project_id column behind")
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestConcurrentDuplicateProjectCreateKeepsOneIdentity(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	dir := t.TempDir()
	start := make(chan struct{})
	projects := []*Project{{Name: "One", CanonicalDir: dir}, {Name: "Two", CanonicalDir: dir}}
	errs := make(chan error, 2)
	for _, project := range projects {
		project := project
		go func() { <-start; errs <- store.CreateProject(ctx, project) }()
	}
	close(start)
	err1, err2 := <-errs, <-errs
	if !((err1 == nil && errors.Is(err2, ErrProjectDuplicate)) || (err2 == nil && errors.Is(err1, ErrProjectDuplicate))) {
		t.Fatalf("create errors = %v, %v", err1, err2)
	}
	if projects[0].ID == "" || projects[0].ID != projects[1].ID {
		t.Fatalf("project IDs = %q, %q", projects[0].ID, projects[1].ID)
	}
	list, err := store.ListProjects(ctx, ProjectListOptions{IncludeArchived: true})
	if err != nil || len(list) != 1 {
		t.Fatalf("projects = %#v, %v", list, err)
	}
}

func TestSessionProjectRoundTripSearchAndConditionalBinding(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	p := &Project{Name: "Alpha", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	sess := createWorkspaceTestSession(t, store, "project-session")
	binding := SessionWorkspaceBinding{ProjectID: p.ID, CWD: p.CanonicalDir}
	bound, err := store.BindSessionWorkspace(ctx, sess.ID, binding)
	if err != nil {
		t.Fatal(err)
	}
	if bound.ProjectID != p.ID || bound.CWD != p.CanonicalDir || bound.ProjectName != p.Name {
		t.Fatalf("bound session = %#v", bound)
	}
	if _, err := store.BindSessionWorkspace(ctx, sess.ID, binding); err != nil {
		t.Fatalf("idempotent bind: %v", err)
	}
	if _, err := store.BindSessionWorkspace(ctx, sess.ID, SessionWorkspaceBinding{ProjectID: "prj_other", CWD: t.TempDir()}); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("conflicting bind = %v", err)
	}
	msg := NewMessage(sess.ID, llm.UserText("project needle"), 0)
	if err := store.AddMessage(ctx, sess.ID, msg); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List(ctx, ListOptions{ProjectID: p.ID})
	if err != nil || len(listed) != 1 || listed[0].ProjectID != p.ID || listed[0].ProjectName != p.Name {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
	searched, err := store.Search(ctx, SearchOptions{Query: "project needle", ProjectID: p.ID})
	if err != nil || len(searched) != 1 || searched[0].ProjectID != p.ID || searched[0].ProjectName != p.Name {
		t.Fatalf("searched = %#v, %v", searched, err)
	}
	legacy := &Session{ID: "legacy-assignment", Provider: "mock", Model: "mock", CWD: p.CanonicalDir, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: StatusComplete}
	if err := store.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignSessionProject(ctx, legacy.ID, p.ID, legacy.CWD, legacy.WorktreeDir); err != nil {
		t.Fatal(err)
	}
	assigned, err := store.Get(ctx, legacy.ID)
	if err != nil || assigned.ProjectID != p.ID || assigned.CWD != p.CanonicalDir || assigned.WorktreeDir != "" {
		t.Fatalf("assigned session = %#v, %v", assigned, err)
	}
	if err := store.AssignSessionProject(ctx, legacy.ID, "prj_other", legacy.CWD, legacy.WorktreeDir); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("second assignment = %v", err)
	}
	moved := &Session{ID: "assignment-moved", Provider: "mock", Model: "mock", CWD: p.CanonicalDir, CreatedAt: time.Now(), UpdatedAt: time.Now(), Status: StatusComplete}
	if err := store.Create(ctx, moved); err != nil {
		t.Fatal(err)
	}
	expectedCWD := moved.CWD
	moved.CWD = t.TempDir()
	if err := store.Update(ctx, moved); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignSessionProject(ctx, moved.ID, p.ID, expectedCWD, ""); !errors.Is(err, ErrWorkspaceConflict) {
		t.Fatalf("assignment after workspace change = %v", err)
	}
	unchanged, err := store.Get(ctx, moved.ID)
	if err != nil || unchanged.ProjectID != "" {
		t.Fatalf("raced assignment changed project: %#v, %v", unchanged, err)
	}
}

func TestStaleSessionUpdateCannotOverwriteProjectWorkspaceBinding(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	project := &Project{Name: "Immutable", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	stale := createWorkspaceTestSession(t, store, "stale-metadata-update")
	binding := SessionWorkspaceBinding{ProjectID: project.ID, CWD: project.CanonicalDir, WorktreeDir: filepath.Join(project.CanonicalDir, "managed")}
	if _, err := store.BindSessionWorkspace(ctx, stale.ID, binding); err != nil {
		t.Fatal(err)
	}
	stale.Name = "metadata update after binding"
	stale.CWD = ""
	stale.WorktreeDir = ""
	if err := store.Update(ctx, stale); err != nil {
		t.Fatal(err)
	}
	persisted, err := store.Get(ctx, stale.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ProjectID != project.ID || persisted.CWD != binding.CWD || persisted.WorktreeDir != binding.WorktreeDir || persisted.Name != stale.Name {
		t.Fatalf("stale update changed immutable binding: %#v", persisted)
	}
}

func TestConcurrentWorkspaceBindingFirstWriterWins(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	sess := createWorkspaceTestSession(t, store, "binding-race")
	start := make(chan struct{})
	type result struct {
		project string
		err     error
	}
	results := make(chan result, 2)
	for _, project := range []string{"prj_a", "prj_b"} {
		project := project
		go func() {
			<-start
			_, err := store.BindSessionWorkspace(ctx, sess.ID, SessionWorkspaceBinding{ProjectID: project, CWD: "/tmp/" + project})
			results <- result{project: project, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	successes, conflicts := 0, 0
	winner := ""
	for _, got := range []result{first, second} {
		switch {
		case got.err == nil:
			successes++
			winner = got.project
		case errors.Is(got.err, ErrWorkspaceConflict):
			conflicts++
		default:
			t.Fatalf("binding %s error = %v", got.project, got.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	persisted, err := store.Get(ctx, sess.ID)
	if err != nil || persisted.ProjectID != winner || persisted.CWD != "/tmp/"+winner {
		t.Fatalf("winner=%q persisted=%#v err=%v", winner, persisted, err)
	}
}

func TestProjectSidebarActivityUsesAllRowsBeyondPinnedWindow(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	alpha := &Project{Name: "Alpha", CanonicalDir: filepath.Join(t.TempDir(), "alpha")}
	beta := &Project{Name: "Beta", CanonicalDir: filepath.Join(t.TempDir(), "beta")}
	for _, project := range []*Project{alpha, beta} {
		if err := store.CreateProject(ctx, project); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-24 * time.Hour)
	for i := 0; i < 4; i++ {
		sess := createWorkspaceTestSession(t, store, fmt.Sprintf("alpha-pinned-%d", i))
		if _, err := store.BindSessionWorkspace(ctx, sess.ID, SessionWorkspaceBinding{ProjectID: alpha.ID, CWD: alpha.CanonicalDir}); err != nil {
			t.Fatal(err)
		}
		sess.Pinned = true
		if err := store.Update(ctx, sess); err != nil {
			t.Fatal(err)
		}
		message := NewMessage(sess.ID, llm.UserText("old pinned"), 0)
		message.CreatedAt = old.Add(time.Duration(i) * time.Minute)
		if err := store.AddMessage(ctx, sess.ID, message); err != nil {
			t.Fatal(err)
		}
	}
	latest := createWorkspaceTestSession(t, store, "alpha-latest-unpinned")
	if _, err := store.BindSessionWorkspace(ctx, latest.ID, SessionWorkspaceBinding{ProjectID: alpha.ID, CWD: alpha.CanonicalDir}); err != nil {
		t.Fatal(err)
	}
	latestMessage := NewMessage(latest.ID, llm.UserText("latest unpinned"), 0)
	latestMessage.CreatedAt = time.Now()
	if err := store.AddMessage(ctx, latest.ID, latestMessage); err != nil {
		t.Fatal(err)
	}
	betaSession := createWorkspaceTestSession(t, store, "beta-middle")
	if _, err := store.BindSessionWorkspace(ctx, betaSession.ID, SessionWorkspaceBinding{ProjectID: beta.ID, CWD: beta.CanonicalDir}); err != nil {
		t.Fatal(err)
	}
	betaMessage := NewMessage(betaSession.ID, llm.UserText("middle"), 0)
	betaMessage.CreatedAt = time.Now().Add(-time.Hour)
	if err := store.AddMessage(ctx, betaSession.ID, betaMessage); err != nil {
		t.Fatal(err)
	}

	groups, err := store.Sidebar(ctx, SidebarOptions{PerProject: 2, IncludeArchivedProjects: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) < 2 || groups[0].Project == nil || groups[0].Project.ID != alpha.ID {
		t.Fatalf("group order ignored latest unpinned activity: %#v", groups)
	}
	if !groups[0].LastActivity.After(groups[1].LastActivity) || !groups[0].LastActivity.Equal(latestMessage.CreatedAt) {
		t.Fatalf("group activity alpha=%v beta=%v latest=%v", groups[0].LastActivity, groups[1].LastActivity, latestMessage.CreatedAt)
	}
}

func TestProjectSidebarBoundedGroupsAndIndependentCursor(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	alpha := &Project{Name: "Alpha", CanonicalDir: filepath.Join(t.TempDir(), "alpha")}
	beta := &Project{Name: "Beta", CanonicalDir: filepath.Join(t.TempDir(), "beta")}
	for _, p := range []*Project{alpha, beta} {
		if err := store.CreateProject(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 4; i++ {
		s := createWorkspaceTestSession(t, store, fmt.Sprintf("alpha-%d", i))
		if _, err := store.BindSessionWorkspace(ctx, s.ID, SessionWorkspaceBinding{ProjectID: alpha.ID, CWD: alpha.CanonicalDir}); err != nil {
			t.Fatal(err)
		}
		msg := NewMessage(s.ID, llm.UserText(fmt.Sprintf("turn %d", i)), 0)
		msg.CreatedAt = time.Now().Add(time.Duration(i) * time.Second)
		if err := store.AddMessage(ctx, s.ID, msg); err != nil {
			t.Fatal(err)
		}
	}
	spawnChild := createWorkspaceTestSession(t, store, "spawn-child")
	spawnChild.ParentID = "alpha-0"
	spawnChild.IsSubagent = true
	if err := store.Update(ctx, spawnChild); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindSessionWorkspace(ctx, spawnChild.ID, SessionWorkspaceBinding{ProjectID: alpha.ID, CWD: alpha.CanonicalDir}); err != nil {
		t.Fatal(err)
	}
	spawnMessage := NewMessage(spawnChild.ID, llm.UserText("internal delegated review"), 0)
	spawnMessage.CreatedAt = time.Now().Add(time.Hour)
	if err := store.AddMessage(ctx, spawnChild.ID, spawnMessage); err != nil {
		t.Fatal(err)
	}
	if matches, err := store.Search(ctx, SearchOptions{Query: "internal delegated review", ExcludeSubagents: true}); err != nil || len(matches) != 0 {
		t.Fatalf("spawn_agent child leaked into sidebar search: %#v, %v", matches, err)
	}
	if summaries, err := store.List(ctx, ListOptions{Limit: 100, ExcludeSubagents: true}); err != nil {
		t.Fatal(err)
	} else {
		for _, summary := range summaries {
			if summary.ID == spawnChild.ID {
				t.Fatalf("spawn_agent child leaked into flat sidebar list: %#v", summaries)
			}
		}
	}
	createWorkspaceTestSession(t, store, "legacy-null")
	groups, err := store.Sidebar(ctx, SidebarOptions{PerProject: 2})
	if err != nil {
		t.Fatal(err)
	}
	var alphaGroup, betaGroup, nullGroup *SidebarGroup
	for i := range groups {
		group := &groups[i]
		switch {
		case group.Project != nil && group.Project.ID == alpha.ID:
			alphaGroup = group
		case group.Project != nil && group.Project.ID == beta.ID:
			betaGroup = group
		case group.NoProject:
			nullGroup = group
		}
	}
	if alphaGroup == nil || len(alphaGroup.Sessions) != 2 || alphaGroup.SessionCount != 4 || alphaGroup.NextCursor == "" {
		t.Fatalf("alpha group = %#v", alphaGroup)
	}
	for _, summary := range alphaGroup.Sessions {
		if summary.ID == spawnChild.ID {
			t.Fatalf("spawn_agent child leaked into project sidebar: %#v", alphaGroup)
		}
	}
	if betaGroup == nil || len(betaGroup.Sessions) != 0 {
		t.Fatalf("empty beta group = %#v", betaGroup)
	}
	if nullGroup == nil || nullGroup.SessionCount != 1 {
		t.Fatalf("null group = %#v", nullGroup)
	}
	cursor, _ := DecodeProjectSessionCursor(alphaGroup.NextCursor)
	// A newer row arriving before the cursor must not shift the second page or
	// duplicate a row from the first page.
	newest := createWorkspaceTestSession(t, store, "alpha-newest")
	if _, err := store.BindSessionWorkspace(ctx, newest.ID, SessionWorkspaceBinding{ProjectID: alpha.ID, CWD: alpha.CanonicalDir}); err != nil {
		t.Fatal(err)
	}
	newestMessage := NewMessage(newest.ID, llm.UserText("new after first page"), 0)
	newestMessage.CreatedAt = time.Now().Add(time.Hour)
	if err := store.AddMessage(ctx, newest.ID, newestMessage); err != nil {
		t.Fatal(err)
	}
	page, err := store.List(ctx, ListOptions{ProjectID: alpha.ID, ProjectCursor: &cursor, Limit: 2, SortByActivity: true, ExcludeSubagents: true})
	if err != nil || len(page) != 2 {
		all, _ := store.List(ctx, ListOptions{ProjectID: alpha.ID, Limit: 10, SortByActivity: true})
		t.Fatalf("second page = %#v, %v; cursor=%#v all=%#v", page, err, cursor, all)
	}
	seen := map[string]bool{}
	for _, summary := range alphaGroup.Sessions {
		seen[summary.ID] = true
	}
	for _, summary := range page {
		if seen[summary.ID] {
			t.Fatalf("duplicate cursor row %s", summary.ID)
		}
	}
}

func TestProjectSidebarCountsRespectArchivedSessionFilter(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	project := &Project{Name: "Archived conversations", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	sess := createWorkspaceTestSession(t, store, "archived-project-session")
	if _, err := store.BindSessionWorkspace(ctx, sess.ID, SessionWorkspaceBinding{ProjectID: project.ID, CWD: project.CanonicalDir}); err != nil {
		t.Fatal(err)
	}
	sess.Archived = true
	if err := store.Update(ctx, sess); err != nil {
		t.Fatal(err)
	}
	groups, err := store.Sidebar(ctx, SidebarOptions{PerProject: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].SessionCount != 0 || len(groups[0].Sessions) != 0 {
		t.Fatalf("filtered sidebar groups = %#v", groups)
	}
	groups, err = store.Sidebar(ctx, SidebarOptions{PerProject: 2, IncludeArchivedSessions: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].SessionCount != 1 || len(groups[0].Sessions) != 1 {
		t.Fatalf("archived-inclusive sidebar groups = %#v", groups)
	}
}

func TestClaimProjectSessionsIsIdempotentAndSnapshotGuarded(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	project := &Project{Name: "Claim", CanonicalDir: t.TempDir()}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	matching := createWorkspaceTestSession(t, store, "claim-matching")
	moved := createWorkspaceTestSession(t, store, "claim-moved")
	staleMoved := ProjectSessionMatch{ID: moved.ID, CWD: moved.CWD, WorktreeDir: moved.WorktreeDir}
	moved.CWD = t.TempDir()
	if err := store.Update(ctx, moved); err != nil {
		t.Fatal(err)
	}

	claimed, err := store.ClaimProjectSessions(ctx, project.ID, []ProjectSessionMatch{
		{ID: matching.ID, CWD: matching.CWD, WorktreeDir: matching.WorktreeDir},
		staleMoved,
	})
	if err != nil || claimed != 1 {
		t.Fatalf("first claim = %d, %v; want 1", claimed, err)
	}
	claimed, err = store.ClaimProjectSessions(ctx, project.ID, []ProjectSessionMatch{{ID: matching.ID, CWD: matching.CWD, WorktreeDir: matching.WorktreeDir}})
	if err != nil || claimed != 0 {
		t.Fatalf("second claim = %d, %v; want 0", claimed, err)
	}
	loadedMatching, _ := store.Get(ctx, matching.ID)
	loadedMoved, _ := store.Get(ctx, moved.ID)
	if loadedMatching.ProjectID != project.ID || loadedMoved.ProjectID != "" {
		t.Fatalf("claimed sessions = matching %#v, moved %#v", loadedMatching, loadedMoved)
	}
}

func TestBootstrapProjectIsAtomicAndIdempotent(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	legacy := createWorkspaceTestSession(t, store, "legacy-bootstrap")
	p := &Project{Name: "Bootstrap", CanonicalDir: t.TempDir()}
	if err := store.BootstrapProject(ctx, p, []ProjectSessionMatch{{ID: legacy.ID, CWD: legacy.CWD, WorktreeDir: legacy.WorktreeDir}}); err != nil {
		t.Fatal(err)
	}
	firstID := p.ID
	loaded, err := store.Get(ctx, legacy.ID)
	if err != nil || loaded.ProjectID != firstID {
		t.Fatalf("backfilled session = %#v, %v", loaded, err)
	}
	repeated := &Project{Name: "Other", CanonicalDir: t.TempDir()}
	if err := store.BootstrapProject(ctx, repeated, nil); err != nil {
		t.Fatal(err)
	}
	if repeated.ID != firstID {
		t.Fatalf("repeated bootstrap ID = %q, want %q", repeated.ID, firstID)
	}
	projects, err := store.ListProjects(ctx, ProjectListOptions{IncludeArchived: true})
	if err != nil || len(projects) != 1 {
		t.Fatalf("projects = %#v, %v", projects, err)
	}
}

func TestBootstrapProjectSkipsSessionWhoseWorkspaceChanged(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	legacy := createWorkspaceTestSession(t, store, "bootstrap-workspace-race")
	legacy.CWD = t.TempDir()
	if err := store.Update(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	match := ProjectSessionMatch{ID: legacy.ID, CWD: legacy.CWD, WorktreeDir: legacy.WorktreeDir}
	legacy.CWD = t.TempDir()
	if err := store.Update(ctx, legacy); err != nil {
		t.Fatal(err)
	}

	project := &Project{Name: "Bootstrap", CanonicalDir: match.CWD}
	if err := store.BootstrapProject(ctx, project, []ProjectSessionMatch{match}); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Get(ctx, legacy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ProjectID != "" {
		t.Fatalf("changed workspace was claimed by bootstrap project: %#v", loaded)
	}
}
