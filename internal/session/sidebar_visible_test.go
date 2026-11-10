package session

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// createSidebarVisibilityFixture stores sessions newest-first in the given
// order so activity sorting is deterministic.
func createSidebarVisibilityFixture(t *testing.T, store *SQLiteStore, rows []struct {
	id     string
	mode   SessionMode
	origin SessionOrigin
}) {
	t.Helper()
	base := time.Now()
	for i, row := range rows {
		at := base.Add(-time.Duration(i) * time.Minute)
		sess := &Session{ID: row.id, Provider: "mock", ProviderKey: "mock", Model: "mock-model", Mode: row.mode, Origin: row.origin, CreatedAt: at, UpdatedAt: at, Status: StatusActive}
		if err := store.Create(context.Background(), sess); err != nil {
			t.Fatalf("Create(%s): %v", row.id, err)
		}
	}
}

func TestListSidebarVisibleFillsPagesWithRenderedSessions(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	createSidebarVisibilityFixture(t, store, []struct {
		id     string
		mode   SessionMode
		origin SessionOrigin
	}{
		{"ask-1", ModeAsk, OriginTUI},
		{"ask-2", ModeAsk, OriginTUI},
		{"web-chat", ModeChat, OriginWeb},
		{"plan-1", ModePlan, OriginTUI},
		{"exec-1", ModeExec, OriginTUI},
		{"tui-chat", ModeChat, OriginTUI},
		{"ask-3", ModeAsk, OriginTUI},
		{"telegram-chat", ModeChat, OriginTelegram},
		{"web-ask", ModeAsk, OriginWeb},
	})

	all, err := store.List(ctx, ListOptions{Limit: 100, SortByActivity: true, SidebarVisible: true})
	if err != nil {
		t.Fatal(err)
	}
	// Mirrors the client rule: terminal sessions only as chats, every other
	// origin regardless of mode.
	if want := []string{"web-chat", "tui-chat", "telegram-chat", "web-ask"}; !reflect.DeepEqual(summaryIDs(all), want) {
		t.Fatalf("visible sessions = %v, want %v", summaryIDs(all), want)
	}

	page, err := store.List(ctx, ListOptions{Limit: 2, SortByActivity: true, SidebarVisible: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"web-chat", "tui-chat"}; !reflect.DeepEqual(summaryIDs(page), want) {
		t.Fatalf("first visible page = %v, want %v", summaryIDs(page), want)
	}

	unfiltered, err := store.List(ctx, ListOptions{Limit: 2, SortByActivity: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ask-1", "ask-2"}; !reflect.DeepEqual(summaryIDs(unfiltered), want) {
		t.Fatalf("unfiltered page = %v, want %v (filter must stay opt-in)", summaryIDs(unfiltered), want)
	}
}

func TestProjectSidebarVisibleCountsAndPagesOnlyRenderedSessions(t *testing.T) {
	store := newProjectTestStore(t)
	ctx := context.Background()
	project := &Project{Name: "Alpha", CanonicalDir: filepath.Join(t.TempDir(), "alpha")}
	if err := store.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	createSidebarVisibilityFixture(t, store, []struct {
		id     string
		mode   SessionMode
		origin SessionOrigin
	}{
		{"p-ask-1", ModeAsk, OriginTUI},
		{"p-chat-1", ModeChat, OriginWeb},
		{"p-ask-2", ModeAsk, OriginTUI},
		{"p-chat-2", ModeChat, OriginTUI},
		{"np-ask", ModeAsk, OriginTUI},
		{"np-chat", ModeChat, OriginWeb},
	})
	for _, id := range []string{"p-ask-1", "p-chat-1", "p-ask-2", "p-chat-2"} {
		if _, err := store.BindSessionWorkspace(ctx, id, SessionWorkspaceBinding{ProjectID: project.ID, CWD: project.CanonicalDir}); err != nil {
			t.Fatal(err)
		}
	}

	groups, err := store.Sidebar(ctx, SidebarOptions{PerProject: 1, SidebarVisible: true})
	if err != nil {
		t.Fatal(err)
	}
	var projectGroup, noProjectGroup *SidebarGroup
	for i := range groups {
		switch {
		case groups[i].Project != nil && groups[i].Project.ID == project.ID:
			projectGroup = &groups[i]
		case groups[i].NoProject:
			noProjectGroup = &groups[i]
		}
	}
	if projectGroup == nil || projectGroup.SessionCount != 2 || !reflect.DeepEqual(summaryIDs(projectGroup.Sessions), []string{"p-chat-1"}) || projectGroup.NextCursor == "" {
		t.Fatalf("project group = %#v", projectGroup)
	}
	if noProjectGroup == nil || noProjectGroup.SessionCount != 1 || !reflect.DeepEqual(summaryIDs(noProjectGroup.Sessions), []string{"np-chat"}) {
		t.Fatalf("no-project group = %#v", noProjectGroup)
	}

	cursor, err := DecodeProjectSessionCursor(projectGroup.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.List(ctx, ListOptions{ProjectID: project.ID, ProjectCursor: &cursor, Limit: 10, SortByActivity: true, SidebarVisible: true})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"p-chat-2"}; !reflect.DeepEqual(summaryIDs(next), want) {
		t.Fatalf("next project page = %v, want %v", summaryIDs(next), want)
	}
}
